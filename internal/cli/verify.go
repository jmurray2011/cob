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
		Use:   "verify <manifest|coordinates>",
		Short: "Check a published version's integrity and show its provenance",
		Long: "With a manifest: compares each source's SHA-256 against the " +
			"published assets (precedence: known checksum → recorded S3 origin " +
			"→ cob-provenance.json → --deep download+hash).\n\n" +
			"With compact coordinates (no manifest): self-verifies the version " +
			"against its own recorded cob-provenance.json — every asset must " +
			"still hash to what was recorded — and prints the chain of evidence " +
			"(who published/promoted it, where each file came from). No " +
			"mutation; exits non-zero on any mismatch.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runVerify(cmd.Context(), args[0], flagVersion, flagDeep)
		},
	}
	cmd.Flags().StringVar(&flagVersion, "version", "", "Package version (required, or set COB_VERSION)")
	cmd.Flags().BoolVar(&flagDeep, "deep", false, "Manifest mode: download and hash sources lacking a checksum")
	return cmd
}

func runVerify(ctx context.Context, target, versionFlag string, deep bool) error {
	if isManifestPath(target) {
		return runVerifyManifest(ctx, target, versionFlag, deep)
	}
	return runVerifyCoords(ctx, target, versionFlag)
}

func runVerifyManifest(ctx context.Context, manifestPath, versionFlag string, deep bool) error {
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

	prov, perr := cob.FetchProvenance(ctx, client.CodeArtifact, coords)
	if perr != nil {
		// Don't silently downgrade to "unverified" — the user should know
		// the provenance fallback is unavailable.
		out.Warn("could not read %s: %s", cob.ProvenanceFile, perr)
	}
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

// runVerifyCoords self-verifies a published version against its own recorded
// cob-provenance.json — no manifest, no source access. Every recorded asset
// must still hash to what provenance recorded, and the chain of evidence is
// printed.
func runVerifyCoords(ctx context.Context, target, versionFlag string) error {
	out := output.New(flagJSON)

	coords, err := manifest.ParseCoordinates(target)
	if err != nil {
		return fail(out, "verify", cob.ExitError, "%s", err)
	}
	if coords.Namespace == "" || coords.Package == "" {
		return fail(out, "verify", cob.ExitError, "full coordinates required (domain/repo/namespace/package[@version])")
	}
	if coords.Version == "" {
		v, verr := resolveVersion(versionFlag)
		if verr != nil {
			return fail(out, "verify", cob.ExitError, "%s", verr)
		}
		coords.Version = v
	}

	client, err := cob.NewClient(ctx, cob.ClientOptions{Profile: flagProfile, Region: flagRegion})
	if err != nil {
		return fail(out, "verify", cob.ExitError, "%s", err)
	}
	registry := cob.NewRegistry(client)
	if err := resolveLatestIfNeeded(ctx, coords, registry, out); err != nil {
		return fail(out, "verify", cob.ExitNotFound, "%s", err)
	}

	prov, err := cob.FetchProvenance(ctx, client.CodeArtifact, coords)
	if err != nil {
		return fail(out, "verify", cob.ExitError, "%s", err)
	}
	if prov == nil {
		return fail(out, "verify", cob.ExitError,
			"no %s for %s/%s@%s — cannot self-verify (was it published with cob?)",
			cob.ProvenanceFile, coords.Namespace, coords.Package, coords.Version)
	}

	assets, err := registry.ListAssets(ctx, coords)
	if err != nil {
		return fail(out, "verify", cob.ExitNotFound, "%s", err)
	}
	pubSHA := make(map[string]string, len(assets))
	for _, a := range assets {
		pubSHA[a.Name] = a.SHA256
	}

	out.Header("Self-verifying %s/%s@%s in %s/%s against recorded provenance",
		coords.Namespace, coords.Package, coords.Version, coords.Domain, coords.Repository)
	renderChain(out, prov)
	renderOrigins(out, prov, "")

	result := &cob.CommandResult{
		Command:    "verify",
		Package:    fmt.Sprintf("%s/%s@%s", coords.Namespace, coords.Package, coords.Version),
		Repository: fmt.Sprintf("%s/%s", coords.Domain, coords.Repository),
		Status:     "ok",
	}

	var failures int
	for _, e := range prov.Assets {
		ar := cob.AssetResult{Name: e.Asset, Source: e.Source, SHA256: e.SHA256}
		cur, ok := pubSHA[e.Asset]
		switch {
		case !ok:
			failures++
			ar.Method = "missing"
			out.AssetFail(e.Asset, e.Source, fmt.Errorf("recorded in provenance but not in the published version"))
		case cur == e.SHA256:
			ar.Method = "match(provenance)"
			out.AssetOK(&ar, e.Source)
		default:
			failures++
			ar.Method = "altered"
			out.AssetFail(e.Asset, e.Source, fmt.Errorf("altered since publish: provenance %s != published %s",
				short(e.SHA256), short(cur)))
		}
		result.Assets = append(result.Assets, ar)
	}

	recorded := make(map[string]bool, len(prov.Assets))
	for _, e := range prov.Assets {
		recorded[e.Asset] = true
	}
	var extra int
	for _, a := range assets {
		if a.Name != cob.ProvenanceFile && !recorded[a.Name] {
			extra++
		}
	}
	if extra > 0 {
		out.Warn("%d published asset(s) not recorded in provenance", extra)
	}

	if failures > 0 {
		result.Status = "error"
		result.Error = fmt.Sprintf("%d asset(s) failed self-verification", failures)
		out.Summary("FAILED: %d asset(s) altered or missing since publish.", failures)
		out.CommandResult(result)
		return &ExitError{Code: cob.ExitError}
	}
	out.Summary("OK: every recorded asset still matches; chain intact.")
	return out.CommandResult(result)
}

func short(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}
