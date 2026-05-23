package cli

import (
	"os"
	"strconv"

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
	// Version is the cob build version, recorded into provenance documents.
	Version string
}

// applyEnvFallbacks merges COB_* env vars into cfg for any persistent flag
// the user didn't pass explicitly. Mirrors the precedence the README
// promises: CLI flag > COB_* env > zero value.
func applyEnvFallbacks(cmd *cobra.Command, cfg *Config) {
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
