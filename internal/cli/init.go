package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/jmurray2011/cob/internal/cob"
	"github.com/jmurray2011/cob/internal/manifest"

	"github.com/jmurray2011/cob/internal/cliutil"
)

func newInitCmd(cfg *cliutil.Config) *cobra.Command {
	var (
		flagForCoords string
		flagForce     bool
		flagOutput    string
		flagMinimal   bool
	)

	cmd := &cobra.Command{
		Use:   "init [<dir>]",
		Short: "Generate a manifest from a directory of files",
		Long: "Scans <dir> (default: .) for top-level regular files and " +
			"writes a manifest with each file as a local `./<filename>` " +
			"source. Coordinates (domain/repository/namespace/package) are " +
			"placeholders unless --for is given; edit them before publishing.\n\n" +
			"By default the manifest is written to <dir>/cob-manifest.yaml " +
			"so the ./<filename> source paths resolve. Use -o to choose a " +
			"different path, or -o - to write to stdout.\n\n" +
			"Skips hidden files, sub-directories, and the cob-* files " +
			"(cob-manifest.yaml, cob-provenance.json) so a re-run on a " +
			"pulled directory doesn't recurse on its own metadata. Adjust " +
			"source URIs in the generated manifest if you want s3:// or " +
			"ca:// sources instead of local files.",
		Example: `  # Scaffold a manifest from the current directory
  cob init

  # Scaffold from a specific directory
  cob init ~/build-output

  # Pre-fill coordinates so you don't have to edit them after
  cob init ~/build-output --for acme/dev/tools/my-app

  # Print to stdout instead of writing a file
  cob init . -o -

  # Bare schema (no comments, no promote stages)
  cob init . --minimal`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := "."
			if len(args) > 0 {
				dir = args[0]
			}
			return runInit(cfg, dir, flagForCoords, flagOutput, flagForce, flagMinimal)
		},
	}
	cmd.Flags().StringVar(&flagForCoords, "for", "", "Pre-fill coordinates (e.g. acme/dev/tools/my-app)")
	cmd.Flags().BoolVar(&flagForce, "force", false, "Overwrite an existing manifest at the output path")
	cmd.Flags().StringVarP(&flagOutput, "output", "o", "", "Output path (default: <dir>/cob-manifest.yaml; '-' for stdout)")
	cmd.Flags().BoolVar(&flagMinimal, "minimal", false, "Omit comments and the promote section")
	return cmd
}

func runInit(cfg *cliutil.Config, dirArg, coordsArg, outputPath string, force, minimal bool) error {
	out := cliutil.NewWriter(cfg)
	defer out.Close()

	info, err := os.Stat(dirArg)
	if err != nil {
		return cliutil.Fail(out, "init", cob.ExitError, "%s", err)
	}
	if !info.IsDir() {
		return cliutil.Fail(out, "init", cob.ExitError, "%s is not a directory", dirArg)
	}

	files, skipped, err := scanInitDir(dirArg)
	if err != nil {
		return cliutil.Fail(out, "init", cob.ExitError, "%s", err)
	}
	if len(files) == 0 {
		return cliutil.Fail(out, "init", cob.ExitError,
			"no eligible files in %s — init generates a manifest from a directory of files (it skips hidden files, sub-dirs, and cob-{manifest.yaml,provenance.json})",
			dirArg)
	}

	// Placeholder coords. Sized so a `cob diff <manifest>` lint passes
	// without the user editing anything first — they can iterate, then
	// fill in real values when they're ready to publish.
	coords := &cob.PackageCoordinates{
		Domain:     "my-domain",
		Repository: "dev",
		Namespace:  "my-namespace",
		Package:    "my-package",
	}
	if coordsArg != "" {
		parsed, err := manifest.ParseCoordinates(coordsArg)
		if err != nil {
			return cliutil.Fail(out, "init", cob.ExitError, "--for: %s", err)
		}
		// @version on --for is a usage error — the version belongs to
		// the publish call, not the manifest.
		if parsed.Version != "" {
			return cliutil.Fail(out, "init", cob.ExitError,
				"--for does not accept an @version segment — the version is supplied at publish time")
		}
		if parsed.Domain != "" {
			coords.Domain = parsed.Domain
		}
		if parsed.Repository != "" {
			coords.Repository = parsed.Repository
		}
		if parsed.Namespace != "" {
			coords.Namespace = parsed.Namespace
		}
		if parsed.Package != "" {
			coords.Package = parsed.Package
		}
	}

	// Pick output destination. `-` is the conventional stdout sigil;
	// empty string means "use the default", which is
	// <dir>/cob-manifest.yaml so the ./<filename> source paths land
	// next to the files they reference.
	var w io.Writer
	var outputFile string
	switch outputPath {
	case "-":
		w = out.Stdout()
	case "":
		outputFile = filepath.Join(dirArg, "cob-manifest.yaml")
	default:
		outputFile = outputPath
	}
	if outputFile != "" {
		// Without --force, create exclusively: O_EXCL fails if the file
		// already exists AND refuses to follow a symlink onto its target —
		// closing the stat-then-create TOCTOU the previous os.Stat/os.Create
		// pair left open. With --force we truncate in place.
		flag := os.O_WRONLY | os.O_CREATE | os.O_TRUNC
		if !force {
			flag = os.O_WRONLY | os.O_CREATE | os.O_EXCL
		}
		f, err := os.OpenFile(outputFile, flag, 0o644)
		if errors.Is(err, os.ErrExist) {
			return cliutil.Fail(out, "init", cob.ExitConflict,
				"%s already exists; --force to overwrite", outputFile)
		}
		if err != nil {
			return cliutil.Fail(out, "init", cob.ExitError, "%s", err)
		}
		defer f.Close()
		w = f
	}

	renderInitManifest(w, coords, files, dirArg, minimal)

	// Operator-visible epilogue — only when writing a real file (stdout
	// mode would corrupt the manifest output with these helper lines).
	if outputFile != "" {
		abs, _ := filepath.Abs(outputFile)
		out.Plain("Wrote %s (%d sources).", abs, len(files))
		if skipped > 0 {
			out.Plain("Skipped %d entries (hidden / cob-manifest.yaml / cob-provenance.json / sub-dirs).", skipped)
		}
		if coordsArg == "" {
			out.Plain("Edit domain/repository/namespace/package, then `cob publish %s --version <X>`.", abs)
		} else {
			out.Plain("Ready: `cob publish %s --version <X>`.", abs)
		}
	}
	return nil
}

// scanInitDir lists top-level regular files in dir that are eligible
// to be manifest sources. It deliberately skips:
//   - directories (we don't recurse; init is one-level only)
//   - hidden files (./.git, .DS_Store, .gitignore would all surprise
//     someone who ran init expecting build artifacts)
//   - cob-manifest.yaml / cob-provenance.json (so a re-run on a pulled
//     directory doesn't recurse on its own metadata)
//   - non-regular files (symlinks, FIFOs, etc. — would cliutil.Fail at
//     publish time anyway)
//
// Returns the eligible file names (sorted) and a skipped-count for
// the epilogue line. Order is alphabetical so manifest diffs across
// re-runs are stable.
func scanInitDir(dir string) (files []string, skipped int, err error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, 0, err
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() {
			skipped++
			continue
		}
		if strings.HasPrefix(name, ".") {
			skipped++
			continue
		}
		if name == "cob-manifest.yaml" || name == cob.ProvenanceFile {
			skipped++
			continue
		}
		info, err := e.Info()
		if err != nil {
			skipped++
			continue
		}
		if !info.Mode().IsRegular() {
			skipped++
			continue
		}
		files = append(files, name)
	}
	sort.Strings(files)
	return files, skipped, nil
}

// renderInitManifest writes the generated manifest. Source keys are
// the file basenames; source URIs are ./<filename> so the manifest is
// resolvable from the directory it lives in. A widest-key pad keeps
// the value column aligned for readability.
func renderInitManifest(w io.Writer, coords *cob.PackageCoordinates, files []string, dirArg string, minimal bool) {
	if !minimal {
		fmt.Fprintf(w, "# cob package manifest — generated by 'cob init %s'.\n", dirArg)
		fmt.Fprintln(w, "# Edit the domain/repository/namespace/package fields below.")
		fmt.Fprintln(w, "# Swap any source's `./<name>` for an s3:// or ca:// URI if the bytes")
		fmt.Fprintln(w, "# should come from elsewhere instead of the local file.")
		fmt.Fprintln(w, "# Run 'cob diff <this-file>' (no --version) for offline schema feedback.")
		fmt.Fprintln(w)
	}
	fmt.Fprintf(w, "domain: %s\n", coords.Domain)
	fmt.Fprintf(w, "repository: %s\n", coords.Repository)
	fmt.Fprintf(w, "namespace: %s\n", coords.Namespace)
	fmt.Fprintf(w, "package: %s\n", coords.Package)
	fmt.Fprintln(w)
	fmt.Fprintln(w, "sources:")

	nameWidth := 0
	for _, f := range files {
		if l := len(f) + 1; l > nameWidth { // +1 for the trailing colon
			nameWidth = l
		}
	}
	for _, f := range files {
		key := f + ":"
		fmt.Fprintf(w, "  %-*s ./%s\n", nameWidth, key, f)
	}

	if !minimal {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "# Promotion ordering (optional). Drives 'cob promote <pkg.yaml> --to <stage>'.")
		fmt.Fprintln(w, "promote:")
		fmt.Fprintln(w, "  stages:")
		fmt.Fprintln(w, "    - dev")
		fmt.Fprintln(w, "    - staging")
		fmt.Fprintln(w, "    - prod")
	}
}
