package cli

import (
	"os"

	"github.com/spf13/cobra"
)

var (
	flagProfile string
	flagRegion  string
	flagJSON    bool
)

// NewRootCmd creates the top-level cob command.
func NewRootCmd(version string) *cobra.Command {
	root := &cobra.Command{
		Use:   "cob",
		Short: "Assemble CodeArtifact packages from remote sources",
		Long:  "cob assembles AWS CodeArtifact packages from S3, other CodeArtifact packages, and local files. No local artifacts required.",
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
		},
	}

	root.PersistentFlags().StringVar(&flagProfile, "profile", "", "AWS profile")
	root.PersistentFlags().StringVar(&flagRegion, "region", "", "AWS region")
	root.PersistentFlags().BoolVar(&flagJSON, "json", false, "Machine-readable JSON output")

	root.AddCommand(
		newPublishCmd(),
		newPullCmd(),
		newPromoteCmd(),
		newLsCmd(),
		newResolveCmd(),
	)

	root.Version = version

	return root
}
