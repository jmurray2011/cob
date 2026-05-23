package cli

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/jmurray2011/cob/internal/cob"
	"github.com/jmurray2011/cob/internal/manifest"
)

func newDiffCmd(cfg *Config) *cobra.Command {
	var (
		flagVersion string
		flagDeep    bool
	)

	cmd := &cobra.Command{
		Use:   "diff <manifest> | <coords-a> <coords-b>",
		Short: "Show how a manifest differs from a published version, or how two published versions differ",
		Long: "Two modes:\n\n" +
			"Manifest mode (one arg): compares a manifest's sources against a " +
			"published version. Source hash precedence: known checksum → " +
			"recorded cob-provenance.json → (with --deep) download+hash. " +
			"Run before `publish --force` to see what would change.\n\n" +
			"Version-to-version mode (two args): compares two published " +
			"versions of the same package — added (+), removed (-), changed " +
			"(~), same. Both sides' SHA-256s come from CodeArtifact's stored " +
			"asset metadata, so no downloads are needed (and --deep is " +
			"ignored). \"What changed in this release?\"\n\n" +
			"Both modes exit 1 on any drift, 0 when identical — like `diff(1)`.",
		Example: `  # manifest vs published
  cob diff ./my-package.yaml --version 2.1.0

  # what changed between two releases
  cob diff acme/dev/tools/my-app@2.0.0 acme/dev/tools/my-app@2.1.0

  # cross-repo: did promotion preserve the bytes?
  cob diff acme/dev/tools/my-app@2.1.0 acme/prod/tools/my-app@2.1.0`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 2 {
				return runDiffVersions(cmd.Context(), cfg, args[0], args[1])
			}
			return runDiff(cmd.Context(), cfg, args[0], flagVersion, flagDeep)
		},
	}
	cmd.Flags().StringVar(&flagVersion, "version", "", "Package version (required for manifest mode, or set COB_VERSION)")
	cmd.Flags().BoolVar(&flagDeep, "deep", false, "Manifest mode: download and hash sources lacking a checksum (no S3 writes)")
	return cmd
}

// runDiffVersions compares two published versions of the same package by
// the SHA-256 each side has recorded in CodeArtifact. The provenance
// asset is excluded from the comparison on both sides — its bytes always
// differ (chain timestamps, IDs) but that's not a change in the package
// itself. cross-repo comparison is allowed and is the natural way to ask
// "did promotion preserve the bytes?".
func runDiffVersions(ctx context.Context, cfg *Config, leftTarget, rightTarget string) error {
	out := newWriter(cfg)

	left, err := manifest.ParseCoordinates(leftTarget)
	if err != nil {
		return fail(out, "diff", cob.ExitError, "left: %s", err)
	}
	right, err := manifest.ParseCoordinates(rightTarget)
	if err != nil {
		return fail(out, "diff", cob.ExitError, "right: %s", err)
	}
	for _, c := range []*cob.PackageCoordinates{left, right} {
		if c.Namespace == "" || c.Package == "" || c.Version == "" {
			return fail(out, "diff", cob.ExitError,
				"both arguments must be full coordinates with a version (domain/repo/ns/pkg@version)")
		}
	}
	// Same-package guard: comparing tools/app to libs/utils would be
	// meaningful only by coincidence of asset names. Cross-*repo* same
	// package is the supported cross-cutting case; cross-package is not.
	if left.Namespace != right.Namespace || left.Package != right.Package {
		return fail(out, "diff", cob.ExitError,
			"both versions must reference the same package (%s/%s vs %s/%s)",
			left.Namespace, left.Package, right.Namespace, right.Package)
	}

	client, err := dialClient(ctx, cfg)
	if err != nil {
		return fail(out, "diff", cob.ExitError, "%s", err)
	}
	registry := cob.NewRegistry(client)
	if err := resolveLatestIfNeeded(ctx, left, registry, out); err != nil {
		return fail(out, "diff", codeFor(err), "left: %s", err)
	}
	if err := resolveLatestIfNeeded(ctx, right, registry, out); err != nil {
		return fail(out, "diff", codeFor(err), "right: %s", err)
	}

	cmps, err := compareVersions(ctx, registry, left, right)
	if err != nil {
		return fail(out, "diff", codeFor(err), "%s", err)
	}

	leftLabel := fmt.Sprintf("%s/%s/%s/%s@%s", left.Domain, left.Repository, left.Namespace, left.Package, left.Version)
	rightLabel := fmt.Sprintf("%s/%s/%s/%s@%s", right.Domain, right.Repository, right.Namespace, right.Package, right.Version)
	out.Header("diff %s vs %s", leftLabel, rightLabel)

	result := &cob.CommandResult{
		Command:    "diff",
		Package:    fmt.Sprintf("%s/%s: %s vs %s", left.Namespace, left.Package, left.Version, right.Version),
		Repository: fmt.Sprintf("%s/%s vs %s/%s", left.Domain, left.Repository, right.Domain, right.Repository),
		Status:     "ok",
	}
	fillClientMeta(ctx, client, result)

	var added, removed, changed, same int
	for _, c := range cmps {
		ar := cob.AssetResult{Name: c.Name, SHA256: c.RightSHA}
		switch {
		case !c.InLeft && c.InRight:
			added++
			ar.Method = "added"
			out.Plain("  + %s  (added in %s)", c.Name, right.Version)
		case c.InLeft && !c.InRight:
			removed++
			ar.Method = "removed"
			out.Plain("  - %s  (removed in %s)", c.Name, right.Version)
		case !strings.EqualFold(c.LeftSHA, c.RightSHA):
			// hex SHA-256 case-insensitive — avoid spurious drift.
			changed++
			ar.Method = "changed"
			out.Plain("  ~ %s  (%s -> %s)", c.Name, short(c.LeftSHA), short(c.RightSHA))
		default:
			same++
			ar.Method = "same"
		}
		result.Assets = append(result.Assets, ar)
	}

	drift := added + removed + changed
	out.Summary("%d added, %d removed, %d changed, %d same.", added, removed, changed, same)

	if drift > 0 {
		result.Status = "drift"
		result.Error = fmt.Sprintf("%d added, %d removed, %d changed", added, removed, changed)
		out.CommandResult(result)
		return &ExitError{Code: cob.ExitMismatch}
	}
	return out.CommandResult(result)
}

// versionCompare is one asset's left-vs-right comparison for the
// version-to-version diff. SHAs come straight from CodeArtifact's stored
// asset metadata on each side, so no downloads happen and a per-source
// "from where" doesn't apply.
type versionCompare struct {
	Name     string
	LeftSHA  string
	RightSHA string
	InLeft   bool
	InRight  bool
}

// compareVersions builds a union of stored asset names across two
// published versions (excluding the provenance asset, whose bytes
// trivially differ even when the package itself didn't change) and
// records each side's recorded SHA-256.
func compareVersions(ctx context.Context, reg *cob.Registry, left, right *cob.PackageCoordinates) ([]versionCompare, error) {
	leftAssets, err := reg.ListAssets(ctx, left)
	if err != nil {
		return nil, fmt.Errorf("listing left: %w", err)
	}
	rightAssets, err := reg.ListAssets(ctx, right)
	if err != nil {
		return nil, fmt.Errorf("listing right: %w", err)
	}

	leftByName := assetMapExcludingProvenance(leftAssets)
	rightByName := assetMapExcludingProvenance(rightAssets)

	names := make(map[string]struct{}, len(leftByName)+len(rightByName))
	for n := range leftByName {
		names[n] = struct{}{}
	}
	for n := range rightByName {
		names[n] = struct{}{}
	}
	sorted := make([]string, 0, len(names))
	for n := range names {
		sorted = append(sorted, n)
	}
	sort.Strings(sorted)

	out := make([]versionCompare, 0, len(sorted))
	for _, n := range sorted {
		c := versionCompare{Name: n}
		if l, ok := leftByName[n]; ok {
			c.InLeft = true
			c.LeftSHA = l.SHA256
		}
		if r, ok := rightByName[n]; ok {
			c.InRight = true
			c.RightSHA = r.SHA256
		}
		out = append(out, c)
	}
	return out, nil
}

func assetMapExcludingProvenance(assets []cob.AssetSummary) map[string]cob.AssetSummary {
	m := make(map[string]cob.AssetSummary, len(assets))
	for _, a := range assets {
		if a.Name == cob.ProvenanceFile {
			continue
		}
		m[a.Name] = a
	}
	return m
}

func runDiff(ctx context.Context, cfg *Config, manifestPath, versionFlag string, deep bool) error {
	out := newWriter(cfg)

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

	client, err := dialClient(ctx, cfg)
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
	fillClientMeta(ctx, client, result)

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
