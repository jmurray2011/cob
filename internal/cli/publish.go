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
		flagVersion     string
		flagForce       bool
		flagDryRun      bool
		flagYes         bool
		flagConcurrency int
	)

	cmd := &cobra.Command{
		Use:   "publish <manifest>",
		Short: "Publish a package from a manifest",
		Long:  "Reads a manifest file, resolves variables, pulls from each source, and publishes to CodeArtifact.",
		Example: `  # publish a manifest at an explicit version
  cob publish ./my-package.yaml --version 2.1.0

  # CI: skip the confirmation prompt
  cob publish ./my-package.yaml --version 2.1.0 --yes`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPublish(cmd.Context(), args[0], flagVersion, flagForce, flagDryRun, flagYes, flagConcurrency)
		},
	}

	cmd.Flags().StringVar(&flagVersion, "version", "", "Package version (required, or set COB_VERSION)")
	cmd.Flags().BoolVar(&flagForce, "force", false, "Overwrite existing version")
	cmd.Flags().BoolVar(&flagDryRun, "dry-run", false, "Verify sources exist, show plan, don't publish")
	cmd.Flags().BoolVar(&flagYes, "yes", false, "Skip confirmation")
	cmd.Flags().IntVar(&flagConcurrency, "concurrency", defaultConcurrency, "Max assets transferred in parallel (1 = sequential)")

	return cmd
}

func runPublish(ctx context.Context, manifestPath, versionFlag string, force, dryRun, yes bool, concurrency int) error {
	out := newWriter(flagJSON)

	version, err := resolveVersion(versionFlag)
	if err != nil {
		return fail(out, "publish", cob.ExitError, "%s", err)
	}
	if version == "latest" {
		return fail(out, "publish", cob.ExitError, "cannot publish to @latest, provide an explicit version")
	}

	m, err := manifest.Load(manifestPath)
	if err != nil {
		return fail(out, "publish", cob.ExitError, "%s", err)
	}
	warnManifestOverrides(m, out)

	if err := m.ResolveVariables(version); err != nil {
		return fail(out, "publish", cob.ExitError, "%s", err)
	}

	coords := &cob.PackageCoordinates{
		Domain:     m.Domain,
		Repository: m.Repository,
		Namespace:  m.Namespace,
		Package:    m.Package,
		Version:    version,
	}

	client, err := dialClient(ctx)
	if err != nil {
		return fail(out, "publish", cob.ExitError, "%s", err)
	}

	publisher := cob.NewPublisher(client)
	registry := cob.NewRegistry(client)

	// Check whether the version already exists. The conflict gate runs
	// *after* the dry-run dispatch below: a dry run mutates nothing, so it
	// must work even when the version is present — which is exactly when you
	// want a preview before deciding to --force.
	exists, err := registry.CheckVersionExists(ctx, coords)
	if err != nil {
		return fail(out, "publish", cob.ExitError, "checking version: %s", err)
	}

	sources, err := buildSources(m, client)
	if err != nil {
		return fail(out, "publish", cob.ExitError, "%s", err)
	}

	if dryRun {
		out.Header("Publishing %s/%s@%s -> %s/%s (dry run)", m.Namespace, m.Package, version, m.Domain, m.Repository)
		return runDryRun(ctx, coords, sources, out)
	}

	if exists && !force {
		return fail(out, "publish", cob.ExitConflict, "version %s already exists in %s/%s. Use --force to overwrite.", version, m.Domain, m.Repository)
	}

	out.Header("Publishing %s/%s@%s -> %s/%s", m.Namespace, m.Package, version, m.Domain, m.Repository)

	proceed, err := confirmAction(ctx, yes, fmt.Sprintf("Publish %d assets?", len(sources)))
	if err != nil {
		return fail(out, "publish", cob.ExitError, "%s", err)
	}
	if !proceed {
		fmt.Fprintln(os.Stderr, "Aborted.")
		return nil
	}

	// Force: delete existing version first.
	if exists && force {
		if err := publisher.DeleteVersion(ctx, coords); err != nil {
			out.Error("deleting existing version: %s", err)
			return &ExitError{Code: cob.ExitError}
		}
	}

	start := time.Now()
	result := &cob.CommandResult{
		Command:    "publish",
		Package:    fmt.Sprintf("%s/%s@%s", m.Namespace, m.Package, version),
		Repository: fmt.Sprintf("%s/%s", m.Domain, m.Repository),
		Status:     "ok",
	}

	// Real sources publish concurrently as Unfinished; the provenance
	// document is published last with unfinished=false, which both records
	// what was published and flips the version to Published.
	concurrency = resolveConcurrency(concurrency, out)
	results, ok := runConcurrent(len(sources), concurrency,
		func(i int) (*cob.AssetResult, error) {
			ns := sources[i]
			out.AssetStart(ns.Name, ns.Source.URI(), 0)
			ar, err := publisher.PublishAsset(ctx, coords, ns.Name, ns.Source, true)
			if err != nil {
				out.AssetFail(ns.Name, ns.Source.URI(), err)
				return ar, err
			}
			out.AssetOK(ar, ns.Source.URI())
			return ar, nil
		})

	for _, r := range results {
		if r != nil && r.Error == nil {
			result.Assets = append(result.Assets, *r)
			result.TotalSize += r.Size
		}
	}

	if !ok {
		result.DurationMs = time.Since(start).Milliseconds()
		result.Status = "error"
		result.Error = firstResultError(results)
		out.Error("%s\n  Published %d of %d assets. Version is in unfinished state.\n  Re-run with --force to delete and retry.",
			result.Error, len(result.Assets), len(sources))
		out.CommandResult(result)
		return &ExitError{Code: cob.ExitError}
	}

	// Build + publish provenance (the finalizer): one publish chain event
	// plus, per asset, where it physically came from.
	prov := &cob.Provenance{Package: fmt.Sprintf("%s/%s", m.Namespace, m.Package)}
	for i, ns := range sources {
		entry := cob.ProvenanceEntry{
			Key:    ns.Name,
			Source: ns.Source.URI(),
			Asset:  ns.Source.Filename(),
			SHA256: results[i].SHA256,
			Size:   results[i].Size,
		}
		if o, oerr := ns.Source.Origin(ctx); oerr == nil {
			entry.Origin = o
		}
		prov.Assets = append(prov.Assets, entry)
	}
	prov.Chain = []cob.ProvenanceEvent{{
		Event:          "publish",
		Repository:     fmt.Sprintf("%s/%s", m.Domain, m.Repository),
		Version:        version,
		Time:           cob.NowStamp(),
		CobVersion:     buildVersion,
		Region:         client.Region,
		ManifestSHA256: m.SHA256(),
		Actor:          client.CallerIdentity(ctx),
	}}
	if err := finalizeProvenance(ctx, publisher, coords, prov, out, result, start,
		"Assets published but provenance/finalize failed. Version is in unfinished state."); err != nil {
		return err
	}

	out.Summary("Published %d assets (%s) in %s",
		len(result.Assets), output.FormatSize(result.TotalSize), output.FormatDuration(result.DurationMs))
	return out.CommandResult(result)
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

func runDryRun(ctx context.Context, coords *cob.PackageCoordinates, sources []NamedSource, out *output.Writer) error {
	result := &cob.CommandResult{
		Command:    "publish",
		Package:    fmt.Sprintf("%s/%s@%s", coords.Namespace, coords.Package, coords.Version),
		Repository: fmt.Sprintf("%s/%s", coords.Domain, coords.Repository),
		Status:     "ok",
	}

	var failures int
	for _, ns := range sources {
		meta, err := ns.Source.Resolve(ctx)
		if err != nil {
			out.AssetFail(ns.Name, ns.Source.URI(), err)
			ar := cob.AssetResult{Name: ns.Name, Source: ns.Source.URI(), Method: "buffered"}
			ar.SetError(err)
			result.Assets = append(result.Assets, ar)
			failures++
			continue
		}
		ar := cob.AssetResult{
			Name:   ns.Name,
			Source: ns.Source.URI(),
			Size:   meta.Size,
			SHA256: meta.SHA256,
			Method: "buffered",
		}
		out.AssetOK(&ar, ns.Source.URI())
		result.Assets = append(result.Assets, ar)
		result.TotalSize += ar.Size
	}

	verified := len(sources) - failures
	if failures > 0 {
		result.Status = "error"
		result.Error = fmt.Sprintf("%d of %d sources failed verification", failures, len(sources))
		out.Summary("Dry run complete. %d of %d sources verified, %d failed.", verified, len(sources), failures)
		out.CommandResult(result)
		return &ExitError{Code: cob.ExitError}
	}
	out.Summary("Dry run complete. All %d sources verified.", len(sources))
	return out.CommandResult(result)
}
