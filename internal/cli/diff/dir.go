package diff

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jmurray2011/cob/internal/cob"
	"github.com/jmurray2011/cob/internal/manifest"
	"github.com/jmurray2011/cob/internal/output"

	"github.com/jmurray2011/cob/internal/cliutil"
)

// runDir hashes every file in <dir> whose name matches a published
// asset of <coords> and compares to the published SHA-256. Was
// `cob verify <dir> <coords>`. Local files without a matching
// published asset are silently left alone (could be a README, source
// files, etc.). Published assets without a local file are reported as
// "missing locally". The cob-provenance.json asset is excluded — it's
// audit metadata, not a package file.
func runDir(ctx context.Context, cfg *cliutil.Config, out *output.Writer, dirPath, coordsArg string, verbose bool) error {
	ctx, cancel := cliutil.Interruptable(ctx, cfg, out)
	defer cancel()

	coords, err := manifest.ParseCoordinates(coordsArg)
	if err != nil {
		return cliutil.Fail(out, "diff", cob.ExitError, "%s", err)
	}
	if coords.Namespace == "" || coords.Package == "" {
		return cliutil.Fail(out, "diff", cob.ExitError,
			"full coordinates required (domain/repo/namespace/package[@version]); got %q", coordsArg)
	}
	if coords.Version == "" {
		return cliutil.Fail(out, "diff", cob.ExitError,
			"version required: use @version or @latest in the coordinates")
	}

	client, err := cliutil.DialClient(ctx, cfg, out)
	if err != nil {
		return cliutil.Fail(out, "diff", cob.ExitError, "%s", err)
	}
	registry := cob.NewRegistry(client)
	if err := cliutil.ResolveLatestIfNeeded(ctx, coords, registry, out); err != nil {
		return cliutil.Fail(out, "diff", cliutil.CodeFor(err), "%s", err)
	}

	pub, err := registry.ListAssets(ctx, coords)
	if err != nil {
		return cliutil.Fail(out, "diff", cliutil.CodeFor(err), "%s", err)
	}

	absDir, _ := filepath.Abs(dirPath)
	out.Header("diff %s vs %s/%s/%s/%s@%s",
		absDir, coords.Domain, coords.Repository, coords.Namespace, coords.Package, coords.Version)
	out.Plain("  (hashing local files; comparing to the published asset SHAs)")
	out.Plain("")

	result := &cob.CommandResult{
		Command:    "diff",
		Package:    fmt.Sprintf("%s/%s@%s", coords.Namespace, coords.Package, coords.Version),
		Repository: fmt.Sprintf("%s/%s", coords.Domain, coords.Repository),
		Status:     "ok",
	}
	cliutil.FillClientMeta(ctx, client, result)

	nameWidth := pickNameColumnWidth(extractAssetNames(pub))
	termWidth := output.TerminalWidth()
	pubLabel := fmt.Sprintf("%s/%s/%s/%s@%s",
		coords.Domain, coords.Repository, coords.Namespace, coords.Package, coords.Version)

	var matched, mismatch, missing, opErrors int
	for _, a := range pub {
		if a.Name == cob.ProvenanceFile {
			continue
		}
		// CodeArtifact asset names are server-controlled and path-like; route
		// through the same cliutil.SafeJoin defense as pull so a malicious or
		// malformed name (../../etc/passwd, an absolute path, a symlinked
		// component) can't pull hashes of files outside dirPath into the
		// comparison report.
		localPath, joinErr := cliutil.SafeJoin(dirPath, a.Name)
		if joinErr != nil {
			opErrors++
			ar := cob.AssetResult{Name: a.Name, Kind: cob.KindCompare, Source: filepath.Join(dirPath, a.Name)}
			ar.SetError(joinErr)
			renderDiffSingleFail(out, a.Name, "unsafe asset name", nameWidth,
				[]diffDetail{{"error", joinErr.Error()}}, termWidth)
			result.Assets = append(result.Assets, ar)
			continue
		}
		ar := cob.AssetResult{Name: a.Name, Kind: cob.KindCompare, Source: localPath}

		info, statErr := os.Stat(localPath)
		switch {
		case os.IsNotExist(statErr):
			missing++
			ar.Method = cob.CompareMissingLocal
			ar.Size = a.Size
			ar.SHA256 = a.SHA256
			renderDiffSingleFail(out, a.Name, "missing locally", nameWidth,
				[]diffDetail{
					{"published", fmt.Sprintf("%s   %s", rightPadSize(a.Size, 10), pubLabel)},
					{"sha256", a.SHA256},
				}, termWidth)
			result.Assets = append(result.Assets, ar)
			continue
		case statErr != nil:
			opErrors++
			ar.SetError(statErr)
			renderDiffSingleFail(out, a.Name, "could not stat local file", nameWidth,
				[]diffDetail{{"error", statErr.Error()}}, termWidth)
			result.Assets = append(result.Assets, ar)
			continue
		case !info.Mode().IsRegular():
			opErrors++
			err := fmt.Errorf("not a regular file (mode %s)", info.Mode())
			ar.SetError(err)
			renderDiffSingleFail(out, a.Name, err.Error(), nameWidth, nil, termWidth)
			result.Assets = append(result.Assets, ar)
			continue
		}

		localHash, hashErr := fileSHA256(localPath)
		if hashErr != nil {
			opErrors++
			ar.SetError(hashErr)
			renderDiffSingleFail(out, a.Name, "could not hash local file", nameWidth,
				[]diffDetail{{"error", hashErr.Error()}}, termWidth)
			result.Assets = append(result.Assets, ar)
			continue
		}
		ar.Size = info.Size()
		ar.SHA256 = localHash

		if strings.EqualFold(localHash, a.SHA256) {
			matched++
			ar.Method = cob.CompareMatch
			renderDiffMatch(out, a.Name, localPath, "match", localHash,
				info.Size(), nameWidth, termWidth, verbose)
		} else {
			mismatch++
			ar.Method = cob.CompareMismatch
			ar.ErrorMsg = fmt.Sprintf("SHA-256 mismatch: local=%s published=%s", localHash, a.SHA256)
			renderDiffMismatch(out, a.Name, nameWidth,
				diffSide{label: "local", uri: localPath, size: info.Size(), hash: localHash},
				diffSide{label: "published", uri: pubLabel, size: a.Size, hash: a.SHA256},
			)
		}
		result.Assets = append(result.Assets, ar)
	}

	out.Plain("")

	// Exit-code precedence: real drift (mismatch/missing) wins over
	// op-errors. CI gates that branch on exit 4 (ExitMismatch) get the
	// user-visible truth — "stuff differs" — even when one row also
	// failed to stat. The opposite ordering meant a single transient
	// op-error masked the actual diff and turned a deterministic-cliutil.Fail
	// gate into "retry, maybe it's flaky." Op errors still surface
	// (warning in the summary line + per-asset Err on the JSON result),
	// they just don't *override* a mismatch verdict.
	switch {
	case mismatch > 0 || missing > 0:
		result.Status = "mismatch"
		if opErrors > 0 {
			result.Error = fmt.Sprintf("%d mismatch, %d missing locally (%d also failed to check)", mismatch, missing, opErrors)
			out.Summary("FAILED: %d mismatch, %d missing locally, %d could not be checked (%d matched).",
				mismatch, missing, opErrors, matched)
		} else {
			result.Error = fmt.Sprintf("%d mismatch, %d missing locally", mismatch, missing)
			out.Summary("FAILED: %d mismatch, %d missing locally (%d matched).",
				mismatch, missing, matched)
		}
		out.CommandResult(result)
		return &cliutil.ExitError{Code: cob.ExitMismatch}
	case opErrors > 0:
		result.Status = "error"
		result.Error = fmt.Sprintf("%d asset(s) could not be checked", opErrors)
		out.Summary("FAILED: %d could not be checked (%d matched).", opErrors, matched)
		out.CommandResult(result)
		return &cliutil.ExitError{Code: cob.ExitError}
	}
	out.Summary("OK: %d files match the published version byte-for-byte.", matched)
	return out.CommandResult(result)
}
