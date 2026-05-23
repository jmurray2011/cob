package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/jmurray2011/cob/internal/cob"
	"github.com/jmurray2011/cob/internal/manifest"
	"github.com/jmurray2011/cob/internal/output"
)

func newPullCmd(cfg *Config) *cobra.Command {
	var (
		flagVersion     string
		flagOutput      string
		flagAssets      string
		flagConcurrency int
	)

	cmd := &cobra.Command{
		Use:   "pull <manifest|coordinates> [asset]",
		Short: "Download assets from CodeArtifact",
		Long:  "Downloads assets to a local directory. Use with a manifest (all assets) or compact coordinates (ad-hoc).",
		Example: `  # pull every asset of a version into a directory
  cob pull acme/dev/tools/my-app@2.1.0 --output ./assets/

  # pull one asset, resolving the latest version
  cob pull acme/dev/tools/my-app@latest app.tar.gz --output ./app.tar.gz`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			var assetName string
			if len(args) > 1 {
				assetName = args[1]
			}
			return runPull(cmd.Context(), cfg, args[0], flagVersion, flagOutput, flagAssets, assetName, flagConcurrency)
		},
	}

	cmd.Flags().StringVar(&flagVersion, "version", "", "Specific version (required with manifest)")
	cmd.Flags().StringVarP(&flagOutput, "output", "o", "", "Output path (directory or filename)")
	cmd.Flags().StringVar(&flagAssets, "assets", "", "Pull specific assets only (comma-separated)")
	cmd.Flags().IntVar(&flagConcurrency, "concurrency", defaultConcurrency, "Max assets downloaded in parallel (1 = sequential)")

	return cmd
}

func runPull(ctx context.Context, cfg *Config, target, versionFlag, outputPath, assetsFilter, assetArg string, concurrency int) error {
	out := newWriter(cfg)
	defer out.Close()
	ctx, cancel := interruptable(ctx, out)
	defer cancel()

	client, err := dialClient(ctx, cfg)
	if err != nil {
		return fail(out, "pull", cob.ExitError, "%s", err)
	}

	puller := cob.NewPuller(client)
	var coords *cob.PackageCoordinates

	if isManifestPath(target) {
		// Manifest mode.
		version, err := resolveVersion(versionFlag)
		if err != nil {
			return fail(out, "pull", cob.ExitError, "%s", err)
		}

		m, err := manifest.Load(target)
		if err != nil {
			return fail(out, "pull", cob.ExitError, "%s", err)
		}
		warnManifestOverrides(m, out)
		// Implicit pre-flight lint — see runPublish for rationale.
		// Pull only reads m.Domain/Repository/Namespace/Package to
		// resolve coords, but a malformed manifest (bad URI syntax,
		// reserved asset name, basename collision) should still fail
		// the same way every other manifest-based command does.
		if err := validateManifest(m, version); err != nil {
			return fail(out, "pull", cob.ExitError, "%s", err)
		}

		coords = &cob.PackageCoordinates{
			Domain:     m.Domain,
			Repository: m.Repository,
			Namespace:  m.Namespace,
			Package:    m.Package,
			Version:    version,
		}
	} else {
		// Compact coordinates mode.
		coords, err = manifest.ParseCoordinates(target)
		if err != nil {
			return fail(out, "pull", cob.ExitError, "%s", err)
		}
		if coords.Version == "" {
			return fail(out, "pull", cob.ExitError, "version is required for pull (use domain/repo/ns/pkg@version or @latest)")
		}
	}

	// Resolve @latest if needed.
	registry := cob.NewRegistry(client)
	if err := resolveLatestIfNeeded(ctx, coords, registry, out); err != nil {
		return fail(out, "pull", codeFor(err), "%s", err)
	}

	// Fetch all asset metadata in a single API call.
	allAssets, err := puller.FetchAssetInfo(ctx, coords)
	if err != nil {
		return fail(out, "pull", codeFor(err), "listing assets: %s", err)
	}

	// Narrow to the requested assets.
	assets, unmatched, err := selectAssets(allAssets, assetArg, assetsFilter)
	for _, name := range unmatched {
		out.Warn("asset %q not found in %s/%s@%s, skipping", name, coords.Namespace, coords.Package, coords.Version)
	}
	if err != nil {
		return fail(out, "pull", cob.ExitNotFound, "%s in %s/%s@%s", err, coords.Namespace, coords.Package, coords.Version)
	}

	if outputPath == "" {
		outputPath = "."
	}

	// A multi-asset / trailing-slash / existing-dir target is a directory;
	// anything else is a single-file path. Create the target up front so
	// `--output ./new/dir/` works without the caller mkdir'ing it first.
	dirTarget := len(assets) > 1 || isDir(outputPath) || strings.HasSuffix(outputPath, "/")
	mkdir := outputPath
	if !dirTarget {
		mkdir = filepath.Dir(outputPath)
	}
	if mkdir != "" && mkdir != "." {
		if err := os.MkdirAll(mkdir, 0o755); err != nil {
			return fail(out, "pull", cob.ExitError, "creating output directory %s: %s", mkdir, err)
		}
	}

	out.Header("Pulling %s/%s@%s from %s/%s",
		coords.Namespace, coords.Package, coords.Version, coords.Domain, coords.Repository)

	start := time.Now()
	result := &cob.CommandResult{
		Command:    "pull",
		Package:    fmt.Sprintf("%s/%s@%s", coords.Namespace, coords.Package, coords.Version),
		Repository: fmt.Sprintf("%s/%s", coords.Domain, coords.Repository),
		Status:     "ok",
	}
	fillClientMeta(ctx, client, result)

	out.AssetsExpected(len(assets), totalAssetSize(assets))
	puller.Progress = out.AssetProgress

	concurrency = resolveConcurrency(concurrency, out)
	results, ok := runConcurrent(len(assets), concurrency, func(i int) (*cob.AssetResult, error) {
		info := assets[i]
		dest := outputPath
		if dirTarget {
			d, jerr := safeJoin(outputPath, info.Name)
			if jerr != nil {
				out.AssetFail(info.Name, "", jerr)
				ar := &cob.AssetResult{Name: info.Name}
				ar.SetError(jerr)
				return ar, jerr
			}
			dest = d
		}

		out.AssetStart(info.Name, "", info.Size)
		ar, err := puller.PullAsset(ctx, coords, info, dest)
		if err != nil {
			out.AssetFail(info.Name, "", err)
			return ar, err
		}
		if ar.Method == "skipped" {
			out.AssetSkipped(info.Name)
		} else {
			out.AssetOK(ar, "")
		}
		return ar, nil
	})

	// Record every asset that ran — successes and failures — so a --json
	// consumer can see which one failed. Downloaded vs skipped is split
	// so an interrupted-pull summary can report accurately ("I had to
	// fetch 1, the other 11 were already there, 3 didn't finish") rather
	// than a single misleading "Pulled N" line.
	var downloaded, skipped, failed int
	for _, r := range results {
		if r == nil {
			continue // never scheduled: an earlier task failed first
		}
		result.Assets = append(result.Assets, *r)
		switch {
		case r.Error != nil:
			failed++
		case r.Method == "skipped":
			skipped++
			result.TotalSize += r.Size
		default:
			downloaded++
			result.TotalSize += r.Size
		}
	}
	if !ok {
		result.Status = "error"
		result.Error = firstResultError(results)
	}

	result.DurationMs = time.Since(start).Milliseconds()

	// Interrupted path: the user hit Ctrl-C inside the live TUI, which
	// cancels the runXxx ctx and lets the in-flight goroutines abort.
	// They appear in result.Assets with Error set; we report them as
	// "incomplete" rather than swallowed and exit 130 (POSIX SIGINT
	// convention) so a CI step can distinguish "user canceled" from
	// "operation failed".
	if out.Interrupted() {
		result.Status = "interrupted"
		incomplete := len(assets) - downloaded - skipped
		out.Summary("Interrupted: %d downloaded, %d already present, %d incomplete — re-run to resume (present files with matching SHA-256 will be skipped)",
			downloaded, skipped, incomplete)
		out.CommandResult(result)
		return &ExitError{Code: cob.ExitInterrupted}
	}

	out.Summary("Pulled %d assets to %s", downloaded+skipped, outputPath)

	// A failed asset must surface as a non-zero exit (a CI step that does
	// `cob pull && deploy` otherwise deploys with missing/partial assets).
	// JSON is still emitted so pipelines can parse the partial result.
	if result.Status == "error" {
		out.CommandResult(result)
		return &ExitError{Code: cob.ExitError}
	}

	// A whole-package pull into a directory also gets a recoverable
	// manifest (reconstructed from provenance, or inferred). Best-effort:
	// the assets are already down, so a manifest hiccup only warns.
	wholePackage := assetArg == "" && assetsFilter == ""
	if wholePackage && dirTarget {
		writePulledManifest(ctx, client, coords, assets, outputPath, out)
	}
	return out.CommandResult(result)
}

// totalAssetSize sums known asset sizes, for the transfer progress meter.
func totalAssetSize(assets []cob.AssetInfo) int64 {
	var n int64
	for _, a := range assets {
		n += a.Size
	}
	return n
}

// selectAssets narrows the full asset list to what the caller asked for: a
// single positional asset, a comma-separated --assets filter, or — given
// neither — everything. unmatched holds any --assets names that matched no
// asset (the caller warns on each). A non-nil error means nothing matched
// at all; it carries no coordinates, so the caller frames it.
func selectAssets(all []cob.AssetInfo, assetArg, assetsFilter string) (selected []cob.AssetInfo, unmatched []string, err error) {
	switch {
	case assetArg != "":
		for _, a := range all {
			if a.Name == assetArg {
				return []cob.AssetInfo{a}, nil, nil
			}
		}
		return nil, nil, fmt.Errorf("asset %q not found", assetArg)
	case assetsFilter != "":
		wanted := make(map[string]bool)
		for _, name := range strings.Split(assetsFilter, ",") {
			wanted[strings.TrimSpace(name)] = true
		}
		matched := make(map[string]bool)
		for _, a := range all {
			if wanted[a.Name] {
				selected = append(selected, a)
				matched[a.Name] = true
			}
		}
		for name := range wanted {
			if !matched[name] {
				unmatched = append(unmatched, name)
			}
		}
		if len(selected) == 0 {
			return nil, unmatched, fmt.Errorf("none of the requested assets found")
		}
		return selected, unmatched, nil
	default:
		return all, nil, nil
	}
}

func writePulledManifest(ctx context.Context, client *cob.Client, coords *cob.PackageCoordinates, assets []cob.AssetInfo, dir string, out *output.Writer) {
	for _, a := range assets {
		if a.Name == generatedManifestFile {
			out.Warn("an asset is named %s; skipping generated manifest", generatedManifestFile)
			return
		}
	}
	y, err := manifestYAMLFor(ctx, client, coords)
	if err != nil {
		out.Warn("could not generate %s: %s", generatedManifestFile, err)
		return
	}
	p := filepath.Join(dir, generatedManifestFile)
	if err := os.WriteFile(p, []byte(y), 0o644); err != nil {
		out.Warn("could not write %s: %s", generatedManifestFile, err)
		return
	}
	out.Plain("Wrote %s", p)
}

// safeJoin joins an asset name under root and confirms the result stays
// within root — lexically (no ../ traversal) and physically (no symlink at
// root or at an existing component of the destination redirects the write).
// CodeArtifact asset names are path-like and server-controlled; cob must not
// write outside the chosen output directory regardless of what they contain.
func safeJoin(root, name string) (string, error) {
	dest := filepath.Join(root, name)
	if escapes(root, dest) {
		return "", fmt.Errorf("asset %q escapes the output directory", name)
	}
	// Lexical containment is not enough: a symlink at root, or at any
	// existing component of dest, could redirect the write elsewhere.
	// Resolve symlinks on the deepest existing prefix of each and re-check.
	realRoot, err := resolveExisting(root)
	if err != nil {
		return "", err
	}
	realDest, err := resolveExisting(dest)
	if err != nil {
		return "", err
	}
	if escapes(realRoot, realDest) {
		return "", fmt.Errorf("asset %q escapes the output directory via a symlink", name)
	}
	return dest, nil
}

// escapes reports whether dest lies outside root by lexical path comparison.
func escapes(root, dest string) bool {
	rel, err := filepath.Rel(root, dest)
	return err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}

// resolveExisting returns filepath.EvalSymlinks of the deepest ancestor of p
// that exists. A pull target usually does not exist yet, so EvalSymlinks(p)
// itself would fail; resolving the existing prefix is what containment needs.
func resolveExisting(p string) (string, error) {
	p = filepath.Clean(p)
	for {
		resolved, err := filepath.EvalSymlinks(p)
		if err == nil {
			return resolved, nil
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(p)
		if parent == p {
			return p, nil // reached the filesystem root; nothing existed
		}
		p = parent
	}
}

func isDir(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return info.IsDir()
}
