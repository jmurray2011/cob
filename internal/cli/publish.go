package cli

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/jmurray2011/cob/internal/cob"
	"github.com/jmurray2011/cob/internal/manifest"
	"github.com/jmurray2011/cob/internal/output"
)

func newPublishCmd() *cobra.Command {
	var (
		flagVersion     string
		flagForce       bool
		flagDryRun      bool
		flagYes         bool
		flagResume      bool
		flagConcurrency int
	)

	cmd := &cobra.Command{
		Use:   "publish <manifest>",
		Short: "Publish a package from a manifest",
		Long:  "Reads a manifest file, resolves variables, pulls from each source, and publishes to CodeArtifact.",
		Example: `  # publish a manifest at an explicit version
  cob publish ./my-package.yaml --version 2.1.0

  # CI: skip the confirmation prompt
  cob publish ./my-package.yaml --version 2.1.0 --yes

  # finish an interrupted publish without re-uploading what already landed
  cob publish ./my-package.yaml --version 2.1.0 --resume`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPublish(cmd.Context(), args[0], flagVersion, flagForce, flagDryRun, flagYes, flagResume, flagConcurrency)
		},
	}

	cmd.Flags().StringVar(&flagVersion, "version", "", "Package version (required, or set COB_VERSION)")
	cmd.Flags().BoolVar(&flagForce, "force", false, "Overwrite existing version")
	cmd.Flags().BoolVar(&flagDryRun, "dry-run", false, "Verify sources exist, show plan, don't publish")
	cmd.Flags().BoolVar(&flagYes, "yes", false, "Skip confirmation")
	cmd.Flags().BoolVar(&flagResume, "resume", false, "Continue an unfinished publish: upload only the missing assets")
	cmd.Flags().IntVar(&flagConcurrency, "concurrency", defaultConcurrency, "Max assets transferred in parallel (1 = sequential)")

	return cmd
}

func runPublish(ctx context.Context, manifestPath, versionFlag string, force, dryRun, yes, resume bool, concurrency int) error {
	out := newWriter(flagJSON)

	if resume && force {
		return fail(out, "publish", cob.ExitError, "--resume and --force are mutually exclusive (one continues a version, the other replaces it)")
	}

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

	// Asset sizes aren't known until each source is resolved, so the meter
	// shows bytes/rate without a percentage.
	meter := newProgressMeter(out, 0)
	publisher.Progress = meter.add
	defer meter.finish()

	// Read the version's current state. The conflict/resume gate runs
	// *after* the dry-run dispatch below: a dry run mutates nothing, so it
	// must work whatever state the version is in.
	status, exists, err := registry.VersionStatus(ctx, coords)
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

	// present holds the assets already in an unfinished version being
	// resumed; for a normal publish it stays nil, so nothing is skipped.
	var present map[string]cob.AssetSummary
	switch {
	case resume:
		if !exists {
			return fail(out, "publish", cob.ExitError,
				"no version %s in %s/%s to resume — run publish without --resume", version, m.Domain, m.Repository)
		}
		if status != "Unfinished" { // CodeArtifact PackageVersionStatus
			return fail(out, "publish", cob.ExitError,
				"version %s is not resumable (status %q) — only an unfinished publish can be resumed; use --force to overwrite", version, status)
		}
		listed, lerr := registry.ListAssets(ctx, coords)
		if lerr != nil {
			return fail(out, "publish", cob.ExitError, "listing already-published assets: %s", lerr)
		}
		present = make(map[string]cob.AssetSummary, len(listed))
		for _, a := range listed {
			present[a.Name] = a
		}
	case exists && !force:
		return fail(out, "publish", cob.ExitConflict,
			"version %s already exists in %s/%s. Use --force to overwrite, or --resume to continue an unfinished publish.",
			version, m.Domain, m.Repository)
	}

	// todo is how many sources still need uploading; the rest of `present`
	// are already in the version (a no-op for a non-resume publish).
	todo := 0
	for _, ns := range sources {
		if _, done := present[ns.Source.Filename()]; !done {
			todo++
		}
	}

	verb, prompt := "Publishing", fmt.Sprintf("Publish %d assets?", len(sources))
	if resume {
		verb = "Resuming"
		prompt = fmt.Sprintf("Resume publish — upload %d of %d assets?", todo, len(sources))
	}
	out.Header("%s %s/%s@%s -> %s/%s", verb, m.Namespace, m.Package, version, m.Domain, m.Repository)

	proceed, err := confirmAction(ctx, yes, prompt)
	if err != nil {
		return fail(out, "publish", cob.ExitError, "%s", err)
	}
	if !proceed {
		fmt.Fprintln(os.Stderr, "Aborted.")
		return nil
	}

	// Force: delete the existing version first. (resume keeps it.)
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
	// what was published and flips the version to Published. On --resume an
	// asset already in the version is skipped — CodeArtifact validated its
	// SHA-256 on the prior upload, so a present asset is complete.
	concurrency = resolveConcurrency(concurrency, out)
	results, ok := runConcurrent(len(sources), concurrency,
		func(i int) (*cob.AssetResult, error) {
			ns := sources[i]
			if a, done := present[ns.Source.Filename()]; done {
				out.AssetSkipped(ns.Name)
				return &cob.AssetResult{
					Name: ns.Name, Source: ns.Source.URI(),
					SHA256: a.SHA256, Size: a.Size, Method: "skipped",
				}, nil
			}
			out.AssetStart(ns.Name, ns.Source.URI(), 0)
			ar, err := publisher.PublishAsset(ctx, coords, ns.Name, ns.Source, true)
			if err != nil {
				out.AssetFail(ns.Name, ns.Source.URI(), err)
				return ar, err
			}
			out.AssetOK(ar, ns.Source.URI())
			return ar, nil
		})

	// Record every asset that ran — uploaded, skipped, or failed — so a
	// --json consumer sees the full picture. Only fresh uploads count
	// toward the transferred byte total.
	uploaded, skipped := 0, 0
	for _, r := range results {
		if r == nil {
			continue // never scheduled: an earlier task failed first
		}
		result.Assets = append(result.Assets, *r)
		switch {
		case r.Error != nil:
		case r.Method == "skipped":
			skipped++
		default:
			result.TotalSize += r.Size
			uploaded++
		}
	}

	if !ok {
		result.DurationMs = time.Since(start).Milliseconds()
		result.Status = "error"
		result.Error = firstResultError(results)
		out.Error("%s\n  %d of %d assets in place. Version is unfinished — re-run with --resume to continue.",
			result.Error, uploaded+skipped, len(sources))
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
		// Don't re-observe Origin for an asset that was skipped on
		// --resume: ns.Source was never Opened, so a fresh HeadObject now
		// would record S3 state at *resume* time, not upload time. The
		// interrupted publish never wrote provenance, so there's no prior
		// Origin to carry forward — leave it nil. The bytes are still
		// SHA-validated; we just don't have the upload-time source state.
		if results[i].Method != "skipped" {
			if o, oerr := ns.Source.Origin(ctx); oerr == nil {
				entry.Origin = o
			}
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
		"Assets published but provenance/finalize failed. Version is unfinished — re-run with --resume to finalize it."); err != nil {
		return err
	}

	if skipped > 0 {
		out.Summary("Resumed: %d uploaded, %d already present (%s) in %s",
			uploaded, skipped, output.FormatSize(result.TotalSize), output.FormatDuration(result.DurationMs))
	} else {
		out.Summary("Published %d assets (%s) in %s",
			len(result.Assets), output.FormatSize(result.TotalSize), output.FormatDuration(result.DurationMs))
	}
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
