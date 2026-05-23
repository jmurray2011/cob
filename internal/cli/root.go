package cli

import (
	"github.com/spf13/cobra"
)

// NewRootCmd creates the top-level cob command. It owns the per-process
// *Config; subcommand constructors close over it so a run* function reads
// configuration through an argument rather than a package global.
func NewRootCmd(version string) *cobra.Command {
	cfg := &Config{Version: version}

	root := &cobra.Command{
		Use:           "cob",
		Short:         "Assemble CodeArtifact packages from remote sources",
		Long:          "cob assembles AWS CodeArtifact packages from S3, other CodeArtifact packages, and local files. No local artifacts required.",
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRun: func(cmd *cobra.Command, args []string) {
			applyEnvFallbacks(cmd, cfg)
		},
	}

	root.PersistentFlags().StringVar(&cfg.Profile, "profile", "", "AWS profile")
	root.PersistentFlags().StringVar(&cfg.Region, "region", "", "AWS region")
	root.PersistentFlags().BoolVar(&cfg.JSON, "json", false, "Machine-readable JSON output")
	root.PersistentFlags().BoolVarP(&cfg.Quiet, "quiet", "q", false, "Suppress headers, summaries, and progress (errors still print)")
	root.PersistentFlags().BoolVar(&cfg.Debug, "debug", false, "Log AWS API responses/retries to stderr")
	root.PersistentFlags().StringVar(&cfg.TmpDir, "tmpdir", "", "Directory for streaming spill files (default: $TMPDIR)")

	root.AddCommand(
		newPublishCmd(cfg),
		newPullCmd(cfg),
		newPromoteCmd(cfg),
		newLsCmd(cfg),
		newResolveCmd(cfg),
		newValidateCmd(cfg),
		newVerifyCmd(cfg),
		newDiffCmd(cfg),
		newManifestCmd(cfg),
	)

	root.Version = version
	return root
}
