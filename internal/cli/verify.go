package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/jmurray2011/cob/internal/manifest"
	"github.com/jmurray2011/cob/internal/output"
	"github.com/jmurray2011/cob/pkg/cob"
)

func newVerifyCmd() *cobra.Command {
	var (
		flagVersion string
		flagDeep    bool
	)

	cmd := &cobra.Command{
		Use:   "verify <manifest>",
		Short: "Check a published version matches the manifest's sources",
		Long: "Compares each manifest source's SHA-256 against the published " +
			"version's assets. No mutation. Source hash precedence: a known " +
			"checksum (S3 SHA-256 / ca:// / local) → the version's recorded " +
			"cob-provenance.json → (with --deep) downloading and hashing the " +
			"source. Exits non-zero on any mismatch or missing asset. A good " +
			"CI gate for reproducible builds.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runVerify(cmd.Context(), args[0], flagVersion, flagDeep)
		},
	}
	cmd.Flags().StringVar(&flagVersion, "version", "", "Package version (required, or set COB_VERSION)")
	cmd.Flags().BoolVar(&flagDeep, "deep", false, "Download and hash sources lacking a checksum (no S3 writes)")
	return cmd
}

func runVerify(ctx context.Context, manifestPath, versionFlag string, deep bool) error {
	out := output.New(flagJSON)

	version, err := resolveVersion(versionFlag)
	if err != nil {
		return fail(out, "verify", cob.ExitError, "%s", err)
	}
	m, err := manifest.Load(manifestPath)
	if err != nil {
		return fail(out, "verify", cob.ExitError, "%s", err)
	}
	warnManifestOverrides(m, out)
	if err := m.ResolveVariables(version); err != nil {
		return fail(out, "verify", cob.ExitError, "%s", err)
	}

	coords := &cob.PackageCoordinates{
		Domain: m.Domain, Repository: m.Repository,
		Namespace: m.Namespace, Package: m.Package, Version: version,
	}

	client, err := cob.NewClient(ctx, cob.ClientOptions{Profile: flagProfile, Region: flagRegion})
	if err != nil {
		return fail(out, "verify", cob.ExitError, "%s", err)
	}
	sources, err := buildSources(m, client)
	if err != nil {
		return fail(out, "verify", cob.ExitError, "%s", err)
	}

	prov, _ := cob.FetchProvenance(ctx, client.CodeArtifact, coords)
	cmps, err := compareManifestToPublished(ctx, sources, cob.NewRegistry(client), coords, deep, prov)
	if err != nil {
		return fail(out, "verify", cob.ExitNotFound, "%s", err)
	}

	out.Header("Verifying %s/%s@%s against %s", m.Namespace, m.Package, version, manifestPath)

	result := &cob.CommandResult{
		Command:    "verify",
		Package:    fmt.Sprintf("%s/%s@%s", m.Namespace, m.Package, version),
		Repository: fmt.Sprintf("%s/%s", m.Domain, m.Repository),
		Status:     "ok",
	}

	var failures, unverified, extra int
	for _, c := range cmps {
		ar := cob.AssetResult{Name: c.Name, Source: c.Source, SHA256: c.SrcSHA}
		switch {
		case !c.InManifest && c.InPublished:
			extra++
			ar.Method = "extra"
			out.AssetSkipped(c.Name + " (published, not in manifest)")
		case c.Err != nil:
			failures++
			ar.SetError(c.Err)
			out.AssetFail(c.Name, c.Source, c.Err)
		case !c.InPublished:
			failures++
			ar.Method = "missing"
			out.AssetFail(c.Name, c.Source, fmt.Errorf("not published"))
		case c.OriginDrift:
			failures++
			ar.Method = "drift"
			out.AssetFail(c.Name, c.Source, fmt.Errorf("source object changed since publish (etag/version differs from provenance)"))
		case c.SrcSHA == "":
			unverified++
			ar.Method = "unverified"
			out.AssetSkipped(c.Name + " (no checksum/provenance; use --deep to hash)")
		case c.SrcSHA == c.PubSHA:
			ar.Method = "match(" + c.SrcFrom + ")"
			out.AssetOK(&ar, c.Source)
		default:
			failures++
			ar.Method = "mismatch"
			out.AssetFail(c.Name, c.Source, fmt.Errorf("SHA-256 mismatch: source %s != published %s",
				short(c.SrcSHA), short(c.PubSHA)))
		}
		result.Assets = append(result.Assets, ar)
	}

	if extra > 0 {
		out.Warn("%d published asset(s) not declared in the manifest", extra)
	}
	if unverified > 0 {
		out.Warn("%d source(s) unverified (no checksum or provenance; pass --deep to hash)", unverified)
	}

	if failures > 0 {
		result.Status = "error"
		result.Error = fmt.Sprintf("%d asset(s) failed verification", failures)
		out.Summary("FAILED: %d mismatched/missing.", failures)
		out.CommandResult(result)
		return &ExitError{Code: cob.ExitError}
	}
	out.Summary("OK: published version matches the manifest.")
	return out.CommandResult(result)
}

func short(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}
