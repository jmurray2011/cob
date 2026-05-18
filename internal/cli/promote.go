package cli

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/jmurray2011/cob/internal/manifest"
	"github.com/jmurray2011/cob/internal/output"
	"github.com/jmurray2011/cob/pkg/cob"
)

func newPromoteCmd() *cobra.Command {
	var (
		flagVersion     string
		flagTo          string
		flagForce       bool
		flagYes         bool
		flagConcurrency int
	)

	cmd := &cobra.Command{
		Use:   "promote <manifest|coordinates>",
		Short: "Copy a package version between repositories",
		Long:  "Copies a package version from one repo to another, streaming in constant memory (via a temp file).",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPromote(cmd.Context(), args[0], flagVersion, flagTo, flagForce, flagYes, flagConcurrency)
		},
	}

	cmd.Flags().StringVar(&flagVersion, "version", "", "Specific version (required with manifest)")
	cmd.Flags().StringVar(&flagTo, "to", "", "Destination repository (required)")
	cmd.Flags().BoolVar(&flagForce, "force", false, "Overwrite if version exists in destination")
	cmd.Flags().BoolVar(&flagYes, "yes", false, "Skip confirmation")
	cmd.Flags().IntVar(&flagConcurrency, "concurrency", defaultConcurrency, "Max assets transferred in parallel (1 = sequential)")
	cmd.MarkFlagRequired("to")

	return cmd
}

func runPromote(ctx context.Context, target, versionFlag, toRepo string, force, yes bool, concurrency int) error {
	out := output.New(flagJSON)

	client, err := cob.NewClient(ctx, cob.ClientOptions{
		Profile: flagProfile,
		Region:  flagRegion,
	})
	if err != nil {
		return fail(out, "promote", cob.ExitError, "%s", err)
	}

	var coords *cob.PackageCoordinates
	var srcRepo string

	if isManifestPath(target) {
		version, err := resolveVersion(versionFlag)
		if err != nil {
			return fail(out, "promote", cob.ExitError, "%s", err)
		}

		m, err := manifest.Load(target)
		if err != nil {
			return fail(out, "promote", cob.ExitError, "%s", err)
		}
		warnManifestOverrides(m, out)

		// Infer source repo from promote stages.
		srcRepo, err = m.InferPromoteSource(toRepo)
		if err != nil {
			return fail(out, "promote", cob.ExitError, "%s", err)
		}

		coords = &cob.PackageCoordinates{
			Domain:    m.Domain,
			Namespace: m.Namespace,
			Package:   m.Package,
			Version:   version,
		}
	} else {
		coords, err = manifest.ParseCoordinates(target)
		if err != nil {
			return fail(out, "promote", cob.ExitError, "%s", err)
		}
		if coords.Version == "" {
			return fail(out, "promote", cob.ExitError, "version is required for promote (use domain/repo/ns/pkg@version or @latest)")
		}
		srcRepo = coords.Repository
	}

	registry := cob.NewRegistry(client)

	// Resolve @latest from the source repo.
	coords.Repository = srcRepo
	if err := resolveLatestIfNeeded(ctx, coords, registry, out); err != nil {
		return fail(out, "promote", cob.ExitNotFound, "%s", err)
	}

	// Check if version exists in destination.
	destCoords := &cob.PackageCoordinates{
		Domain:     coords.Domain,
		Repository: toRepo,
		Namespace:  coords.Namespace,
		Package:    coords.Package,
		Version:    coords.Version,
	}
	exists, err := registry.CheckVersionExists(ctx, destCoords)
	if err != nil {
		return fail(out, "promote", cob.ExitError, "checking destination: %s", err)
	}
	if exists && !force {
		return fail(out, "promote", cob.ExitConflict, "version %s already exists in %s. Use --force to overwrite.", coords.Version, toRepo)
	}

	out.Header("Promoting %s/%s@%s: %s -> %s",
		coords.Namespace, coords.Package, coords.Version, srcRepo, toRepo)

	proceed, err := confirmAction(yes, fmt.Sprintf("Promote to %s?", toRepo))
	if err != nil {
		return fail(out, "promote", cob.ExitError, "%s", err)
	}
	if !proceed {
		fmt.Fprintln(os.Stderr, "Aborted.")
		return nil
	}

	if exists && force {
		publisher := cob.NewPublisher(client)
		if err := publisher.DeleteVersion(ctx, destCoords); err != nil {
			return fail(out, "promote", cob.ExitError, "deleting existing version in destination: %s", err)
		}
	}

	promoter := cob.NewPromoter(client)
	assetNames, err := promoter.ListAssetsToPromote(ctx, coords, srcRepo)
	if err != nil {
		return fail(out, "promote", cob.ExitError, "%s", err)
	}

	cmdResult := &cob.CommandResult{
		Command:    "promote",
		Package:    fmt.Sprintf("%s/%s@%s", coords.Namespace, coords.Package, coords.Version),
		Repository: fmt.Sprintf("%s -> %s", srcRepo, toRepo),
		Status:     "ok",
	}

	start := time.Now()
	results, _, ok := runFinalizeProtocol(len(assetNames), concurrency,
		func(i int, unfinished bool) (*cob.AssetResult, error) {
			name := assetNames[i]
			out.AssetStart(name, "", 0)
			ar, err := promoter.PromoteAsset(ctx, coords, srcRepo, toRepo, name, unfinished)
			if err != nil {
				out.AssetFail(name, "", err)
				return ar, err
			}
			out.AssetOK(ar, "")
			return ar, nil
		})

	for _, r := range results {
		if r != nil && r.Error == nil {
			cmdResult.Assets = append(cmdResult.Assets, *r)
			cmdResult.TotalSize += r.Size
		}
	}
	cmdResult.DurationMs = time.Since(start).Milliseconds()

	if !ok {
		cmdResult.Status = "error"
		cmdResult.Error = firstResultError(results)
		out.Error("%s\n  Promoted %d of %d assets to %s before failure. Version is in partial state.\n  Re-run with --force to delete and retry.",
			cmdResult.Error, len(cmdResult.Assets), len(assetNames), toRepo)
		out.CommandResult(cmdResult)
		return &ExitError{Code: cob.ExitError}
	}

	out.Summary("Promoted %d assets in %s", len(cmdResult.Assets), output.FormatDuration(cmdResult.DurationMs))
	return out.CommandResult(cmdResult)
}
