package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"strings"

	"github.com/jmurray2011/cob/internal/cob"
	"github.com/jmurray2011/cob/internal/output"
)

// diff_render.go houses the per-row rendering shared across diff's
// modes (manifest, version-vs-version, self-check, dir-vs-published,
// lint). Each mode emits the same five shapes — match, mismatch,
// single-sided failure, skipped/extra, lint-status — through these
// helpers so the output stays consistent regardless of which mode the
// operator landed in.

// diffSide is one half of a mismatch row — the bytes hashed (or
// listed) on one side of the comparison. Used by renderDiffMismatch
// so the same renderer works for manifest mode (source vs published),
// dir mode (local vs published), version-vs-version (left vs right),
// and self-check (recorded vs live).
type diffSide struct {
	label string // "source", "local", "recorded", "published", "left", "right"
	uri   string // path, S3/CA URI, or coords string
	size  int64  // 0 if unknown
	hash  string // full SHA-256 (lowercase hex)
}

// diffDetail is one label/value line on a multi-line diff row.
type diffDetail struct {
	label string
	value string
}

// diffLabelWidth is the column the detail labels (sha256, source,
// path) line up to. Wide enough for "published" without truncating.
const diffLabelWidth = 10

// renderDiffMatch prints a "✓ matched" row.
//
// Default (terse): one line, glyph + name + size + method. Audit info
// (URI, full SHA-256) is omitted — for 15 assets where 12 match,
// dumping 12 SHAs buries the 3 that didn't. The "did anything change?"
// question that diff mostly answers doesn't need the per-row hash.
//
// --verbose adds the source URI and the full SHA-256 as continuation
// lines under each match. URI drops to its own labeled line on narrow
// terminals so wrapping doesn't fragment the row.
//
// Mismatch rows always get the full detail (renderDiffMismatch) —
// that's where the hash actually matters.
func renderDiffMatch(out *output.Writer, name, uri, status, hash string, size int64, nameWidth, termWidth int, verbose bool) {
	out.Plain("  ✓ %s  %s   %s",
		padRight(name, nameWidth), rightPadSize(size, 10), status)
	if !verbose {
		return
	}
	if uri != "" {
		out.Plain("      %-*s %s", diffLabelWidth, "source", uri)
	}
	if hash != "" {
		out.Plain("      %-*s %s", diffLabelWidth, "sha256", hash)
	}
}

// renderDiffMismatch prints an "✗ mismatch" row with both sides laid
// out symmetrically. Each side gets its own size+URI line and hash
// line — so no matter how wide the terminal is, the row is parseable
// by eye and the hash is never wrapped mid-string. Size 0 is read as
// "unknown" (e.g. remote source we never measured) and rendered as a
// blank rather than a misleading "0 B".
func renderDiffMismatch(out *output.Writer, name string, nameWidth int, left, right diffSide) {
	out.Plain("  ✗ %s  mismatch", padRight(name, nameWidth))
	for _, side := range []diffSide{left, right} {
		sizeCol := rightPadSize(side.size, 10)
		if side.size == 0 {
			sizeCol = strings.Repeat(" ", 10) // unknown — don't lie with "0 B"
		}
		out.Plain("      %-*s %s   %s", diffLabelWidth, side.label, sizeCol, side.uri)
		out.Plain("                 %s", side.hash)
	}
}

// renderDiffSingleFail prints a one-sided failure (missing locally,
// not-published, drift, op-error) — the asset has only one side worth
// describing, so layout collapses to a header + a detail line.
func renderDiffSingleFail(out *output.Writer, name, summary string, nameWidth int, details []diffDetail, _ int) {
	out.Plain("  ✗ %s  %s", padRight(name, nameWidth), summary)
	for _, d := range details {
		out.Plain("      %-*s %s", diffLabelWidth, d.label, d.value)
	}
}

// renderDiffSkipped prints a "⊘ skipped" / "⊘ extra" / "⊘ unverified"
// row — non-fatal states that still deserve a context line so the
// operator knows why nothing was checked.
func renderDiffSkipped(out *output.Writer, name, summary string, nameWidth int, details []diffDetail) {
	out.Plain("  ⊘ %s  %s", padRight(name, nameWidth), summary)
	for _, d := range details {
		out.Plain("      %-*s %s", diffLabelWidth, d.label, d.value)
	}
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

// fileSHA256 streams a file through sha256 and returns the lowercase
// hex digest. Used by dir mode to hash local files in constant memory,
// regardless of how large any single file is.
//
// Symlink defense: the leaf is opened via openLeafNoFollow, which on unix
// uses O_NOFOLLOW (atomic refusal — even a TOCTOU swap to a symlink
// between the safeJoin check and the open is caught) and on other
// platforms falls back to an Lstat-then-Open check. Hashing a symlinked
// file would let a local symlink (planted earlier, or accidentally
// created with `ln -s`) redirect the integrity check to arbitrary
// content the user didn't intend to compare — the dir-mode contract is
// "the regular file at this basename matches the published asset?".
func fileSHA256(path string) (string, error) {
	f, err := openLeafNoFollow(path)
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

// short returns the first 8 hex chars of a SHA — enough for human
// visual identity, not enough for verification. Used in version-vs-
// version diff's compact "~ name (X -> Y)" rows where both hashes are
// being shown side-by-side for change identification, not for byte
// equality (a full collision-resistant compare already happened to
// reach the "~" rendering path).
func short(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}
