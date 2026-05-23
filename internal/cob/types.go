package cob

import "time"

// PackageCoordinates identifies a package in CodeArtifact.
type PackageCoordinates struct {
	Domain     string
	Repository string
	Namespace  string
	Package    string
	Version    string
}

// AssetResultKind discriminates what an AssetResult represents. It exists
// so a --json consumer can switch on a stable namespace before reading
// Method, rather than memorizing which string values appear in which
// command context (the original concern: "spilled" is a transfer outcome,
// "match" is a comparison outcome, "exists" is a lint outcome, and a
// consumer used to have to grep all three vocabularies).
//
// Each kind has its own set of Method constants (TransferXxx, CompareXxx,
// LintXxx). Parametric methods like "match(provenance)" / "match(source)"
// stay as concatenated strings — they're already structured via the
// suffix.
type AssetResultKind string

const (
	// KindTransfer — publish, pull, promote real-asset transfers.
	// Methods: TransferSpilled, TransferSkipped.
	KindTransfer AssetResultKind = "transfer"
	// KindCompare — diff manifest/self-check/dir/versions modes.
	// Methods: CompareMatch* / CompareMismatch / CompareAdded /
	// CompareRemoved / CompareChanged / CompareSame / CompareMissing /
	// CompareMissingLocal / CompareAltered / CompareUnknown.
	KindCompare AssetResultKind = "compare"
	// KindLint — diff lint mode (offline schema/URI/local-file checks).
	// Methods: LintExists, LintSyntaxS3, LintSyntaxCA, LintSyntax.
	KindLint AssetResultKind = "lint"
	// KindDryRun — publish/promote --dry-run preview rows.
	// Methods: DryRunPreview.
	KindDryRun AssetResultKind = "dry-run"
)

// Method constants — grouped by AssetResultKind. Producers should use
// these instead of bare string literals so a typo lands at compile time
// (when the constant doesn't exist) instead of at JSON-parse time.
const (
	// Transfer kind.
	TransferSpilled = "spilled" // streamed from source → spill → CodeArtifact
	TransferSkipped = "skipped" // already present in the version (--resume)

	// Compare kind. "match" alone is the dir-mode and version-vs-version
	// case; the parametric "match(<srcFrom>)" forms are produced by the
	// manifest/self-check modes — kept as concatenated strings because
	// the suffix names which trust path produced the match.
	CompareMatch        = "match"
	CompareMismatch     = "mismatch"
	CompareAdded        = "added"         // in manifest, not published
	CompareRemoved      = "removed"       // published, not in manifest
	CompareChanged      = "changed"       // differs from published
	CompareSame         = "same"          // version-vs-version: bytes identical
	CompareMissing      = "missing"       // recorded in provenance but not in published version
	CompareMissingLocal = "missing-local" // dir-mode: not on disk
	CompareAltered      = "altered"       // self-check: published hash diverged from provenance record
	CompareUnknown      = "unknown"       // no checksum/provenance/--deep — couldn't compare

	// Lint kind.
	LintExists   = "exists"     // local file: stat'd and is a regular file
	LintSyntaxS3 = "syntax(s3)" // s3:// URI parsed; existence not checked
	LintSyntaxCA = "syntax(ca)" // ca:// URI parsed; existence not checked
	LintSyntax   = "syntax"     // initial state before classifier runs

	// DryRun kind.
	DryRunPreview = "dry-run"
)

// AssetResult holds the outcome of a single asset operation — transfer,
// compare, lint, or dry-run preview. The Kind field discriminates which
// vocabulary Method draws from (see AssetResultKind constants above).
// Older v0.x JSON consumers that didn't read Kind still work: the field
// is additive and Method retains the same string values it always had.
type AssetResult struct {
	Name       string          `json:"name"`
	Kind       AssetResultKind `json:"kind,omitempty"`
	Source     string          `json:"source,omitempty"`
	Size       int64           `json:"size"`
	SHA256     string          `json:"sha256"`
	Method     string          `json:"method"`
	DurationMs int64           `json:"duration_ms"`
	Error      error           `json:"-"`
	ErrorMsg   string          `json:"error,omitempty"`
}

// SetError sets both the error and its string representation for JSON.
func (r *AssetResult) SetError(err error) {
	r.Error = err
	if err != nil {
		r.ErrorMsg = err.Error()
	}
}

// CommandResult is the top-level JSON output for any command.
//
// Region and Actor are populated for every command that successfully
// dialed an AWS client — audit pipelines need to know which account/role
// executed the operation and in which region without grepping the chain
// or scanning per-asset Origin records. Both are best-effort: a Region
// might be unknown if the SDK couldn't resolve one; an Actor with empty
// ARN/UserID indicates STS GetCallerIdentity failed and is rendered as
// "unknown" by callers. The two are also recorded into provenance for
// publish/promote — this exposes the same information for read commands
// (verify/diff/ls/etc.) where there's no chain event to record.
type CommandResult struct {
	Command    string        `json:"command"`
	Package    string        `json:"package"`
	Repository string        `json:"repository"`
	Region     string        `json:"region,omitempty"`
	Actor      *Actor        `json:"actor,omitempty"`
	Assets     []AssetResult `json:"assets"`
	TotalSize  int64         `json:"total_size"`
	DurationMs int64         `json:"duration_ms"`
	Status     string        `json:"status"` // "ok", "error", "mismatch" (verify), "drift" (diff), "aborted" (declined)
	Error      string        `json:"error,omitempty"`
	// Warnings carries every warning emitted during the command so a --json
	// consumer sees them too — warnings go to stderr, never the stdout JSON.
	Warnings []string `json:"warnings,omitempty"`
}

// PackageSummary is returned by list operations at the repo level.
type PackageSummary struct {
	Namespace     string `json:"namespace"`
	Package       string `json:"package"`
	LatestVersion string `json:"latest_version"`
	VersionCount  int    `json:"version_count"`
}

// VersionSummary is returned by list operations at the package level.
type VersionSummary struct {
	Version   string    `json:"version"`
	Assets    int       `json:"assets"`
	Published time.Time `json:"published"`
}

// AssetSummary is returned by list operations at the version level.
type AssetSummary struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// PromotionStatus shows where a version exists across repos.
type PromotionStatus struct {
	Repository string `json:"repository"`
	Version    string `json:"version"`
	// Status is the CodeArtifact version status as observed in this repo:
	// "Published", "Unfinished", "Unlisted", "Archived", etc., or "-" when
	// the version isn't present and "?" when the check itself failed.
	Status string `json:"status"`
}

// FormatGeneric is the CodeArtifact package format used by cob.
const FormatGeneric = "generic"

// Exit codes.
//
// ExitInterrupted follows the POSIX shell convention (128 + SIGINT/2)
// so a CI step can distinguish "the user canceled" from "the operation
// failed" by exit code alone, the same way a Ctrl-C'd shell pipeline
// would propagate.
const (
	ExitOK          = 0
	ExitError       = 1 // the command could not be completed
	ExitNotFound    = 2 // package/version/asset does not exist
	ExitConflict    = 3 // version already exists (use --force)
	ExitMismatch    = 4 // the check ran and found a difference: verify SHA mismatch, diff drift
	ExitInterrupted = 130
)
