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
		flagVersion string
		flagOutput  string
		flagAssets  string
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
			return runPull(cmd.Context(), args[0], flagVersion, flagOutput, flagAssets, assetName)
		},
	}

	cmd.Flags().StringVar(&flagVersion, "version", "", "Specific version (required with manifest)")
	cmd.Flags().StringVar(&flagOutput, "output", "", "Output path (directory or filename)")
	cmd.Flags().StringVar(&flagAssets, "assets", "", "Pull specific assets only (comma-separated)")

	return cmd
}

func runPull(ctx context.Context, target, versionFlag, outputPath, assetsFilter, assetArg string) error {
	out := output.New(flagJSON)

	client, err := cob.NewClient(ctx, cob.ClientOptions{
		Profile: flagProfile,
		Region:  flagRegion,
	})
	if err != nil {
		out.ErrorResult("pull", err.Error())
		os.Exit(cob.ExitError)
	}

	puller := cob.NewPuller(client)
	var coords *cob.PackageCoordinates

	if isManifestPath(target) {
		// Manifest mode.
		version, err := resolveVersion(versionFlag)
		if err != nil {
			out.ErrorResult("pull", err.Error())
			os.Exit(cob.ExitError)
		}

		m, err := manifest.Load(target)
		if err != nil {
			out.ErrorResult("pull", err.Error())
			os.Exit(cob.ExitError)
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
			out.ErrorResult("pull", err.Error())
			os.Exit(cob.ExitError)
		}
		if coords.Version == "" {
			out.ErrorResult("pull", "version is required for pull (use domain/repo/ns/pkg@version or @latest)")
			os.Exit(cob.ExitError)
		}
	}

	// Resolve @latest if needed.
	registry := cob.NewRegistry(client)
	if err := resolveLatestIfNeeded(ctx, coords, registry, out); err != nil {
		out.ErrorResult("pull", err.Error())
		os.Exit(cob.ExitNotFound)
	}

	// Fetch all asset metadata in a single API call.
	allAssets, err := puller.FetchAssetInfo(ctx, coords)
	if err != nil {
		out.ErrorResult("pull", fmt.Sprintf("listing assets: %s", err))
		os.Exit(cob.ExitNotFound)
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
			out.ErrorResult("pull", fmt.Sprintf("asset %q not found in %s/%s@%s", assetArg, coords.Namespace, coords.Package, coords.Version))
			os.Exit(cob.ExitNotFound)
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
				fmt.Fprintf(os.Stderr, "Warning: asset %q not found in %s/%s@%s, skipping\n",
					name, coords.Namespace, coords.Package, coords.Version)
			}
		}
		if len(assets) == 0 {
			out.ErrorResult("pull", fmt.Sprintf("none of the requested assets found in %s/%s@%s",
				coords.Namespace, coords.Package, coords.Version))
			os.Exit(cob.ExitNotFound)
		}
	} else {
		assets = allAssets
	}

	if outputPath == "" {
		outputPath = "."
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

	for _, info := range assets {
		dest := outputPath
		if len(assets) > 1 || isDir(outputPath) || strings.HasSuffix(outputPath, "/") {
			dest = filepath.Join(outputPath, info.Name)
		}

		ar, err := puller.PullAsset(ctx, coords, info, dest)
		if err != nil {
			out.AssetFail(info.Name, "", err)
			result.Status = "error"
			result.Error = err.Error()
			break
		}
		result.Assets = append(result.Assets, *ar)
		result.TotalSize += ar.Size
		if ar.Method == "skipped" {
			out.AssetSkipped(info.Name)
		} else {
			out.AssetOK(ar, "")
		}
	}

	result.DurationMs = time.Since(start).Milliseconds()
	out.Summary("Pulled %d assets to %s", len(result.Assets), outputPath)

	return out.CommandResult(result)
}

func isDir(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return info.IsDir()
}
