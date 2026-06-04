package diff

import (
	"context"
	"fmt"
	"strings"

	"github.com/jmurray2011/cob/internal/cob"
	"github.com/jmurray2011/cob/internal/manifest"
	"github.com/jmurray2011/cob/internal/output"

	"github.com/jmurray2011/cob/internal/cliutil"
)

// runSelfCheck compares a published version's recorded provenance
// to what CodeArtifact currently stores. Was `cob verify <coords>`.
// Chain of evidence is printed first; the comparison follows.
func runSelfCheck(ctx context.Context, cfg *cliutil.Config, out *output.Writer, target string, verbose, checkRefs bool) error {
	ctx, cancel := cliutil.Interruptable(ctx, cfg, out)
	defer cancel()

	coords, err := manifest.ParseCoordinates(target)
	if err != nil {
		return cliutil.Fail(out, "diff", cob.ExitError, "%s", err)
	}
	if coords.Namespace == "" || coords.Package == "" {
		return cliutil.Fail(out, "diff", cob.ExitError,
			"full coordinates required (domain/repo/namespace/package[@version])")
	}
	if coords.Version == "" {
		return cliutil.Fail(out, "diff", cob.ExitError,
			"version required (use @version or @latest)")
	}

	client, err := cliutil.DialClient(ctx, cfg)
	if err != nil {
		return cliutil.Fail(out, "diff", cob.ExitError, "%s", err)
	}
	registry := cob.NewRegistry(client)
	if err := cliutil.ResolveLatestIfNeeded(ctx, coords, registry, out); err != nil {
		return cliutil.Fail(out, "diff", cliutil.CodeFor(err), "%s", err)
	}

	prov, err := cob.FetchProvenance(ctx, client.CodeArtifact, coords)
	if err != nil {
		return cliutil.Fail(out, "diff", cob.ExitError, "%s", err)
	}
	if prov == nil {
		return cliutil.Fail(out, "diff", cob.ExitError,
			"no %s for %s/%s@%s — cannot self-check (was it published with cob?)",
			cob.ProvenanceFile, coords.Namespace, coords.Package, coords.Version)
	}
	if checkRefs {
		warnMissingChainRefs(ctx, registry, coords, prov, out)
	}

	assets, err := registry.ListAssets(ctx, coords)
	if err != nil {
		return cliutil.Fail(out, "diff", cliutil.CodeFor(err), "%s", err)
	}
	pubSHA := make(map[string]string, len(assets))
	pubSize := make(map[string]int64, len(assets))
	for _, a := range assets {
		pubSHA[a.Name] = a.SHA256
		pubSize[a.Name] = a.Size
	}

	out.Header("Self-check %s/%s@%s in %s/%s against recorded provenance",
		coords.Namespace, coords.Package, coords.Version, coords.Domain, coords.Repository)

	result := &cob.CommandResult{
		Command:    "diff",
		Package:    fmt.Sprintf("%s/%s@%s", coords.Namespace, coords.Package, coords.Version),
		Repository: fmt.Sprintf("%s/%s", coords.Domain, coords.Repository),
		Status:     "ok",
	}
	cliutil.FillClientMeta(ctx, client, result)

	names := make([]string, 0, len(prov.Assets))
	for _, e := range prov.Assets {
		names = append(names, e.Asset)
	}
	nameWidth := pickNameColumnWidth(names)
	termWidth := output.TerminalWidth()
	pubLabel := fmt.Sprintf("%s/%s/%s/%s@%s",
		coords.Domain, coords.Repository, coords.Namespace, coords.Package, coords.Version)

	var failures int
	for _, e := range prov.Assets {
		ar := cob.AssetResult{Name: e.Asset, Kind: cob.KindCompare, Source: e.Source, SHA256: e.SHA256, Size: e.Size}
		cur, ok := pubSHA[e.Asset]
		switch {
		case !ok:
			failures++
			ar.Method = cob.CompareMissing
			renderDiffSingleFail(out, e.Asset,
				"recorded in provenance but not in the published version", nameWidth,
				[]diffDetail{
					{"recorded", fmt.Sprintf("%s   %s", rightPadSize(e.Size, 10), e.SHA256)},
				}, termWidth)
		case strings.EqualFold(cur, e.SHA256):
			ar.Method = cob.CompareMatch + "(provenance)"
			renderDiffMatch(out, e.Asset, "", "match(provenance)", e.SHA256,
				e.Size, nameWidth, termWidth, verbose)
		default:
			failures++
			ar.Method = cob.CompareAltered
			ar.ErrorMsg = fmt.Sprintf("altered since publish: recorded=%s published=%s", e.SHA256, cur)
			renderDiffMismatch(out, e.Asset, nameWidth,
				diffSide{label: "recorded", uri: "(provenance)", size: e.Size, hash: e.SHA256},
				diffSide{label: "published", uri: pubLabel, size: pubSize[e.Asset], hash: cur},
			)
		}
		result.Assets = append(result.Assets, ar)
	}

	out.Plain("")

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
		// Chain + origins render *after* the verdict so the operator
		// sees "did it pass or fail?" before scrolling past supporting
		// evidence. Pre-A2-review the chain rendered above the
		// comparison and pushed the verdict many lines down — diff is
		// for the verdict; the chain is context.
		renderProvenanceContext(out, prov)
		out.CommandResult(result)
		return &cliutil.ExitError{Code: cob.ExitMismatch}
	}
	out.Summary("OK: every recorded asset still matches; chain intact.")
	renderProvenanceContext(out, prov)
	return out.CommandResult(result)
}

// warnMissingChainRefs probes each unique repository in prov.Chain (the
// same probe `cob log --check-references` does) and emits a single warning
// if any reference is deleted or unreachable. Centralized so the manifest
// and self-check diff modes both get the same behavior — and so the day
// we want to surface this more richly (per-line annotations on the
// rendered chain), there's one helper to upgrade.
func warnMissingChainRefs(ctx context.Context, registry *cob.Registry, coords *cob.PackageCoordinates, prov *cob.Provenance, out *output.Writer) {
	statuses := cliutil.ProbeChainReferences(ctx, registry, coords, prov)
	if len(statuses) == 0 {
		return
	}
	missing, unknown := 0, 0
	for _, s := range statuses {
		switch s {
		case cliutil.ChainRefMissing:
			missing++
		case cliutil.ChainRefUnknown:
			unknown++
		}
	}
	if missing > 0 || unknown > 0 {
		out.Warn("chain references: %d deleted, %d could not be probed (run `cob log %s/%s/%s/%s@%s --check-references` for details)",
			missing, unknown, coords.Domain, coords.Repository, coords.Namespace, coords.Package, coords.Version)
	}
}

// renderProvenanceContext prints the chain of evidence and per-asset
// origins as supporting context after the diff verdict. The user-facing
// answer (match / mismatch / altered / missing) appears first in the
// output; this trailer gives the operator the who/where/when if they
// want it. `cob log <coords>` remains the right tool when you only
// want the chain without re-listing assets to compare.
func renderProvenanceContext(out *output.Writer, prov *cob.Provenance) {
	out.Plain("")
	cliutil.RenderChain(out, prov, nil)
	cliutil.RenderOrigins(out, prov, "")
}
