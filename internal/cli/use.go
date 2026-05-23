package cli

import (
	"github.com/spf13/cobra"

	"github.com/jmurray2011/cob/internal/cob"

	"github.com/jmurray2011/cob/internal/cliutil"
)

// newUseCmd implements `cob use` — set, show, or clear the current
// package. Pattern is borrowed from kubectl config use-context / gh
// repo set-default / aws configure: tools that take a coordinate in
// every command save the typing by maintaining a "current" pointer
// for interactive sessions.
//
// Read-only commands (log, diff <coords>, pull <coords>, manifest,
// resolve) fall back to this when no positional argument is given;
// destructive commands (rm, publish, promote) still require explicit
// coordinates — see cliutil.PackageOverride for the rationale.
func newUseCmd(cfg *cliutil.Config) *cobra.Command {
	var (
		flagGlobal bool
		flagShow   bool
		flagClear  bool
	)

	cmd := &cobra.Command{
		Use:   "use [<coordinates>]",
		Short: "Set, show, or clear the current package (used by read-only commands when no positional arg given)",
		Long: "Saves a coordinate string as the 'current package' so subsequent " +
			"read-only commands can be run without retyping it. The pattern " +
			"matches kubectl's current context: cob use sets it, every later " +
			"command picks it up.\n\n" +
			"Default storage is project-local: ./.cob/current (with a .gitignore " +
			"dropped in alongside on creation, so a local current package " +
			"doesn't leak into git). The --global flag writes ~/.config/cob/current " +
			"instead.\n\n" +
			"Resolution precedence when a read-only command sees no positional " +
			"arg: --package flag > COB_PACKAGE_COORDS env > cwd .cob/current " +
			"(walking up to filesystem root) > ~/.config/cob/current. The first " +
			"match wins; subsequent sources are ignored.\n\n" +
			"Destructive commands (rm, publish, promote) deliberately ignore the " +
			"current package — they require explicit coordinates so an operator " +
			"who cd's into the wrong project can't silently target last week's " +
			"`cob use` setting.",
		Example: `  # set the project-local current package (interactive workflow)
  cob use acme/prod/tools/my-app@2.1.0
  cob log                                # picks it up
  cob diff                               # picks it up
  cob pull --output ./assets/            # picks it up

  # show what's currently set
  cob use --show

  # clear it
  cob use --clear

  # set a global default (across all directories)
  cob use --global acme/dev/tools/my-app@latest`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cliutil.NewWriter(cfg)
			defer out.Close()

			switch {
			case flagClear:
				if len(args) > 0 {
					return cliutil.Fail(out, "use", cob.ExitError, "--clear takes no positional argument")
				}
				path, err := cliutil.ClearCurrentPackage(flagGlobal)
				if err != nil {
					return cliutil.Fail(out, "use", cob.ExitError, "%s", err)
				}
				out.Plain("Cleared %s.", path)
				return nil

			case flagShow || len(args) == 0:
				// `cob use` with no args is `cob use --show` (a sane
				// default for an inspection command — kubectl current-context
				// behaves the same way).
				coords, src, err := cliutil.CurrentPackage(cfg)
				if err != nil {
					return cliutil.Fail(out, "use", cob.ExitError, "%s", err)
				}
				if coords == "" {
					out.Plain("No current package set. Run `cob use <coordinates>` to set one.")
					return nil
				}
				out.Plain("%s  (from %s)", coords, src)
				return nil

			default:
				path, err := cliutil.SetCurrentPackage(args[0], flagGlobal)
				if err != nil {
					return cliutil.Fail(out, "use", cob.ExitError, "%s", err)
				}
				out.Plain("Set current package to %s.", args[0])
				out.Plain("Stored at %s.", path)
				return nil
			}
		},
	}
	cmd.Flags().BoolVar(&flagGlobal, "global", false, "Operate on ~/.config/cob/current instead of ./.cob/current")
	cmd.Flags().BoolVar(&flagShow, "show", false, "Print the current package and where it came from")
	cmd.Flags().BoolVar(&flagClear, "clear", false, "Remove the current-package pointer (idempotent — missing file is fine)")
	return cmd
}
