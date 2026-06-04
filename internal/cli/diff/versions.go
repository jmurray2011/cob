package diff

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/jmurray2011/cob/internal/cob"
	"github.com/jmurray2011/cob/internal/manifest"
	"github.com/jmurray2011/cob/internal/output"

	"github.com/jmurray2011/cob/internal/cliutil"
)

// runVersions compares two published versions of the same package
// by the SHA-256 each side has recorded in CodeArtifact. The
// provenance asset is excluded (its bytes trivially differ even when
// the package didn't change). Cross-repo same-package is allowed —
// the natural "did promotion preserve the bytes?" check.
func runVersions(ctx context.Context, cfg *cliutil.Config, out *output.Writer, leftTarget, rightTarget string, verbose bool) error {
	left, err := manifest.ParseCoordinates(leftTarget)
	if err != nil {
		return cliutil.Fail(out, "diff", cob.ExitError, "left: %s", err)
	}
	right, err := manifest.ParseCoordinates(rightTarget)
	if err != nil {
		return cliutil.Fail(out, "diff", cob.ExitError, "right: %s", err)
	}
	for _, c := range []*cob.PackageCoordinates{left, right} {
		if c.Namespace == "" || c.Package == "" || c.Version == "" {
			return cliutil.Fail(out, "diff", cob.ExitError,
				"both arguments must be full coordinates with a version (domain/repo/ns/pkg@version)")
		}
	}
	if left.Namespace != right.Namespace || left.Package != right.Package {
		return cliutil.Fail(out, "diff", cob.ExitError,
			"both versions must reference the same package (%s/%s vs %s/%s)",
			left.Namespace, left.Package, right.Namespace, right.Package)
	}

	client, err := cliutil.DialClient(ctx, cfg)
	if err != nil {
		return cliutil.Fail(out, "diff", cob.ExitError, "%s", err)
	}
	registry := cob.NewRegistry(client)
	if err := cliutil.ResolveLatestIfNeeded(ctx, left, registry, out); err != nil {
		return cliutil.Fail(out, "diff", cliutil.CodeFor(err), "left: %s", err)
	}
	if err := cliutil.ResolveLatestIfNeeded(ctx, right, registry, out); err != nil {
		return cliutil.Fail(out, "diff", cliutil.CodeFor(err), "right: %s", err)
	}

	cmps, err := compareVersions(ctx, registry, left, right)
	if err != nil {
		return cliutil.Fail(out, "diff", cliutil.CodeFor(err), "%s", err)
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
	cliutil.FillClientMeta(ctx, client, result)

	_ = verbose // version-vs-version mode shows compact +/-/~ rows; verbose has no extra detail to add yet

	var added, removed, changed, same int
	for _, c := range cmps {
		ar := cob.AssetResult{Name: c.Name, Kind: cob.KindCompare, SHA256: c.RightSHA}
		switch {
		case !c.InLeft && c.InRight:
			added++
			ar.Method = cob.CompareAdded
			out.Plain("  + %s  (added in %s)", c.Name, right.Version)
		case c.InLeft && !c.InRight:
			removed++
			ar.Method = cob.CompareRemoved
			out.Plain("  - %s  (removed in %s)", c.Name, right.Version)
		case !strings.EqualFold(c.LeftSHA, c.RightSHA):
			// hex SHA-256 case-insensitive — avoid spurious drift.
			changed++
			ar.Method = cob.CompareChanged
			out.Plain("  ~ %s  (%s -> %s)", c.Name, short(c.LeftSHA), short(c.RightSHA))
		default:
			same++
			ar.Method = cob.CompareSame
		}
		result.Assets = append(result.Assets, ar)
	}

	drift := added + removed + changed
	out.Summary("%d added, %d removed, %d changed, %d same.", added, removed, changed, same)

	if drift > 0 {
		result.Status = "drift"
		result.Error = fmt.Sprintf("%d added, %d removed, %d changed", added, removed, changed)
		out.CommandResult(result)
		return &cliutil.ExitError{Code: cob.ExitMismatch}
	}
	return out.CommandResult(result)
}

// versionCompare is one asset's left-vs-right comparison for the
// version-vs-version diff. SHAs come straight from CodeArtifact's
// stored asset metadata on each side, so no downloads happen.
type versionCompare struct {
	Name     string
	LeftSHA  string
	RightSHA string
	InLeft   bool
	InRight  bool
}

// compareVersions builds a union of stored asset names across two
// published versions (excluding the provenance asset, whose bytes
// trivially differ even when the package didn't change) and records
// each side's recorded SHA-256.
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
