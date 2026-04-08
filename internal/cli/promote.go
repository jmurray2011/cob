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
		flagVersion string
		flagTo      string
		flagForce   bool
		flagYes     bool
	)

	cmd := &cobra.Command{
		Use:   "promote <manifest|coordinates>",
		Short: "Copy a package version between repositories",
		Long:  "Copies a package version from one repo to another, streaming through memory.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPromote(cmd.Context(), args[0], flagVersion, flagTo, flagForce, flagYes)
		},
	}

	cmd.Flags().StringVar(&flagVersion, "version", "", "Specific version (required with manifest)")
	cmd.Flags().StringVar(&flagTo, "to", "", "Destination repository (required)")
	cmd.Flags().BoolVar(&flagForce, "force", false, "Overwrite if version exists in destination")
	cmd.Flags().BoolVar(&flagYes, "yes", false, "Skip confirmation")
	cmd.MarkFlagRequired("to")

	return cmd
}

func runPromote(ctx context.Context, target, versionFlag, toRepo string, force, yes bool) error {
	out := output.New(flagJSON)

	client, err := cob.NewClient(ctx, cob.ClientOptions{
		Profile: flagProfile,
		Region:  flagRegion,
	})
	if err != nil {
		out.ErrorResult("promote", err.Error())
		os.Exit(cob.ExitError)
	}

	var coords *cob.PackageCoordinates
	var srcRepo string

	if isManifestPath(target) {
		version, err := resolveVersion(versionFlag)
		if err != nil {
			out.ErrorResult("promote", err.Error())
			os.Exit(cob.ExitError)
		}

		m, err := manifest.Load(target)
		if err != nil {
			out.ErrorResult("promote", err.Error())
			os.Exit(cob.ExitError)
		}
		warnManifestOverrides(m, out)

		// Infer source repo from promote stages.
		srcRepo, err = m.InferPromoteSource(toRepo)
		if err != nil {
			out.ErrorResult("promote", err.Error())
			os.Exit(cob.ExitError)
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
			out.ErrorResult("promote", err.Error())
			os.Exit(cob.ExitError)
		}
		if coords.Version == "" {
			out.ErrorResult("promote", "version is required for promote (use domain/repo/ns/pkg@version or @latest)")
			os.Exit(cob.ExitError)
		}
		srcRepo = coords.Repository
	}

	registry := cob.NewRegistry(client)

	// Resolve @latest from the source repo.
	coords.Repository = srcRepo
	if err := resolveLatestIfNeeded(ctx, coords, registry, out); err != nil {
		out.ErrorResult("promote", err.Error())
		os.Exit(cob.ExitNotFound)
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
		out.ErrorResult("promote", fmt.Sprintf("checking destination: %s", err))
		os.Exit(cob.ExitError)
	}
	if exists && !force {
		out.ErrorResult("promote", fmt.Sprintf("version %s already exists in %s. Use --force to overwrite.", coords.Version, toRepo))
		os.Exit(cob.ExitConflict)
	}

	out.Header("Promoting %s/%s@%s: %s -> %s",
		coords.Namespace, coords.Package, coords.Version, srcRepo, toRepo)

	if !confirmAction(yes, fmt.Sprintf("Promote to %s?", toRepo)) {
		fmt.Fprintln(os.Stderr, "Aborted.")
		os.Exit(0)
	}

	if exists && force {
		publisher := cob.NewPublisher(client)
		if err := publisher.DeleteVersion(ctx, destCoords); err != nil {
			out.ErrorResult("promote", fmt.Sprintf("deleting existing version in destination: %s", err))
			os.Exit(cob.ExitError)
		}
	}

	start := time.Now()
	promoter := cob.NewPromoter(client)
	results, err := promoter.Promote(ctx, coords, srcRepo, toRepo)

	cmdResult := &cob.CommandResult{
		Command:    "promote",
		Package:    fmt.Sprintf("%s/%s@%s", coords.Namespace, coords.Package, coords.Version),
		Repository: fmt.Sprintf("%s -> %s", srcRepo, toRepo),
		Assets:     results,
		Status:     "ok",
	}

	for _, r := range results {
		cmdResult.TotalSize += r.Size
		out.AssetOK(&r, "")
	}

	if err != nil {
		cmdResult.Status = "error"
		cmdResult.Error = err.Error()
		out.Error("%s\n  Promoted %d assets to %s before failure. Version is in partial state.\n  Re-run with --force to delete and retry.",
			err, len(results), toRepo)
		cmdResult.DurationMs = time.Since(start).Milliseconds()
		out.CommandResult(cmdResult)
		os.Exit(cob.ExitError)
	}

	cmdResult.DurationMs = time.Since(start).Milliseconds()
	out.Summary("Promoted %d assets in %s", len(results), output.FormatDuration(cmdResult.DurationMs))

	return out.CommandResult(cmdResult)
}
