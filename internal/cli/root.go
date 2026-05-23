package cli

import (
	"github.com/spf13/cobra"

	"github.com/jmurray2011/cob/internal/cliutil"
)

// NewRootCmd creates the top-level cob command. It owns the per-process
// *cliutil.Config; subcommand constructors close over it so a run* function reads
// configuration through an argument rather than a package global. The
// cliutil.BuildInfo flows into both cliutil.Config.Version (a plain string for chain
// stamping) and cliutil.Config.Build (the structured shape `cob version --json`
// emits).
func NewRootCmd(build cliutil.BuildInfo) *cobra.Command {
	cfg := &cliutil.Config{Version: build.Version, Build: build}

	root := &cobra.Command{
		Use:           "cob",
		Short:         "Assemble CodeArtifact packages from remote sources",
		Long:          "cob assembles AWS CodeArtifact packages from S3, other CodeArtifact packages, and local files. No local artifacts required.",
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRun: func(cmd *cobra.Command, args []string) {
			cliutil.ApplyEnvFallbacks(cmd, cfg)
		},
	}

	root.PersistentFlags().StringVar(&cfg.Profile, "profile", "", "AWS profile")
	root.PersistentFlags().StringVar(&cfg.Region, "region", "", "AWS region")
	root.PersistentFlags().BoolVar(&cfg.JSON, "json", false, "Machine-readable JSON output")
	root.PersistentFlags().BoolVarP(&cfg.Quiet, "quiet", "q", false, "Suppress headers, summaries, and progress (errors still print)")
	root.PersistentFlags().BoolVar(&cfg.Debug, "debug", false, "Log AWS API responses/retries to stderr")
	root.PersistentFlags().StringVar(&cfg.TmpDir, "tmpdir", "", "Directory for streaming spill files (default: $TMPDIR)")
	root.PersistentFlags().BoolVar(&cfg.NoTUI, "no-tui", false, "Force line-stream output even on a TTY (also: COB_TUI=0)")
	root.PersistentFlags().DurationVar(&cfg.Timeout, "timeout", 0, "Deadline for long-running ops (pull/publish/promote/diff); e.g. 30m. 0 = no deadline (also: COB_TIMEOUT)")

	root.AddCommand(
		newPublishCmd(cfg),
		newPullCmd(cfg),
		newPromoteCmd(cfg),
		newLsCmd(cfg),
		newResolveCmd(cfg),
		newDiffCmd(cfg),
		newManifestCmd(cfg),
		newTreeCmd(cfg),
		newRmCmd(cfg),
		newLogCmd(cfg),
		newInitCmd(cfg),
		newVersionCmd(cfg),
	)

	// --version prints the same parenthesized "v (commit, go, time)" line
	// the version subcommand emits in human mode, so the two paths agree.
	root.Version = build.HumanString()
	return root
}
