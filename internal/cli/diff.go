package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/jmurray2011/cob/internal/manifest"
	"github.com/jmurray2011/cob/internal/output"
	"github.com/jmurray2011/cob/pkg/cob"
)

func newDiffCmd() *cobra.Command {
	var flagVersion string

	cmd := &cobra.Command{
		Use:   "diff <manifest>",
		Short: "Show how the manifest differs from a published version",
		Long: "Compares the manifest's sources against a published version and " +
			"reports added / removed / changed assets. No asset is downloaded. " +
			"Exits 1 when there is any drift (like `diff`), 0 when identical. " +
			"Run before `publish --force` to see exactly what would change.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDiff(cmd.Context(), args[0], flagVersion)
		},
	}
	cmd.Flags().StringVar(&flagVersion, "version", "", "Package version (required, or set COB_VERSION)")
	return cmd
}

func runDiff(ctx context.Context, manifestPath, versionFlag string) error {
	out := output.New(flagJSON)

	version, err := resolveVersion(versionFlag)
	if err != nil {
		return fail(out, "diff", cob.ExitError, "%s", err)
	}
	m, err := manifest.Load(manifestPath)
	if err != nil {
		return fail(out, "diff", cob.ExitError, "%s", err)
	}
	warnManifestOverrides(m, out)
	if err := m.ResolveVariables(version); err != nil {
		return fail(out, "diff", cob.ExitError, "%s", err)
	}

	coords := &cob.PackageCoordinates{
		Domain: m.Domain, Repository: m.Repository,
		Namespace: m.Namespace, Package: m.Package, Version: version,
	}

	client, err := cob.NewClient(ctx, cob.ClientOptions{Profile: flagProfile, Region: flagRegion})
	if err != nil {
		return fail(out, "diff", cob.ExitError, "%s", err)
	}
	sources, err := buildSources(m, client)
	if err != nil {
		return fail(out, "diff", cob.ExitError, "%s", err)
	}

	cmps, err := compareManifestToPublished(ctx, sources, cob.NewRegistry(client), coords)
	if err != nil {
		return fail(out, "diff", cob.ExitNotFound, "%s", err)
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
		case c.SrcSHA == "":
			unknown++
			ar.Method = "unknown"
			out.Plain("  ? %s  (no source checksum; can't compare without download)", c.Name)
		case c.SrcSHA != c.PubSHA:
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
		return &ExitError{Code: cob.ExitError}
	}
	return out.CommandResult(result)
}
