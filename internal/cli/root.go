package cli

import (
	"os"

	"github.com/spf13/cobra"
)

var (
	flagProfile string
	flagRegion  string
	flagJSON    bool
	flagDebug   bool
	flagQuiet   bool
	flagTmpDir  string

	// buildVersion is the cob version, recorded in provenance documents.
	buildVersion string
)

// NewRootCmd creates the top-level cob command.
func NewRootCmd(version string) *cobra.Command {
	buildVersion = version
	root := &cobra.Command{
		Use:           "cob",
		Short:         "Assemble CodeArtifact packages from remote sources",
		Long:          "cob assembles AWS CodeArtifact packages from S3, other CodeArtifact packages, and local files. No local artifacts required.",
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRun: func(cmd *cobra.Command, args []string) {
			// CLI flags > COB_* env vars > AWS_* env vars.
			// Only apply env fallback if the flag wasn't explicitly set.
			// Use Changed() so that --profile "" intentionally clears the value.
			if !cmd.Flags().Changed("profile") {
				if v := os.Getenv("COB_PROFILE"); v != "" {
					flagProfile = v
				}
			}
			if !cmd.Flags().Changed("region") {
				if v := os.Getenv("COB_REGION"); v != "" {
					flagRegion = v
				}
			}
			if !cmd.Flags().Changed("tmpdir") {
				if v := os.Getenv("COB_TMPDIR"); v != "" {
					flagTmpDir = v
				}
			}
		},
	}

	root.PersistentFlags().StringVar(&flagProfile, "profile", "", "AWS profile")
	root.PersistentFlags().StringVar(&flagRegion, "region", "", "AWS region")
	root.PersistentFlags().BoolVar(&flagJSON, "json", false, "Machine-readable JSON output")
	root.PersistentFlags().BoolVarP(&flagQuiet, "quiet", "q", false, "Suppress headers, summaries, and progress (errors still print)")
	root.PersistentFlags().BoolVar(&flagDebug, "debug", false, "Log AWS API responses/retries to stderr")
	root.PersistentFlags().StringVar(&flagTmpDir, "tmpdir", "", "Directory for streaming spill files (default: $TMPDIR)")

	root.AddCommand(
		newPublishCmd(),
		newPullCmd(),
		newPromoteCmd(),
		newLsCmd(),
		newResolveCmd(),
		newValidateCmd(),
		newVerifyCmd(),
		newDiffCmd(),
		newManifestCmd(),
	)

	root.Version = version

	return root
}
