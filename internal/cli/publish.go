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

func newPublishCmd() *cobra.Command {
	var (
		flagVersion string
		flagForce   bool
		flagDryRun  bool
		flagYes     bool
	)

	cmd := &cobra.Command{
		Use:   "publish <manifest>",
		Short: "Publish a package from a manifest",
		Long:  "Reads a manifest file, resolves variables, pulls from each source, and publishes to CodeArtifact.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPublish(cmd.Context(), args[0], flagVersion, flagForce, flagDryRun, flagYes)
		},
	}

	cmd.Flags().StringVar(&flagVersion, "version", "", "Package version (required, or set COB_VERSION)")
	cmd.Flags().BoolVar(&flagForce, "force", false, "Overwrite existing version")
	cmd.Flags().BoolVar(&flagDryRun, "dry-run", false, "Verify sources exist, show plan, don't publish")
	cmd.Flags().BoolVar(&flagYes, "yes", false, "Skip confirmation")

	return cmd
}

func runPublish(ctx context.Context, manifestPath, versionFlag string, force, dryRun, yes bool) error {
	out := output.New(flagJSON)

	version, err := resolveVersion(versionFlag)
	if err != nil {
		out.ErrorResult("publish", err.Error())
		os.Exit(cob.ExitError)
	}
	if version == "latest" {
		out.ErrorResult("publish", "cannot publish to @latest, provide an explicit version")
		os.Exit(cob.ExitError)
	}

	m, err := manifest.Load(manifestPath)
	if err != nil {
		out.ErrorResult("publish", err.Error())
		os.Exit(cob.ExitError)
	}
	warnManifestOverrides(m, out)

	if err := m.ResolveVariables(version); err != nil {
		out.ErrorResult("publish", err.Error())
		os.Exit(cob.ExitError)
	}

	coords := &cob.PackageCoordinates{
		Domain:     m.Domain,
		Repository: m.Repository,
		Namespace:  m.Namespace,
		Package:    m.Package,
		Version:    version,
	}

	client, err := cob.NewClient(ctx, cob.ClientOptions{
		Profile: flagProfile,
		Region:  flagRegion,
	})
	if err != nil {
		out.ErrorResult("publish", err.Error())
		os.Exit(cob.ExitError)
	}

	publisher := cob.NewPublisher(client)
	registry := cob.NewRegistry(client)

	// Check if version already exists.
	exists, err := registry.CheckVersionExists(ctx, coords)
	if err != nil {
		out.ErrorResult("publish", fmt.Sprintf("checking version: %s", err))
		os.Exit(cob.ExitError)
	}
	if exists && !force {
		out.ErrorResult("publish", fmt.Sprintf("version %s already exists in %s/%s. Use --force to overwrite.", version, m.Domain, m.Repository))
		os.Exit(cob.ExitConflict)
	}

	sources, err := buildSources(m, client)
	if err != nil {
		out.ErrorResult("publish", err.Error())
		os.Exit(cob.ExitError)
	}

	out.Header("Publishing %s/%s@%s -> %s/%s", m.Namespace, m.Package, version, m.Domain, m.Repository)

	if dryRun {
		return runDryRun(ctx, sources, out)
	}

	if !confirmAction(yes, fmt.Sprintf("Publish %d assets?", len(sources))) {
		fmt.Fprintln(os.Stderr, "Aborted.")
		os.Exit(0)
	}

	// Force: delete existing version first.
	if exists && force {
		if err := publisher.DeleteVersion(ctx, coords); err != nil {
			out.Error("deleting existing version: %s", err)
			os.Exit(cob.ExitError)
		}
	}

	start := time.Now()
	result := &cob.CommandResult{
		Command:    "publish",
		Package:    fmt.Sprintf("%s/%s@%s", m.Namespace, m.Package, version),
		Repository: fmt.Sprintf("%s/%s", m.Domain, m.Repository),
		Status:     "ok",
	}

	for i, ns := range sources {
		isLast := i == len(sources)-1
		ar, err := publisher.PublishAsset(ctx, coords, ns.Name, ns.Source, !isLast)
		if err != nil {
			out.AssetFail(ns.Name, ns.Source.URI(), err)
			// Only skip assets that haven't been attempted yet.
			for _, remaining := range sources[i+1:] {
				out.AssetSkipped(remaining.Name)
			}
			result.Status = "error"
			result.Error = err.Error()
			out.Error("failed to read %s\n  Published %d of %d assets. Version is in unfinished state.\n  Re-run with --force to delete and retry.",
				ns.Source.URI(), len(result.Assets), len(sources))
			out.CommandResult(result)
			os.Exit(cob.ExitError)
		}
		result.Assets = append(result.Assets, *ar)
		result.TotalSize += ar.Size
		out.AssetOK(ar, ns.Source.URI())
	}

	result.DurationMs = time.Since(start).Milliseconds()
	out.Summary("Published %d assets (%s) in %s",
		len(result.Assets), output.FormatSize(result.TotalSize), output.FormatDuration(result.DurationMs))

	return out.CommandResult(result)
}

func runDryRun(ctx context.Context, sources []NamedSource, out *output.Writer) error {
	var failures int
	for _, ns := range sources {
		meta, err := ns.Source.Resolve(ctx)
		if err != nil {
			out.AssetFail(ns.Name, ns.Source.URI(), err)
			failures++
			continue
		}
		r := &cob.AssetResult{
			Name:   ns.Name,
			Size:   meta.Size,
			SHA256: meta.SHA256,
			Method: "buffered",
		}
		out.AssetOK(r, ns.Source.URI())
	}
	verified := len(sources) - failures
	if failures > 0 {
		out.Summary("Dry run complete. %d of %d sources verified, %d failed.", verified, len(sources), failures)
		os.Exit(cob.ExitError)
	}
	out.Summary("Dry run complete. All %d sources verified.", len(sources))
	return nil
}
