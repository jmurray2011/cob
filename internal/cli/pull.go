package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/jmurray2011/cob/internal/manifest"
	"github.com/jmurray2011/cob/internal/output"
	"github.com/jmurray2011/cob/pkg/cob"
)

func newPullCmd() *cobra.Command {
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
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			var assetName string
			if len(args) > 1 {
				assetName = args[1]
			}
			return runPull(cmd.Context(), args[0], flagVersion, flagOutput, flagAssets, assetName, flagConcurrency)
		},
	}

	cmd.Flags().StringVar(&flagVersion, "version", "", "Specific version (required with manifest)")
	cmd.Flags().StringVar(&flagOutput, "output", "", "Output path (directory or filename)")
	cmd.Flags().StringVar(&flagAssets, "assets", "", "Pull specific assets only (comma-separated)")
	cmd.Flags().IntVar(&flagConcurrency, "concurrency", defaultConcurrency, "Max assets downloaded in parallel (1 = sequential)")

	return cmd
}

func runPull(ctx context.Context, target, versionFlag, outputPath, assetsFilter, assetArg string, concurrency int) error {
	out := newWriter(flagJSON)

	client, err := newClient(ctx, cob.ClientOptions{
		Profile: flagProfile,
		Region:  flagRegion,
	})
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

	// Filter to requested assets.
	var assets []cob.AssetInfo
	if assetArg != "" {
		// Single asset by positional arg.
		for _, a := range allAssets {
			if a.Name == assetArg {
				assets = append(assets, a)
				break
			}
		}
		if len(assets) == 0 {
			return fail(out, "pull", cob.ExitNotFound, "asset %q not found in %s/%s@%s", assetArg, coords.Namespace, coords.Package, coords.Version)
		}
	} else if assetsFilter != "" {
		// Comma-separated filter.
		wanted := make(map[string]bool)
		for _, name := range strings.Split(assetsFilter, ",") {
			wanted[strings.TrimSpace(name)] = true
		}
		matched := make(map[string]bool)
		for _, a := range allAssets {
			if wanted[a.Name] {
				assets = append(assets, a)
				matched[a.Name] = true
			}
		}
		// Warn about names in the filter that didn't match any asset.
		for name := range wanted {
			if !matched[name] {
				out.Warn("asset %q not found in %s/%s@%s, skipping",
					name, coords.Namespace, coords.Package, coords.Version)
			}
		}
		if len(assets) == 0 {
			return fail(out, "pull", cob.ExitNotFound, "none of the requested assets found in %s/%s@%s",
				coords.Namespace, coords.Package, coords.Version)
		}
	} else {
		assets = allAssets
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

	for _, r := range results {
		if r != nil && r.Error == nil {
			result.Assets = append(result.Assets, *r)
			result.TotalSize += r.Size
		}
	}
	if !ok {
		result.Status = "error"
		result.Error = firstResultError(results)
	}

	result.DurationMs = time.Since(start).Milliseconds()
	out.Summary("Pulled %d assets to %s", len(result.Assets), outputPath)

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

// safeJoin joins an asset name under root and confirms it stays within
// root. CodeArtifact asset names are path-like and could contain ../
// traversal; cob must not write outside the chosen output directory
// regardless of what the server returns.
func safeJoin(root, name string) (string, error) {
	dest := filepath.Join(root, name)
	rel, err := filepath.Rel(root, dest)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("asset %q escapes the output directory", name)
	}
	return dest, nil
}

func isDir(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return info.IsDir()
}
