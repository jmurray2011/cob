package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/jmurray2011/cob/internal/cob"
	"github.com/jmurray2011/cob/internal/manifest"
	"github.com/jmurray2011/cob/internal/output"
)

func newVerifyCmd(cfg *Config) *cobra.Command {
	var (
		flagVersion string
		flagDeep    bool
	)

	cmd := &cobra.Command{
		Use:   "verify <manifest|coordinates|directory> [coordinates]",
		Short: "Check that bytes match published — for a manifest, a published version's own provenance, or a local directory",
		Long: "Three modes, picked from the positional argument(s):\n\n" +
			"1. Manifest (one arg, file ending .yaml/.yml): hashes each manifest " +
			"source and compares to the published asset of the same name. " +
			"Precedence: known checksum → recorded S3 origin → " +
			"cob-provenance.json → --deep download+hash.\n\n" +
			"2. Coordinates (one arg, domain/repo/ns/pkg[@version]): " +
			"self-verifies a version against its own recorded " +
			"cob-provenance.json — every recorded asset must still hash to " +
			"what provenance recorded — and prints the chain of evidence.\n\n" +
			"3. Directory + coordinates (two args, first must be a directory): " +
			"for each published asset of <coords>, looks for a file of the " +
			"same name in <dir>, hashes it, and compares to the published " +
			"SHA-256. Answers \"do these local files match what was " +
			"published?\" without any manifest in the picture.\n\n" +
			"No mutation; exits non-zero on any mismatch.",
		Example: `  # 1. Manifest mode — does the manifest still describe what was published?
  cob verify ./my-package.yaml --version 2.1.0

  # 2. Coordinates mode — audit a version against its own provenance + chain
  cob verify acme/dev/tools/my-app@2.1.0

  # 3. Directory mode — do my local files match a published version's bytes?
  cob verify ~/pulled-dir acme/dev/tools/my-app@2.1.0`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runVerify(cmd.Context(), cfg, args, flagVersion, flagDeep)
		},
	}
	cmd.Flags().StringVar(&flagVersion, "version", "", "Package version (manifest mode: required, or set COB_VERSION; coordinates/dir modes use @version)")
	cmd.Flags().BoolVar(&flagDeep, "deep", false, "Manifest mode: download and hash sources lacking a checksum")
	return cmd
}

// runVerify dispatches to the right mode. The shape is fixed by what
// args we received:
//
//	1 arg, .yaml/.yml → manifest mode
//	1 arg, directory  → error (directory mode requires coords as arg 2)
//	1 arg, otherwise  → coordinates mode (self-verify)
//	2 args            → directory + coordinates mode (first must be a dir)
func runVerify(ctx context.Context, cfg *Config, args []string, versionFlag string, deep bool) error {
	if len(args) == 2 {
		out := newWriter(cfg)
		defer out.Close()
		// First arg must be a directory; second arg must be coordinates.
		// The verify rendering rejects the case explicitly (rather than
		// silently falling into manifest mode on a file) so a typoed
		// "verify file other-file" surfaces a real error.
		info, err := os.Stat(args[0])
		if err != nil {
			return fail(out, "verify", cob.ExitError, "first argument must be a directory (or omit it to verify a manifest/version): %s", err)
		}
		if !info.IsDir() {
			return fail(out, "verify", cob.ExitError, "first argument must be a directory when two args are given; got file %s", args[0])
		}
		return runVerifyDir(ctx, cfg, out, args[0], args[1])
	}
	target := args[0]
	if isManifestPath(target) {
		return runVerifyManifest(ctx, cfg, target, versionFlag, deep)
	}
	// Catch the obvious mistake: `cob verify /some/dir` without coords.
	if info, err := os.Stat(target); err == nil && info.IsDir() {
		out := newWriter(cfg)
		defer out.Close()
		return fail(out, "verify", cob.ExitError,
			"%s is a directory — to verify its files against a published version, give the coordinates as a second argument:\n  cob verify %s <domain>/<repo>/<ns>/<pkg>@<version>",
			target, target)
	}
	return runVerifyCoords(ctx, cfg, target, versionFlag)
}

// runVerifyDir hashes every file in <dir> whose name matches a published
// asset of <coords> and compares to the published SHA-256. Local files
// without a matching published asset are silently left alone (they
// might be a README, source files, etc.). Published assets without a
// local file are reported as "missing locally". The cob-provenance.json
// asset is excluded — it's audit metadata, not a package file.
func runVerifyDir(ctx context.Context, cfg *Config, out *output.Writer, dirPath, coordsArg string) error {
	ctx, cancel := interruptable(ctx, out)
	defer cancel()

	coords, err := manifest.ParseCoordinates(coordsArg)
	if err != nil {
		return fail(out, "verify", cob.ExitError, "%s", err)
	}
	if coords.Namespace == "" || coords.Package == "" {
		return fail(out, "verify", cob.ExitError,
			"full coordinates required (domain/repo/namespace/package[@version]); got %q", coordsArg)
	}
	if coords.Version == "" {
		return fail(out, "verify", cob.ExitError,
			"version required: use @version or @latest in the coordinates")
	}

	client, err := dialClient(ctx, cfg)
	if err != nil {
		return fail(out, "verify", cob.ExitError, "%s", err)
	}
	registry := cob.NewRegistry(client)
	if err := resolveLatestIfNeeded(ctx, coords, registry, out); err != nil {
		return fail(out, "verify", codeFor(err), "%s", err)
	}

	pub, err := registry.ListAssets(ctx, coords)
	if err != nil {
		return fail(out, "verify", codeFor(err), "%s", err)
	}

	absDir, _ := filepath.Abs(dirPath)
	out.Header("Verifying %s against %s/%s/%s/%s@%s",
		absDir, coords.Domain, coords.Repository, coords.Namespace, coords.Package, coords.Version)
	out.Plain("  (hashing local files; comparing to the published asset SHAs)")
	out.Plain("")

	result := &cob.CommandResult{
		Command:    "verify",
		Package:    fmt.Sprintf("%s/%s@%s", coords.Namespace, coords.Package, coords.Version),
		Repository: fmt.Sprintf("%s/%s", coords.Domain, coords.Repository),
		Status:     "ok",
	}
	fillClientMeta(ctx, client, result)

	nameWidth := pickNameColumnWidth(extractAssetNames(pub))
	termWidth := output.TerminalWidth()
	pubLabel := fmt.Sprintf("%s/%s/%s/%s@%s",
		coords.Domain, coords.Repository, coords.Namespace, coords.Package, coords.Version)

	var matched, mismatch, missing, opErrors int
	for _, a := range pub {
		if a.Name == cob.ProvenanceFile {
			continue
		}
		localPath := filepath.Join(dirPath, a.Name)
		ar := cob.AssetResult{Name: a.Name, Source: localPath}

		info, statErr := os.Stat(localPath)
		switch {
		case os.IsNotExist(statErr):
			missing++
			ar.Method = "missing-local"
			ar.Size = a.Size
			ar.SHA256 = a.SHA256
			renderVerifySingleFail(out, a.Name, "missing locally", nameWidth,
				[]verifyDetail{
					{"published", fmt.Sprintf("%s   %s", rightPadSize(a.Size, 10), pubLabel)},
					{"sha256", a.SHA256},
				}, termWidth)
			result.Assets = append(result.Assets, ar)
			continue
		case statErr != nil:
			opErrors++
			ar.SetError(statErr)
			renderVerifySingleFail(out, a.Name, "could not stat local file", nameWidth,
				[]verifyDetail{{"error", statErr.Error()}}, termWidth)
			result.Assets = append(result.Assets, ar)
			continue
		case !info.Mode().IsRegular():
			opErrors++
			err := fmt.Errorf("not a regular file (mode %s)", info.Mode())
			ar.SetError(err)
			renderVerifySingleFail(out, a.Name, err.Error(), nameWidth, nil, termWidth)
			result.Assets = append(result.Assets, ar)
			continue
		}

		localHash, hashErr := fileSHA256(localPath)
		if hashErr != nil {
			opErrors++
			ar.SetError(hashErr)
			renderVerifySingleFail(out, a.Name, "could not hash local file", nameWidth,
				[]verifyDetail{{"error", hashErr.Error()}}, termWidth)
			result.Assets = append(result.Assets, ar)
			continue
		}
		ar.Size = info.Size()
		ar.SHA256 = localHash

		if strings.EqualFold(localHash, a.SHA256) {
			matched++
			ar.Method = "match"
			renderVerifyMatch(out, a.Name, localPath,
				fmt.Sprintf("%s   match", rightPadSize(info.Size(), 10)),
				localHash, nameWidth, termWidth)
		} else {
			mismatch++
			ar.Method = "mismatch"
			ar.ErrorMsg = fmt.Sprintf("SHA-256 mismatch: local=%s published=%s", localHash, a.SHA256)
			renderVerifyMismatch(out, a.Name, nameWidth,
				verifySide{label: "local", uri: localPath, size: info.Size(), hash: localHash},
				verifySide{label: "published", uri: pubLabel, size: a.Size, hash: a.SHA256},
			)
		}
		result.Assets = append(result.Assets, ar)
	}

	out.Plain("")

	switch {
	case opErrors > 0:
		result.Status = "error"
		result.Error = fmt.Sprintf("%d asset(s) could not be checked", opErrors)
		out.Summary("FAILED: %d could not be checked, %d mismatch, %d missing locally (%d matched).",
			opErrors, mismatch, missing, matched)
		out.CommandResult(result)
		return &ExitError{Code: cob.ExitError}
	case mismatch > 0 || missing > 0:
		result.Status = "mismatch"
		result.Error = fmt.Sprintf("%d mismatch, %d missing locally", mismatch, missing)
		out.Summary("FAILED: %d mismatch, %d missing locally (%d matched).",
			mismatch, missing, matched)
		out.CommandResult(result)
		return &ExitError{Code: cob.ExitMismatch}
	}
	out.Summary("OK: %d files match the published version byte-for-byte.", matched)
	return out.CommandResult(result)
}

func runVerifyManifest(ctx context.Context, cfg *Config, manifestPath, versionFlag string, deep bool) error {
	out := newWriter(cfg)
	defer out.Close()
	ctx, cancel := interruptable(ctx, out)
	defer cancel()

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

	// Pub size lookup so mismatch rows can show both sides' sizes.
	pubSize := make(map[string]int64)
	if pub, perr := registry.ListAssets(ctx, coords); perr == nil {
		for _, a := range pub {
			pubSize[a.Name] = a.Size
		}
	}

	out.Header("Verifying %s/%s@%s against %s", m.Namespace, m.Package, version, manifestPath)
	out.Plain("  (hashing each manifest source; comparing to the corresponding published asset)")
	out.Plain("")

	result := &cob.CommandResult{
		Command:    "verify",
		Package:    fmt.Sprintf("%s/%s@%s", m.Namespace, m.Package, version),
		Repository: fmt.Sprintf("%s/%s", m.Domain, m.Repository),
		Status:     "ok",
	}
	fillClientMeta(ctx, client, result)

	names := make([]string, 0, len(cmps))
	for _, c := range cmps {
		names = append(names, c.Name)
	}
	nameWidth := pickNameColumnWidth(names)
	termWidth := output.TerminalWidth()
	pubLabel := fmt.Sprintf("%s/%s/%s/%s@%s",
		m.Domain, m.Repository, m.Namespace, m.Package, version)

	var failures, opErrors, unverified, extra int
	for _, c := range cmps {
		ar := cob.AssetResult{Name: c.Name, Source: c.Source, SHA256: c.SrcSHA}
		switch {
		case !c.InManifest && c.InPublished:
			extra++
			ar.Method = "extra"
			renderVerifySkipped(out, c.Name, "(published, not in manifest)", nameWidth, nil)
		case c.Err != nil:
			opErrors++
			ar.SetError(c.Err)
			renderVerifySingleFail(out, c.Name, "could not check", nameWidth,
				[]verifyDetail{
					{"source", c.Source},
					{"error", c.Err.Error()},
				}, termWidth)
		case !c.InPublished:
			failures++
			ar.Method = "missing"
			renderVerifySingleFail(out, c.Name,
				fmt.Sprintf("not published in @%s", version), nameWidth,
				[]verifyDetail{
					{"source", c.Source},
					{"sha256", c.SrcSHA},
				}, termWidth)
		case c.OriginDrift:
			failures++
			ar.Method = "drift"
			renderVerifySingleFail(out, c.Name,
				"S3 source changed since publish (etag/version differs from provenance)",
				nameWidth, []verifyDetail{{"source", c.Source}}, termWidth)
		case c.SrcSHA == "":
			unverified++
			ar.Method = "unverified"
			renderVerifySkipped(out, c.Name,
				"unverified — no checksum/provenance (use --deep to hash)",
				nameWidth, []verifyDetail{{"source", c.Source}})
		case strings.EqualFold(c.SrcSHA, c.PubSHA):
			// hex SHA-256 is case-insensitive — different sources/SDKs
			// return upper- vs lower-hex; EqualFold avoids a spurious
			// mismatch.
			ar.Method = "match(" + c.SrcFrom + ")"
			renderVerifyMatch(out, c.Name, c.Source,
				fmt.Sprintf("match(%s)", c.SrcFrom), c.SrcSHA, nameWidth, termWidth)
		default:
			failures++
			ar.Method = "mismatch"
			ar.ErrorMsg = fmt.Sprintf("SHA-256 mismatch: source=%s published=%s", c.SrcSHA, c.PubSHA)
			renderVerifyMismatch(out, c.Name, nameWidth,
				verifySide{label: "source", uri: c.Source, hash: c.SrcSHA},
				verifySide{label: "published", uri: pubLabel, size: pubSize[c.Name], hash: c.PubSHA},
			)
		}
		result.Assets = append(result.Assets, ar)
	}

	out.Plain("")

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
	defer out.Close()
	ctx, cancel := interruptable(ctx, out)
	defer cancel()

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
	pubSize := make(map[string]int64, len(assets))
	for _, a := range assets {
		pubSHA[a.Name] = a.SHA256
		pubSize[a.Name] = a.Size
	}

	out.Header("Self-verifying %s/%s@%s in %s/%s against recorded provenance",
		coords.Namespace, coords.Package, coords.Version, coords.Domain, coords.Repository)
	renderChain(out, prov, nil)
	renderOrigins(out, prov, "")
	out.Plain("")

	result := &cob.CommandResult{
		Command:    "verify",
		Package:    fmt.Sprintf("%s/%s@%s", coords.Namespace, coords.Package, coords.Version),
		Repository: fmt.Sprintf("%s/%s", coords.Domain, coords.Repository),
		Status:     "ok",
	}
	fillClientMeta(ctx, client, result)

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
		ar := cob.AssetResult{Name: e.Asset, Source: e.Source, SHA256: e.SHA256, Size: e.Size}
		cur, ok := pubSHA[e.Asset]
		switch {
		case !ok:
			failures++
			ar.Method = "missing"
			renderVerifySingleFail(out, e.Asset,
				"recorded in provenance but not in the published version", nameWidth,
				[]verifyDetail{
					{"recorded", fmt.Sprintf("%s   %s", rightPadSize(e.Size, 10), e.SHA256)},
				}, termWidth)
		case strings.EqualFold(cur, e.SHA256):
			// hex SHA-256 case-insensitive — see compareManifestToPublished.
			ar.Method = "match(provenance)"
			renderVerifyMatch(out, e.Asset, "",
				fmt.Sprintf("%s   match(provenance)", rightPadSize(e.Size, 10)),
				e.SHA256, nameWidth, termWidth)
		default:
			failures++
			ar.Method = "altered"
			ar.ErrorMsg = fmt.Sprintf("altered since publish: recorded=%s published=%s", e.SHA256, cur)
			renderVerifyMismatch(out, e.Asset, nameWidth,
				verifySide{label: "recorded", uri: "(provenance)", size: e.Size, hash: e.SHA256},
				verifySide{label: "published", uri: pubLabel, size: pubSize[e.Asset], hash: cur},
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
		out.CommandResult(result)
		return &ExitError{Code: cob.ExitMismatch}
	}
	out.Summary("OK: every recorded asset still matches; chain intact.")
	return out.CommandResult(result)
}

// pickNameColumnWidth picks an alignment column width from the longest
// asset name in the set, capped to keep absurdly long names from
// pushing the rest of every row to the right. Padding is purely visual
// — names longer than the cap still render in full, just unaligned.
func pickNameColumnWidth(names []string) int {
	const minCol, maxCol = 12, 40
	w := minCol
	for _, n := range names {
		if l := len(n); l > w {
			w = l
		}
	}
	if w > maxCol {
		w = maxCol
	}
	return w
}

// extractAssetNames pulls the names out of a published-asset list,
// skipping the provenance asset. Convenience for pickNameColumnWidth.
func extractAssetNames(pub []cob.AssetSummary) []string {
	out := make([]string, 0, len(pub))
	for _, a := range pub {
		if a.Name == cob.ProvenanceFile {
			continue
		}
		out = append(out, a.Name)
	}
	return out
}

// verifySide is one half of a mismatch row — the bytes we hashed (or
// listed) on one side of the comparison. Used by renderVerifyMismatch
// so the same renderer works for dir mode (local vs published),
// manifest mode (source vs published), and coords mode (recorded vs
// published).
type verifySide struct {
	label string // "local", "source", "recorded", "published"
	uri   string // path, S3/CA URI, or coords string
	size  int64  // 0 if unknown
	hash  string // full SHA-256 (lowercase hex)
}

// verifyLabelWidth is the column the detail labels (sha256, source,
// path) line up to. Wide enough for "published" without truncating.
const verifyLabelWidth = 10

// renderVerifyMatch prints a "✓ matched" row. If the compact one-line
// form would fit in the current terminal, use it; otherwise drop the
// URI to its own detail line so wrapping doesn't fragment the row
// into a soup of half-words. The full SHA-256 always gets its own
// line so an operator can string-compare against `sha256sum` output.
func renderVerifyMatch(out *output.Writer, name, uri, status, hash string, nameWidth, termWidth int) {
	compact := fmt.Sprintf("  ✓ %s  %s   %s", padRight(name, nameWidth), uri, status)
	if uri == "" || len(compact) <= termWidth {
		// Compact form fits (or there's no URI to bother with) — use it.
		out.Plain("%s", compact)
		if hash != "" {
			out.Plain("      %-*s %s", verifyLabelWidth, "sha256", hash)
		}
		return
	}
	// Expanded form: status stays on the header, URI drops to its own
	// labeled line. Reading top-to-bottom remains "this asset → these
	// facts about it."
	out.Plain("  ✓ %s  %s", padRight(name, nameWidth), status)
	out.Plain("      %-*s %s", verifyLabelWidth, "source", uri)
	if hash != "" {
		out.Plain("      %-*s %s", verifyLabelWidth, "sha256", hash)
	}
}

// renderVerifyMismatch prints an "✗ mismatch" row with both sides laid
// out symmetrically. Each side gets its own size+URI line and hash
// line — so no matter how wide the terminal is, the row is parseable
// by eye and the hash is never wrapped mid-string.
func renderVerifyMismatch(out *output.Writer, name string, nameWidth int, left, right verifySide) {
	out.Plain("  ✗ %s  mismatch", padRight(name, nameWidth))
	for _, side := range []verifySide{left, right} {
		out.Plain("      %-*s %s   %s", verifyLabelWidth, side.label, rightPadSize(side.size, 10), side.uri)
		out.Plain("                 %s", side.hash)
	}
}

// renderVerifySingleFail prints a one-sided failure (missing locally,
// not-published, drift, op-error) — the asset has only one side worth
// describing, so layout collapses to a header + a detail line.
func renderVerifySingleFail(out *output.Writer, name, summary string, nameWidth int, details []verifyDetail, termWidth int) {
	out.Plain("  ✗ %s  %s", padRight(name, nameWidth), summary)
	for _, d := range details {
		out.Plain("      %-*s %s", verifyLabelWidth, d.label, d.value)
	}
}

// renderVerifySkipped prints a "⊘ skipped" / "⊘ extra" / "⊘ unverified"
// row — non-fatal states that still deserve a context line so the
// operator knows why nothing was checked.
func renderVerifySkipped(out *output.Writer, name, summary string, nameWidth int, details []verifyDetail) {
	out.Plain("  ⊘ %s  %s", padRight(name, nameWidth), summary)
	for _, d := range details {
		out.Plain("      %-*s %s", verifyLabelWidth, d.label, d.value)
	}
}

// verifyDetail is one label/value line on a multi-line verify row.
type verifyDetail struct {
	label string
	value string
}

// fileSHA256 streams a file through sha256 and returns the lowercase
// hex digest. Used by directory mode to hash local files in constant
// memory, regardless of how large any single file is.
func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// padRight pads s with spaces to width characters; if s is already
// wider, returns it unmodified (we'd rather a row wrap than truncate
// the asset name a user is trying to read).
func padRight(s string, width int) string {
	if len(s) >= width {
		return s
	}
	return s + strings.Repeat(" ", width-len(s))
}

// rightPadSize formats a byte count and right-pads it to width so a
// column of sizes aligns on the unit (KB/MB/GB) rather than the digits.
func rightPadSize(n int64, width int) string {
	s := output.FormatSize(n)
	if len(s) >= width {
		return s
	}
	return strings.Repeat(" ", width-len(s)) + s
}

func short(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}
