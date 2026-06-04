package diff

import (
	"context"
	"os"

	"github.com/spf13/cobra"

	"github.com/jmurray2011/cob/internal/cob"

	"github.com/jmurray2011/cob/internal/cliutil"
)

// diff is cob's only comparison verb. Mode is picked from the
// positional shape — there's no separate `validate`, no separate
// `verify`; lint and integrity checks are diff cases. Each mode lives
// in its own file (lint.go, manifest.go, selfcheck.go, dir.go,
// versions.go); the shared renderers are in render.go and the
// comparison helpers in compare.go. cliutil.ValidateManifest runs
// implicitly inside manifest mode (and at the top of any other
// manifest-consuming command) so a broken manifest short-circuits
// before any AWS work.
//
//	cob diff <manifest>                  → lint only (offline; was `cob validate`)
//	cob diff <manifest> --version X      → manifest sources vs published @X
//	cob diff <coords>                    → self-integrity: provenance vs live
//	cob diff <dir> <coords>              → local files vs published
//	cob diff <coords-A> <coords-B>       → version vs version
func NewCmd(cfg *cliutil.Config) *cobra.Command {
	var (
		flagVersion   string
		flagDeep      bool
		flagVerbose   bool
		flagCheckRefs bool
	)

	cmd := &cobra.Command{
		Use:   "diff <target> [<target>]",
		Short: "Compare bytes — manifest vs published, version vs version, local dir vs published, or lint a manifest",
		Long: "diff is cob's only comparison verb. The mode is picked from the " +
			"positional argument(s); none mutate; exit code is 0 if identical, " +
			"non-zero on any drift or error.\n\n" +
			"Five modes:\n\n" +
			"1. Manifest lint (one arg, file ending .yaml/.yml, no --version): " +
			"schema + URI syntax + local-file existence. Offline. The same " +
			"checks run implicitly at the top of every other manifest-based " +
			"command, so this mode is just \"give me the lint result.\"\n\n" +
			"2. Manifest vs published (one arg, file ending .yaml/.yml, with " +
			"--version): hashes each source and compares to the published " +
			"asset of the same name. Precedence: known checksum → recorded S3 " +
			"origin → cob-provenance.json → --deep download+hash.\n\n" +
			"3. Self-integrity (one arg, coordinates with version): fetches the " +
			"recorded cob-provenance.json and compares each entry's SHA-256 " +
			"to what CodeArtifact currently stores. Prints the chain of " +
			"evidence first. Audit a version with nothing but its coordinates.\n\n" +
			"4. Local dir vs published (two args, first must be a directory): " +
			"for each published asset of <coords>, looks for a local file of " +
			"the same name in <dir> and compares SHA-256. Answers \"do these " +
			"local files match what was published?\"\n\n" +
			"5. Version vs version (two args, both coordinates): compares two " +
			"published versions of the same package. cob-provenance.json is " +
			"excluded (its bytes trivially differ even when the package " +
			"didn't change). Cross-repo same-package is allowed (\"did the " +
			"promote preserve the bytes?\").",
		Example: `  # 1. Lint a manifest (offline)
  cob diff ./my-package.yaml

  # 2. Manifest vs published
  cob diff ./my-package.yaml --version 2.1.0

  # 3. Self-integrity (audit by coordinates)
  cob diff acme/dev/tools/my-app@2.1.0

  # 4. Local dir vs published
  cob diff ~/pulled-dir acme/dev/tools/my-app@2.1.0

  # 5. Version vs version
  cob diff acme/dev/tools/my-app@2.0.0 acme/dev/tools/my-app@2.1.0`,
		Args: cobra.MaximumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return Run(cmd.Context(), cfg, args, flagVersion, flagDeep, flagVerbose, flagCheckRefs)
		},
	}
	cmd.Flags().StringVar(&flagVersion, "version", "", "Package version (manifest mode with --version → online; without → offline lint)")
	cmd.Flags().BoolVar(&flagDeep, "deep", false, "Manifest mode: download and hash sources lacking a checksum (no S3 writes)")
	cmd.Flags().BoolVarP(&flagVerbose, "verbose", "v", false, "Show source URI and full SHA-256 on every row (default: only on mismatches)")
	cmd.Flags().BoolVar(&flagCheckRefs, "check-references", false, "Manifest/self-check modes: probe each chain reference's repository (one VersionStatus per unique repo); surface deleted/unreachable refs as warnings (mirrors `cob log --check-references`)")
	return cmd
}

// Run routes to the right mode by inspecting positional shape and
// filesystem state. Each branch's discriminator is explicit so a typo
// surfaces as an error pointing at the right invocation, not as a
// silent fallback into the wrong mode.
func Run(ctx context.Context, cfg *cliutil.Config, args []string, versionFlag string, deep, verbose, checkRefs bool) error {
	out := cliutil.NewWriter(cfg)
	defer out.Close()

	switch len(args) {
	case 0:
		// No positional → resolve via current package (--package /
		// COB_PACKAGE_COORDS / .cob/current / ~/.config/cob/current),
		// then run as if the user had passed those coords as a single
		// arg (self-integrity check). Falling all the way through the
		// switch keeps the one-arg dispatch's manifest/dir/coords
		// detection in one place; here we know it's coords because the
		// current-package file only ever holds coordinate strings.
		target, err := cliutil.ResolveTarget(cfg, out, nil, "diff")
		if err != nil {
			return cliutil.Fail(out, "diff", cob.ExitError, "%s", err)
		}
		return runSelfCheck(ctx, cfg, out, target, verbose, checkRefs)
	case 1:
		target := args[0]
		if cliutil.IsManifestPath(target) {
			version := versionFlag
			if version == "" {
				version = os.Getenv("COB_VERSION")
			}
			if version == "" {
				// No --version → offline lint. The implicit validation
				// also runs at the top of every other manifest-based
				// command; this mode is the user-facing report of it.
				return runLint(out, target)
			}
			return runManifest(ctx, cfg, out, target, version, deep, verbose, checkRefs)
		}
		// Directory as a single arg is ambiguous (which package?) — point
		// at the right shape instead of guessing.
		if info, err := os.Stat(target); err == nil && info.IsDir() {
			return cliutil.Fail(out, "diff", cob.ExitError,
				"%s is a directory — to diff its files against a published version, give the coordinates as a second argument:\n  cob diff %s <domain>/<repo>/<ns>/<pkg>@<version>",
				target, target)
		}
		// Otherwise: coordinates → self-integrity check.
		return runSelfCheck(ctx, cfg, out, target, verbose, checkRefs)
	case 2:
		// Two args: dir + coords, OR coords + coords.
		info, err := os.Stat(args[0])
		if err == nil && info.IsDir() {
			return runDir(ctx, cfg, out, args[0], args[1], verbose)
		}
		// Reject "file + coords" — the only valid first-arg-is-file case
		// is manifest mode, which takes ONE positional. A two-arg invocation
		// where the first isn't a directory is a typo.
		if err == nil {
			return cliutil.Fail(out, "diff", cob.ExitError,
				"first argument must be a directory or coordinates when two args are given; got file %s", args[0])
		}
		return runVersions(ctx, cfg, out, args[0], args[1], verbose)
	}
	return cliutil.Fail(out, "diff", cob.ExitError, "diff takes one or two positional arguments")
}
