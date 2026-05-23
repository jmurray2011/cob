package cli

import (
	"fmt"
	"runtime"

	"github.com/spf13/cobra"

	"github.com/jmurray2011/cob/internal/cliutil"
)

func newVersionCmd(cfg *cliutil.Config) *cobra.Command {
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
			out := cliutil.NewWriter(cfg)
			defer out.Close()
			// Fill the runtime fields lazily: they don't change per build,
			// but keeping them out of cfg.Build means tests can construct
			// a pinned cliutil.BuildInfo without needing to spoof
			// runtime.GOOS too.
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
