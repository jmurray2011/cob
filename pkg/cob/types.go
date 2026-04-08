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

// AssetResult holds the outcome of a single asset transfer.
type AssetResult struct {
	Name       string        `json:"name"`
	Source     string        `json:"source,omitempty"`
	Size       int64         `json:"size"`
	SHA256     string        `json:"sha256"`
	Method     string        `json:"method"` // "buffered" or "skipped"
	Duration   time.Duration `json:"-"`
	DurationMs int64         `json:"duration_ms"`
	Error      error         `json:"-"`
	ErrorMsg   string        `json:"error,omitempty"`
}

// SetError sets both the error and its string representation for JSON.
func (r *AssetResult) SetError(err error) {
	r.Error = err
	if err != nil {
		r.ErrorMsg = err.Error()
	}
}

// CommandResult is the top-level JSON output for any command.
type CommandResult struct {
	Command    string        `json:"command"`
	Package    string        `json:"package"`
	Repository string        `json:"repository"`
	Assets     []AssetResult `json:"assets"`
	TotalSize  int64         `json:"total_size"`
	DurationMs int64         `json:"duration_ms"`
	Status     string        `json:"status"` // "ok" or "error"
	Error      string        `json:"error,omitempty"`
}

// PackageSummary is returned by list operations at the repo level.
type PackageSummary struct {
	Namespace    string `json:"namespace"`
	Package      string `json:"package"`
	LatestVersion string `json:"latest_version"`
	VersionCount int    `json:"version_count"`
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
	Status     string `json:"status"` // "Published" or "-"
}

// FormatGeneric is the CodeArtifact package format used by cob.
const FormatGeneric = "generic"

// Exit codes.
const (
	ExitOK       = 0
	ExitError    = 1
	ExitNotFound = 2
	ExitConflict = 3
)
