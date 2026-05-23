package pull

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

	"github.com/jmurray2011/cob/internal/cliutil"
)

// generatedManifestFile is the manifest cob writes next to assets on a
// whole-package pull (kept in sync with the cli/manifest_cmd.go constant).
const generatedManifestFile = "cob-manifest.yaml"

func NewCmd(cfg *cliutil.Config) *cobra.Command {
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
		Args: cobra.MaximumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			// Single arg is always the target (a hypothetical
			// "asset-name-with-implicit-target" rule would collide
			// with pull's existing 1-or-2 positional shape). 0 args
			// triggers the current-package fallback inside Run.
			var target, assetName string
			if len(args) > 0 {
				target = args[0]
			}
			if len(args) > 1 {
				assetName = args[1]
			}
			return Run(cmd.Context(), cfg, target, flagVersion, flagOutput, flagAssets, assetName, flagConcurrency)
		},
	}

	cmd.Flags().StringVar(&flagVersion, "version", "", "Specific version (required with manifest)")
	cmd.Flags().StringVarP(&flagOutput, "output", "o", "", "Output path (directory or filename)")
	cmd.Flags().StringVar(&flagAssets, "assets", "", "Pull specific assets only (comma-separated)")
	cmd.Flags().IntVar(&flagConcurrency, "concurrency", cliutil.DefaultConcurrency, "Max assets downloaded in parallel (1 = sequential; clamped to [1,32] to avoid CodeArtifact throttling — a warning prints if a passed value was changed)")

	return cmd
}

func Run(ctx context.Context, cfg *cliutil.Config, target, versionFlag, outputPath, assetsFilter, assetArg string, concurrency int) error {
	out := cliutil.NewWriter(cfg)
	defer out.Close()
	ctx, cancel := cliutil.Interruptable(ctx, cfg, out)
	defer cancel()

	// Empty target → fall back to the current package (sourced from
	// --package, COB_PACKAGE_COORDS, .cob/current, or
	// ~/.config/cob/current). Cobra's MaximumNArgs(2) allows the
	// zero-arg call.
	if target == "" {
		var err error
		target, err = cliutil.ResolveTarget(cfg, out, nil, "pull")
		if err != nil {
			return cliutil.Fail(out, "pull", cob.ExitError, "%s", err)
		}
	}

	client, err := cliutil.DialClient(ctx, cfg)
	if err != nil {
		return cliutil.Fail(out, "pull", cob.ExitError, "%s", err)
	}

	puller := cob.NewPuller(client)
	var coords *cob.PackageCoordinates

	if cliutil.IsManifestPath(target) {
		// Manifest mode.
		version, err := cliutil.ResolveVersion(versionFlag)
		if err != nil {
			return cliutil.Fail(out, "pull", cob.ExitError, "%s", err)
		}

		m, err := manifest.Load(target)
		if err != nil {
			return cliutil.Fail(out, "pull", cob.ExitError, "%s", err)
		}
		cliutil.WarnManifestOverrides(m, out)
		// Implicit pre-flight lint — see runPublish for rationale.
		// Pull only reads m.Domain/Repository/Namespace/Package to
		// resolve coords, but a malformed manifest (bad URI syntax,
		// reserved asset name, basename collision) should still cliutil.Fail
		// the same way every other manifest-based command does.
		if err := cliutil.ValidateManifest(m, version); err != nil {
			return cliutil.Fail(out, "pull", cob.ExitError, "%s", err)
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
			return cliutil.Fail(out, "pull", cob.ExitError, "%s", err)
		}
		if coords.Version == "" {
			return cliutil.Fail(out, "pull", cob.ExitError, "version is required for pull (use domain/repo/ns/pkg@version or @latest)")
		}
	}

	// Resolve @latest if needed.
	registry := cob.NewRegistry(client)
	if err := cliutil.ResolveLatestIfNeeded(ctx, coords, registry, out); err != nil {
		return cliutil.Fail(out, "pull", cliutil.CodeFor(err), "%s", err)
	}

	// Fetch all asset metadata in a single API call.
	allAssets, err := puller.FetchAssetInfo(ctx, coords)
	if err != nil {
		return cliutil.Fail(out, "pull", cliutil.CodeFor(err), "listing assets: %s", err)
	}

	// Narrow to the requested assets.
	assets, unmatched, err := selectAssets(allAssets, assetArg, assetsFilter)
	for _, name := range unmatched {
		out.Warn("asset %q not found in %s/%s@%s, skipping", name, coords.Namespace, coords.Package, coords.Version)
	}
	if err != nil {
		return cliutil.Fail(out, "pull", cob.ExitNotFound, "%s in %s/%s@%s", err, coords.Namespace, coords.Package, coords.Version)
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
			return cliutil.Fail(out, "pull", cob.ExitError, "creating output directory %s: %s", mkdir, err)
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
	cliutil.FillClientMeta(ctx, client, result)

	out.AssetsExpected(len(assets), totalAssetSize(assets))
	puller.Progress = out.AssetProgress

	concurrency = cliutil.ResolveConcurrency(concurrency, out)
	results, ok := cliutil.RunConcurrent(ctx, len(assets), concurrency, func(ctx context.Context, i int) (*cob.AssetResult, error) {
		info := assets[i]
		dest := outputPath
		if dirTarget {
			d, jerr := cliutil.SafeJoin(outputPath, info.Name)
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
		return &cliutil.ExitError{Code: cob.ExitInterrupted}
	}

	out.Summary("Pulled %d assets to %s", downloaded+skipped, outputPath)

	// A failed asset must surface as a non-zero exit (a CI step that does
	// `cob pull && deploy` otherwise deploys with missing/partial assets).
	// JSON is still emitted so pipelines can parse the partial result.
	if result.Status == "error" {
		out.CommandResult(result)
		return &cliutil.ExitError{Code: cob.ExitError}
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
	y, err := cliutil.ManifestYAMLFor(ctx, client, coords)
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

func isDir(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return info.IsDir()
}

// firstResultError returns the error message of the first failed asset
// result, for the partial-failure summary.
func firstResultError(results []*cob.AssetResult) string {
	for _, r := range results {
		if r != nil && r.ErrorMsg != "" {
			return r.ErrorMsg
		}
	}
	return "asset transfer failed"
}
