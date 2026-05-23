// Package cliutil holds the shared building blocks every cob subcommand
// needs: the per-process Config, the AWS client / output writer test
// seams, validation + concurrency helpers, and the small utilities that
// keep run* signatures honest about what they depend on.
//
// Lives as a sibling of internal/cli so per-command packages
// (internal/cli/diff, /publish, /promote, /pull) can import it without
// dragging in the rest of the CLI, and so a future non-CLI consumer
// (daemon mode, e.g.) could reuse the same building blocks without
// pulling in cobra wiring.
package cliutil

import (
	"os"
	"strconv"
	"time"

	"github.com/spf13/cobra"
)

// Config is the runtime configuration assembled from cob's persistent flags
// and COB_* environment fallbacks. NewRootCmd owns one *Config; each
// subcommand constructor closes over it, and every run* takes it as the
// second argument after ctx. This replaces the pre-Config package-level
// flag globals — a run* signature now states what it depends on.
//
// Per-command flags (--version, --force, --resume, --output, etc.) stay
// local to their newXxxCmd closures and are not on Config: they belong to
// the command, not the process.
type Config struct {
	Profile, Region, TmpDir string
	JSON, Quiet, Debug      bool
	// NoTUI forces the line-stream renderer even on an interactive TTY —
	// for screen-recording, exotic emulators, or pipelines that want
	// scriptable output without the JSON envelope. Set by --no-tui or
	// COB_TUI=0.
	NoTUI bool
	// Timeout bounds how long a long-running operation (pull, publish,
	// promote, diff dir/manifest/versions) may run before the ambient ctx
	// is canceled. 0 = no deadline (the AWS SDK still has its own per-call
	// timeouts). Set by --timeout or COB_TIMEOUT (a Go duration string
	// like 30m, 90s, 2h). Quick commands (ls, log, version, …) aren't
	// wrapped — they're either fast or pure local, so a deadline there
	// would mostly create confusing aborts.
	Timeout time.Duration
	// Version is the cob build version string, recorded into provenance
	// documents and surfaced via `cob --version`. For richer structured
	// build metadata (commit, time, toolchain) see Build, which the
	// version subcommand renders as JSON.
	Version string
	Build   BuildInfo
}

// BuildInfo carries the version metadata cob can determine from the
// build: the version string (from -ldflags or VCS), the VCS revision
// and timestamp when available, plus runtime details (Go toolchain,
// OS/arch). Surfaced both as the cobra --version line and as
// `cob version --json` for CI consumers that want to pin or detect
// upgrades programmatically.
type BuildInfo struct {
	Version string `json:"version"`
	Commit  string `json:"commit,omitempty"`
	Time    string `json:"time,omitempty"`
	Go      string `json:"go"`
	OS      string `json:"os"`
	Arch    string `json:"arch"`
}

// HumanString renders the parenthesized "version (commit, go, time)" shape
// the cobra --version line and the `cob version` subcommand both use, so
// the two paths stay in lockstep.
func (b BuildInfo) HumanString() string {
	extra := b.Go
	if b.Commit != "" {
		extra = b.Commit + ", " + extra
	}
	if b.Time != "" {
		extra += ", " + b.Time
	}
	if extra != "" {
		return b.Version + " (" + extra + ")"
	}
	return b.Version
}

// ApplyEnvFallbacks merges COB_* env vars into cfg for any persistent flag
// the user didn't pass explicitly. Mirrors the precedence the README
// promises: CLI flag > COB_* env > zero value. Called from root.go's
// PersistentPreRun.
func ApplyEnvFallbacks(cmd *cobra.Command, cfg *Config) {
	// Use Changed() so an explicit "--profile ''" still clears the value;
	// only fall back to the env var when the flag was *not* passed.
	if !cmd.Flags().Changed("profile") {
		if v := os.Getenv("COB_PROFILE"); v != "" {
			cfg.Profile = v
		}
	}
	if !cmd.Flags().Changed("region") {
		if v := os.Getenv("COB_REGION"); v != "" {
			cfg.Region = v
		}
	}
	if !cmd.Flags().Changed("tmpdir") {
		if v := os.Getenv("COB_TMPDIR"); v != "" {
			cfg.TmpDir = v
		}
	}
	if !cmd.Flags().Changed("json") {
		if set, b := envBool("COB_JSON"); set {
			cfg.JSON = b
		}
	}
	if !cmd.Flags().Changed("quiet") {
		if set, b := envBool("COB_QUIET"); set {
			cfg.Quiet = b
		}
	}
	if !cmd.Flags().Changed("debug") {
		if set, b := envBool("COB_DEBUG"); set {
			cfg.Debug = b
		}
	}
	if !cmd.Flags().Changed("timeout") {
		if v := os.Getenv("COB_TIMEOUT"); v != "" {
			if d, err := time.ParseDuration(v); err == nil {
				cfg.Timeout = d
			}
			// Silently ignore an unparseable COB_TIMEOUT — surfacing it
			// from PersistentPreRun would either need to bubble up as an
			// error (intrusive) or fmt.Fprint to stderr (race with the
			// command's own output). The flag form's parser catches
			// typos for anyone who cares enough to set --timeout.
		}
	}
	// COB_TUI maps inversely: COB_TUI=0 means "disable the TUI" (set
	// NoTUI=true). COB_TUI=1 or unset keeps the default. Done after the
	// other env flags so an explicit --no-tui on the CLI still wins.
	if !cmd.Flags().Changed("no-tui") {
		if set, b := envBool("COB_TUI"); set {
			cfg.NoTUI = !b
		}
		// ACCESSIBLE is a cross-tool convention (Charm libs, gh, etc.)
		// for "I'm using a screen reader; please degrade to text". The
		// live TUI's box-drawing and ANSI cursor movement are unusable
		// in that mode — force the stream renderer regardless of TTY
		// detection. Only set NoTUI=true; never unset (a user with both
		// COB_TUI=1 and ACCESSIBLE=1 set still gets the accessible
		// path, which is the conservative choice).
		if set, b := envBool("ACCESSIBLE"); set && b {
			cfg.NoTUI = true
		}
	}
}

// envBool reads a COB_* boolean env var. Returns (set=false) when the var
// is unset or empty; an unparseable non-empty value is treated as true.
func envBool(name string) (set, val bool) {
	v, ok := os.LookupEnv(name)
	if !ok || v == "" {
		return false, false
	}
	if b, err := strconv.ParseBool(v); err == nil {
		return true, b
	}
	return true, true
}
