package cli

import (
	"fmt"
	"runtime"

	"github.com/spf13/cobra"
)

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

// HumanString renders the same parenthesized "version (commit, go, time)"
// shape resolveBuildVersion historically produced for cobra's --version
// line. Kept in this package so the version subcommand and the cobra
// flag stay in lockstep.
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

func newVersionCmd(cfg *Config) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print cob's build version",
		Long: "Prints the cob version, including VCS commit and Go toolchain when " +
			"available. With --json (global flag), emits a structured document " +
			"{version, commit, time, go, os, arch} suitable for CI pin checks. " +
			"The same version string is reachable via `cob --version` for parity " +
			"with other cobra-based tools.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			out := newWriter(cfg)
			defer out.Close()
			// Fill the runtime fields lazily: they don't change per build,
			// but keeping them out of cfg.Build means tests can construct a
			// pinned BuildInfo without needing to spoof runtime.GOOS too.
			bi := cfg.Build
			if bi.Go == "" {
				bi.Go = runtime.Version()
			}
			if bi.OS == "" {
				bi.OS = runtime.GOOS
			}
			if bi.Arch == "" {
				bi.Arch = runtime.GOARCH
			}
			if out.JSON(bi) {
				return nil
			}
			fmt.Fprintln(out.Stdout(), bi.HumanString())
			return nil
		},
	}
}
