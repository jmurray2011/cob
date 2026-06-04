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

// runManifest compares a manifest's sources to a published
// version's assets. Implicit cliutil.ValidateManifest runs at the top — so a
// broken manifest can't reach the comparison loop.
func runManifest(ctx context.Context, cfg *cliutil.Config, out *output.Writer, manifestPath, version string, deep, verbose, checkRefs bool) error {
	ctx, cancel := cliutil.Interruptable(ctx, cfg, out)
	defer cancel()

	m, err := manifest.Load(manifestPath)
	if err != nil {
		return cliutil.Fail(out, "diff", cob.ExitError, "%s", err)
	}
	cliutil.WarnManifestOverrides(m, out)
	if err := cliutil.ValidateManifest(m, version); err != nil {
		return cliutil.Fail(out, "diff", cob.ExitError, "%s", err)
	}

	coords := &cob.PackageCoordinates{
		Domain: m.Domain, Repository: m.Repository,
		Namespace: m.Namespace, Package: m.Package, Version: version,
	}

	client, err := cliutil.DialClient(ctx, cfg, out)
	if err != nil {
		return cliutil.Fail(out, "diff", cob.ExitError, "%s", err)
	}

	// Resolve @latest before expanding ${VERSION} — otherwise the
	// manifest's source URIs and the lookup would target a version
	// literally "latest".
	registry := cob.NewRegistry(client)
	if err := cliutil.ResolveLatestIfNeeded(ctx, coords, registry, out); err != nil {
		return cliutil.Fail(out, "diff", cliutil.CodeFor(err), "%s", err)
	}
	version = coords.Version

	if err := m.ResolveVariables(version); err != nil {
		return cliutil.Fail(out, "diff", cob.ExitError, "%s", err)
	}

	sources, err := cliutil.BuildSources(m, client)
	if err != nil {
		return cliutil.Fail(out, "diff", cob.ExitError, "%s", err)
	}

	prov, perr := cob.FetchProvenance(ctx, client.CodeArtifact, coords)
	if perr != nil {
		out.Warn("could not read %s: %s", cob.ProvenanceFile, perr)
	}
	// --check-references mirrors `cob log --check-references`: probe each
	// unique repository in the chain (publish.Repository, promote.From,
	// promote.To). One VersionStatus per repo, in parallel; deleted/
	// unreachable references surface as a warning before the diff proceeds.
	if checkRefs && prov != nil {
		warnMissingChainRefs(ctx, registry, coords, prov, out)
	}
	cmps, err := compareManifestToPublished(ctx, sources, registry, coords, deep, prov)
	if err != nil {
		return cliutil.Fail(out, "diff", cliutil.CodeFor(err), "%s", err)
	}

	// Pub size lookup so mismatch rows show both sides' sizes when known.
	pubSize := make(map[string]int64)
	if pub, perr := registry.ListAssets(ctx, coords); perr == nil {
		for _, a := range pub {
			pubSize[a.Name] = a.Size
		}
	}

	out.Header("diff %s/%s@%s vs %s", m.Namespace, m.Package, version, manifestPath)

	result := &cob.CommandResult{
		Command:    "diff",
		Package:    fmt.Sprintf("%s/%s@%s", m.Namespace, m.Package, version),
		Repository: fmt.Sprintf("%s/%s", m.Domain, m.Repository),
		Status:     "ok",
	}
	cliutil.FillClientMeta(ctx, client, result)

	names := make([]string, 0, len(cmps))
	for _, c := range cmps {
		names = append(names, c.Name)
	}
	nameWidth := pickNameColumnWidth(names)
	termWidth := output.TerminalWidth()
	pubLabel := fmt.Sprintf("%s/%s/%s/%s@%s",
		m.Domain, m.Repository, m.Namespace, m.Package, version)

	var added, removed, changed, same, unknown, errs int
	for _, c := range cmps {
		ar := cob.AssetResult{Name: c.Name, Kind: cob.KindCompare, Source: c.Source, SHA256: c.SrcSHA}
		switch {
		case c.Err != nil:
			errs++
			ar.SetError(c.Err)
			renderDiffSingleFail(out, c.Name, "could not check", nameWidth,
				[]diffDetail{
					{"source", c.Source},
					{"error", c.Err.Error()},
				}, termWidth)
		case c.InManifest && !c.InPublished:
			added++
			ar.Method = cob.CompareAdded
			out.Plain("  + %s  (not published in @%s)", padRight(c.Name, nameWidth), version)
		case !c.InManifest && c.InPublished:
			removed++
			ar.Method = cob.CompareRemoved
			out.Plain("  - %s  (published, not in manifest)", padRight(c.Name, nameWidth))
		case c.OriginDrift:
			changed++
			ar.Method = cob.CompareChanged
			renderDiffSingleFail(out, c.Name,
				"S3 source changed since publish (etag/version differs from provenance)",
				nameWidth, []diffDetail{{"source", c.Source}}, termWidth)
		case c.SrcSHA == "":
			unknown++
			ar.Method = cob.CompareUnknown
			renderDiffSkipped(out, c.Name,
				"unverified — no checksum/provenance (use --deep to hash)",
				nameWidth, []diffDetail{{"source", c.Source}})
		case strings.EqualFold(c.SrcSHA, c.PubSHA):
			// hex SHA-256 case-insensitive — different sources/SDKs return
			// upper- vs lower-hex; EqualFold avoids a spurious mismatch.
			same++
			ar.Method = cob.CompareMatch + "(" + c.SrcFrom + ")"
			renderDiffMatch(out, c.Name, c.Source,
				fmt.Sprintf("match(%s)", c.SrcFrom), c.SrcSHA,
				pubSize[c.Name], nameWidth, termWidth, verbose)
		default:
			changed++
			ar.Method = cob.CompareChanged
			ar.ErrorMsg = fmt.Sprintf("SHA-256 mismatch: source=%s published=%s", c.SrcSHA, c.PubSHA)
			renderDiffMismatch(out, c.Name, nameWidth,
				diffSide{label: "source", uri: c.Source, hash: c.SrcSHA},
				diffSide{label: "published", uri: pubLabel, size: pubSize[c.Name], hash: c.PubSHA},
			)
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
		return &cliutil.ExitError{Code: cob.ExitError}
	}
	if drift > 0 {
		result.Status = "drift"
		result.Error = fmt.Sprintf("%d added, %d removed, %d changed", added, removed, changed)
		out.CommandResult(result)
		return &cliutil.ExitError{Code: cob.ExitMismatch}
	}
	return out.CommandResult(result)
}
