package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/jmurray2011/cob/internal/cob"
	"github.com/jmurray2011/cob/internal/manifest"
)

func newDiffCmd() *cobra.Command {
	var (
		flagVersion string
		flagDeep    bool
	)

	cmd := &cobra.Command{
		Use:   "diff <manifest>",
		Short: "Show how the manifest differs from a published version",
		Long: "Compares the manifest's sources against a published version and " +
			"reports added / removed / changed assets. Source hash precedence: " +
			"known checksum → recorded cob-provenance.json → (with --deep) " +
			"download+hash. Exits 1 on any drift (like `diff`), 0 when " +
			"identical. Run before `publish --force` to see what would change.",
		Example: `  # show how a manifest differs from a published version
  cob diff ./my-package.yaml --version 2.1.0`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDiff(cmd.Context(), args[0], flagVersion, flagDeep)
		},
	}
	cmd.Flags().StringVar(&flagVersion, "version", "", "Package version (required, or set COB_VERSION)")
	cmd.Flags().BoolVar(&flagDeep, "deep", false, "Download and hash sources lacking a checksum (no S3 writes)")
	return cmd
}

func runDiff(ctx context.Context, manifestPath, versionFlag string, deep bool) error {
	out := newWriter(flagJSON)

	version, err := resolveVersion(versionFlag)
	if err != nil {
		return fail(out, "diff", cob.ExitError, "%s", err)
	}
	m, err := manifest.Load(manifestPath)
	if err != nil {
		return fail(out, "diff", cob.ExitError, "%s", err)
	}
	warnManifestOverrides(m, out)

	coords := &cob.PackageCoordinates{
		Domain: m.Domain, Repository: m.Repository,
		Namespace: m.Namespace, Package: m.Package, Version: version,
	}

	client, err := dialClient(ctx)
	if err != nil {
		return fail(out, "diff", cob.ExitError, "%s", err)
	}

	// Resolve @latest before expanding ${VERSION} — otherwise the manifest's
	// source URIs and the lookup would target a version literally "latest".
	registry := cob.NewRegistry(client)
	if err := resolveLatestIfNeeded(ctx, coords, registry, out); err != nil {
		return fail(out, "diff", codeFor(err), "%s", err)
	}
	version = coords.Version

	if err := m.ResolveVariables(version); err != nil {
		return fail(out, "diff", cob.ExitError, "%s", err)
	}

	sources, err := buildSources(m, client)
	if err != nil {
		return fail(out, "diff", cob.ExitError, "%s", err)
	}

	prov, perr := cob.FetchProvenance(ctx, client.CodeArtifact, coords)
	if perr != nil {
		out.Warn("could not read %s: %s", cob.ProvenanceFile, perr)
	}
	cmps, err := compareManifestToPublished(ctx, sources, registry, coords, deep, prov)
	if err != nil {
		return fail(out, "diff", codeFor(err), "%s", err)
	}

	out.Header("diff %s/%s@%s vs %s", m.Namespace, m.Package, version, manifestPath)

	result := &cob.CommandResult{
		Command:    "diff",
		Package:    fmt.Sprintf("%s/%s@%s", m.Namespace, m.Package, version),
		Repository: fmt.Sprintf("%s/%s", m.Domain, m.Repository),
		Status:     "ok",
	}

	var added, removed, changed, same, unknown, errs int
	for _, c := range cmps {
		ar := cob.AssetResult{Name: c.Name, Source: c.Source, SHA256: c.SrcSHA}
		switch {
		case c.Err != nil:
			errs++
			ar.SetError(c.Err)
			out.AssetFail(c.Name, c.Source, c.Err)
		case c.InManifest && !c.InPublished:
			added++
			ar.Method = "added"
			out.Plain("  + %s  (not published)", c.Name)
		case !c.InManifest && c.InPublished:
			removed++
			ar.Method = "removed"
			out.Plain("  - %s  (published, not in manifest)", c.Name)
		case c.OriginDrift:
			changed++
			ar.Method = "changed"
			out.Plain("  ~ %s  (S3 source changed since publish: etag/version differs)", c.Name)
		case c.SrcSHA == "":
			unknown++
			ar.Method = "unknown"
			out.Plain("  ? %s  (no source checksum; can't compare without download)", c.Name)
		case !strings.EqualFold(c.SrcSHA, c.PubSHA):
			// hex SHA-256 case-insensitive — avoid spurious drift.
			changed++
			ar.Method = "changed"
			out.Plain("  ~ %s  (%s -> %s)", c.Name, short(c.PubSHA), short(c.SrcSHA))
		default:
			same++
			ar.Method = "same"
		}
		result.Assets = append(result.Assets, ar)
	}

	drift := added + removed + changed
	out.Summary("%d added, %d removed, %d changed, %d same, %d unknown.",
		added, removed, changed, same, unknown)

	if errs > 0 {
		result.Status = "error"
		result.Error = fmt.Sprintf("%d source(s) could not be resolved", errs)
		out.CommandResult(result)
		return &ExitError{Code: cob.ExitError}
	}
	if drift > 0 {
		result.Status = "drift"
		result.Error = fmt.Sprintf("%d added, %d removed, %d changed", added, removed, changed)
		out.CommandResult(result)
		// Drift is a completed check that found a difference — distinct from
		// the errs path above, which is a check that could not run.
		return &ExitError{Code: cob.ExitMismatch}
	}
	return out.CommandResult(result)
}
