package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/jmurray2011/cob/internal/cob"
	"github.com/jmurray2011/cob/internal/manifest"
)

func newVerifyCmd(cfg *Config) *cobra.Command {
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
		Example: `  # verify a published version against its recorded provenance
  cob verify acme/dev/tools/my-app@2.1.0

  # verify a manifest still matches what was published
  cob verify ./my-package.yaml --version 2.1.0`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runVerify(cmd.Context(), cfg, args[0], flagVersion, flagDeep)
		},
	}
	cmd.Flags().StringVar(&flagVersion, "version", "", "Package version (required, or set COB_VERSION)")
	cmd.Flags().BoolVar(&flagDeep, "deep", false, "Manifest mode: download and hash sources lacking a checksum")
	return cmd
}

func runVerify(ctx context.Context, cfg *Config, target, versionFlag string, deep bool) error {
	if isManifestPath(target) {
		return runVerifyManifest(ctx, cfg, target, versionFlag, deep)
	}
	return runVerifyCoords(ctx, cfg, target, versionFlag)
}

func runVerifyManifest(ctx context.Context, cfg *Config, manifestPath, versionFlag string, deep bool) error {
	out := newWriter(cfg)

	version, err := resolveVersion(versionFlag)
	if err != nil {
		return fail(out, "verify", cob.ExitError, "%s", err)
	}
	m, err := manifest.Load(manifestPath)
	if err != nil {
		return fail(out, "verify", cob.ExitError, "%s", err)
	}
	warnManifestOverrides(m, out)

	coords := &cob.PackageCoordinates{
		Domain: m.Domain, Repository: m.Repository,
		Namespace: m.Namespace, Package: m.Package, Version: version,
	}

	client, err := dialClient(ctx, cfg)
	if err != nil {
		return fail(out, "verify", cob.ExitError, "%s", err)
	}

	// Resolve @latest before expanding ${VERSION} — otherwise the manifest's
	// source URIs and the lookup would target a version literally "latest".
	registry := cob.NewRegistry(client)
	if err := resolveLatestIfNeeded(ctx, coords, registry, out); err != nil {
		return fail(out, "verify", codeFor(err), "%s", err)
	}
	version = coords.Version

	if err := m.ResolveVariables(version); err != nil {
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
	cmps, err := compareManifestToPublished(ctx, sources, registry, coords, deep, prov)
	if err != nil {
		return fail(out, "verify", codeFor(err), "%s", err)
	}

	out.Header("Verifying %s/%s@%s against %s", m.Namespace, m.Package, version, manifestPath)

	result := &cob.CommandResult{
		Command:    "verify",
		Package:    fmt.Sprintf("%s/%s@%s", m.Namespace, m.Package, version),
		Repository: fmt.Sprintf("%s/%s", m.Domain, m.Repository),
		Status:     "ok",
	}
	fillClientMeta(ctx, client, result)

	var failures, opErrors, unverified, extra int
	for _, c := range cmps {
		ar := cob.AssetResult{Name: c.Name, Source: c.Source, SHA256: c.SrcSHA}
		switch {
		case !c.InManifest && c.InPublished:
			extra++
			ar.Method = "extra"
			out.AssetSkipped(c.Name + " (published, not in manifest)")
		case c.Err != nil:
			// An operational error (couldn't hash a source, network) — the
			// check did not complete; distinct from a real mismatch.
			opErrors++
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
		case strings.EqualFold(c.SrcSHA, c.PubSHA):
			// hex SHA-256 is case-insensitive — different sources/SDKs return
			// upper- vs lower-hex, so EqualFold avoids a spurious mismatch.
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

	// An operational error means the check did not complete — exit "error".
	// A clean run that found mismatches/drift exits "mismatch" (code 4), so a
	// CI gate can tell "the bytes changed" from "the check failed to run".
	if opErrors > 0 {
		result.Status = "error"
		result.Error = fmt.Sprintf("%d asset(s) could not be checked", opErrors)
		out.Summary("FAILED: %d asset(s) could not be checked.", opErrors)
		out.CommandResult(result)
		return &ExitError{Code: cob.ExitError}
	}
	if failures > 0 {
		result.Status = "mismatch"
		result.Error = fmt.Sprintf("%d asset(s) do not match the manifest", failures)
		out.Summary("FAILED: %d mismatched/missing.", failures)
		out.CommandResult(result)
		return &ExitError{Code: cob.ExitMismatch}
	}
	out.Summary("OK: published version matches the manifest.")
	return out.CommandResult(result)
}

// runVerifyCoords self-verifies a published version against its own recorded
// cob-provenance.json — no manifest, no source access. Every recorded asset
// must still hash to what provenance recorded, and the chain of evidence is
// printed.
func runVerifyCoords(ctx context.Context, cfg *Config, target, versionFlag string) error {
	out := newWriter(cfg)

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

	client, err := dialClient(ctx, cfg)
	if err != nil {
		return fail(out, "verify", cob.ExitError, "%s", err)
	}
	registry := cob.NewRegistry(client)
	if err := resolveLatestIfNeeded(ctx, coords, registry, out); err != nil {
		return fail(out, "verify", codeFor(err), "%s", err)
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
		return fail(out, "verify", codeFor(err), "%s", err)
	}
	pubSHA := make(map[string]string, len(assets))
	for _, a := range assets {
		pubSHA[a.Name] = a.SHA256
	}

	out.Header("Self-verifying %s/%s@%s in %s/%s against recorded provenance",
		coords.Namespace, coords.Package, coords.Version, coords.Domain, coords.Repository)
	renderChain(out, prov, nil)
	renderOrigins(out, prov, "")

	result := &cob.CommandResult{
		Command:    "verify",
		Package:    fmt.Sprintf("%s/%s@%s", coords.Namespace, coords.Package, coords.Version),
		Repository: fmt.Sprintf("%s/%s", coords.Domain, coords.Repository),
		Status:     "ok",
	}
	fillClientMeta(ctx, client, result)

	var failures int
	for _, e := range prov.Assets {
		ar := cob.AssetResult{Name: e.Asset, Source: e.Source, SHA256: e.SHA256}
		cur, ok := pubSHA[e.Asset]
		switch {
		case !ok:
			failures++
			ar.Method = "missing"
			out.AssetFail(e.Asset, e.Source, fmt.Errorf("recorded in provenance but not in the published version"))
		case strings.EqualFold(cur, e.SHA256):
			// hex SHA-256 case-insensitive — see compareManifestToPublished.
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
		result.Status = "mismatch"
		result.Error = fmt.Sprintf("%d asset(s) altered or missing since publish", failures)
		out.Summary("FAILED: %d asset(s) altered or missing since publish.", failures)
		out.CommandResult(result)
		return &ExitError{Code: cob.ExitMismatch}
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
