package cli

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/jmurray2011/cob/internal/cob"
	"github.com/jmurray2011/cob/internal/manifest"
	"github.com/jmurray2011/cob/internal/output"

	"github.com/jmurray2011/cob/internal/cliutil"
)

func newPublishCmd(cfg *cliutil.Config) *cobra.Command {
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
			return runPublish(cmd.Context(), cfg, args[0], flagVersion, flagForce, flagDryRun, flagYes, flagResume, flagConcurrency)
		},
	}

	cmd.Flags().StringVar(&flagVersion, "version", "", "Package version (required, or set COB_VERSION)")
	cmd.Flags().BoolVarP(&flagForce, "force", "f", false, "Overwrite existing version")
	cmd.Flags().BoolVar(&flagDryRun, "dry-run", false, "Verify sources exist, show plan, don't publish")
	cmd.Flags().BoolVarP(&flagYes, "yes", "y", false, "Skip confirmation")
	cmd.Flags().BoolVar(&flagResume, "resume", false, "Continue an unfinished publish: upload only the missing assets")
	cmd.Flags().IntVar(&flagConcurrency, "concurrency", cliutil.DefaultConcurrency, "Max assets transferred in parallel (1 = sequential; clamped to [1,32] to avoid CodeArtifact throttling — a warning prints if a passed value was changed)")

	return cmd
}

func runPublish(ctx context.Context, cfg *cliutil.Config, manifestPath, versionFlag string, force, dryRun, yes, resume bool, concurrency int) error {
	out := cliutil.NewWriter(cfg)
	defer out.Close()
	ctx, cancel := cliutil.Interruptable(ctx, cfg, out)
	defer cancel()

	if resume && force {
		return cliutil.Fail(out, "publish", cob.ExitError, "--resume and --force are mutually exclusive (one continues a version, the other replaces it)")
	}

	version, err := cliutil.ResolveVersion(versionFlag)
	if err != nil {
		return cliutil.Fail(out, "publish", cob.ExitError, "%s", err)
	}
	if version == "latest" {
		return cliutil.Fail(out, "publish", cob.ExitError, "cannot publish to @latest, provide an explicit version")
	}

	m, err := manifest.Load(manifestPath)
	if err != nil {
		return cliutil.Fail(out, "publish", cob.ExitError, "%s", err)
	}
	cliutil.WarnManifestOverrides(m, out)
	// Implicit pre-flight lint — same checks `cob diff <manifest>`
	// exposes user-side. Bails before any AWS work if the manifest is
	// broken (bad URI, missing local file, reserved asset name, basename
	// collision). The `cob validate` command went away because this
	// guard ensures every manifest-based command refuses a bad manifest
	// in the same way; there's no longer a "ran lint, didn't run lint"
	// distinction to remember.
	if err := cliutil.ValidateManifest(m, version); err != nil {
		return cliutil.Fail(out, "publish", cob.ExitError, "%s", err)
	}

	if err := m.ResolveVariables(version); err != nil {
		return cliutil.Fail(out, "publish", cob.ExitError, "%s", err)
	}

	coords := &cob.PackageCoordinates{
		Domain:     m.Domain,
		Repository: m.Repository,
		Namespace:  m.Namespace,
		Package:    m.Package,
		Version:    version,
	}

	client, err := cliutil.DialClient(ctx, cfg)
	if err != nil {
		return cliutil.Fail(out, "publish", cob.ExitError, "%s", err)
	}

	publisher := cob.NewPublisher(client)
	registry := cob.NewRegistry(client)

	// Asset sizes aren't known until each source resolves, so the live
	// renderer's "total" row stays open-ended (bytes/rate, no percent).
	publisher.Progress = out.AssetProgress

	// Read the version's current state. The conflict/resume gate runs
	// *after* the dry-run dispatch below: a dry run mutates nothing, so it
	// must work whatever state the version is in.
	status, exists, err := registry.VersionStatus(ctx, coords)
	if err != nil {
		return cliutil.Fail(out, "publish", cob.ExitError, "checking version: %s", err)
	}

	sources, err := cliutil.BuildSources(m, client)
	if err != nil {
		return cliutil.Fail(out, "publish", cob.ExitError, "%s", err)
	}
	// Tell the live renderer how many rows to expect; bytes are 0
	// because sources resolve lazily and we don't know sizes until they
	// stream.
	out.AssetsExpected(len(sources), 0)

	if dryRun {
		out.Header("Publishing %s/%s@%s -> %s/%s (dry run)", m.Namespace, m.Package, version, m.Domain, m.Repository)
		return runDryRun(ctx, coords, sources, client, out)
	}

	present, code, gerr := gatePublish(ctx, registry, coords, status, exists, resume, force)
	if gerr != nil {
		return cliutil.Fail(out, "publish", code, "%s", gerr)
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

	proceed, err := cliutil.ConfirmAction(ctx, yes, prompt)
	if err != nil {
		return cliutil.Fail(out, "publish", cob.ExitError, "%s", err)
	}
	if !proceed {
		out.Aborted("publish")
		return nil
	}

	// Force: delete the existing version first. (resume keeps it.)
	if exists && force {
		if err := publisher.DeleteVersion(ctx, coords); err != nil {
			out.Error("deleting existing version: %s", err)
			return &cliutil.ExitError{Code: cob.ExitError}
		}
	}

	start := time.Now()
	result := &cob.CommandResult{
		Command:    "publish",
		Package:    fmt.Sprintf("%s/%s@%s", m.Namespace, m.Package, version),
		Repository: fmt.Sprintf("%s/%s", m.Domain, m.Repository),
		Status:     "ok",
	}
	cliutil.FillClientMeta(ctx, client, result)

	// Real sources publish concurrently as Unfinished; the provenance
	// document is published last with unfinished=false, which both records
	// what was published and flips the version to Published. On --resume an
	// asset already in the version is skipped — CodeArtifact validated its
	// SHA-256 on the prior upload, so a present asset is complete.
	// Pre-fill skipped results (no concurrency needed) and collect the
	// indices that actually need an upload.
	results := make([]*cob.AssetResult, len(sources))
	var todos []int
	for i, ns := range sources {
		if a, done := present[ns.Source.Filename()]; done {
			out.AssetSkipped(ns.Name)
			results[i] = &cob.AssetResult{
				Name: ns.Name, Source: ns.Source.URI(),
				SHA256: a.SHA256, Size: a.Size, Method: "skipped",
			}
			continue
		}
		todos = append(todos, i)
	}

	uploadOne := func(ctx context.Context, i int) (*cob.AssetResult, error) {
		ns := sources[i]
		out.AssetStart(ns.Name, ns.Source.URI(), 0)
		ar, err := publisher.PublishAsset(ctx, coords, ns.Name, ns.Source, true)
		if err != nil {
			out.AssetFail(ns.Name, ns.Source.URI(), err)
			return ar, err
		}
		out.AssetOK(ar, ns.Source.URI())
		return ar, nil
	}

	// Sync-publish the first to-upload asset so concurrent goroutines don't
	// race CodeArtifact's implicit version creation. Once that returns the
	// version exists; the rest fan out safely.
	ok := true
	if len(todos) > 0 {
		first := todos[0]
		ar, err := uploadOne(ctx, first)
		results[first] = ar
		if err != nil {
			ok = false
		}
		todos = todos[1:]
	}

	if ok && len(todos) > 0 {
		concurrency = cliutil.ResolveConcurrency(concurrency, out)
		rest, restOk := cliutil.RunConcurrent(ctx, len(todos), concurrency, func(ctx context.Context, j int) (*cob.AssetResult, error) {
			return uploadOne(ctx, todos[j])
		})
		for j, r := range rest {
			results[todos[j]] = r
		}
		ok = restOk
	}

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
		// Interrupted (Ctrl-C in the live TUI) is a "could not be
		// completed" the same way an upload error is, but distinguish
		// it via exit code 130 — CI gates that retry on transient
		// errors should not retry a deliberate cancellation.
		if out.Interrupted() {
			result.Status = "interrupted"
			out.Error("Interrupted.\n  %d of %d assets in place. Version is unfinished — re-run with --resume to continue.",
				uploaded+skipped, len(sources))
			out.CommandResult(result)
			return &cliutil.ExitError{Code: cob.ExitInterrupted}
		}
		result.Status = "error"
		result.Error = firstResultError(results)
		out.Error("%s\n  %d of %d assets in place. Version is unfinished — re-run with --resume to continue.",
			result.Error, uploaded+skipped, len(sources))
		out.CommandResult(result)
		return &cliutil.ExitError{Code: cob.ExitError}
	}

	prov := buildPublishProvenance(ctx, m, version, sources, results, client, cfg.Version)
	if err := cliutil.FinalizeProvenance(ctx, publisher, coords, prov, out, result, start,
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

func runDryRun(ctx context.Context, coords *cob.PackageCoordinates, sources []cliutil.NamedSource, client *cob.Client, out *output.Writer) error {
	result := &cob.CommandResult{
		Command:    "publish",
		Package:    fmt.Sprintf("%s/%s@%s", coords.Namespace, coords.Package, coords.Version),
		Repository: fmt.Sprintf("%s/%s", coords.Domain, coords.Repository),
		Status:     "ok",
	}
	cliutil.FillClientMeta(ctx, client, result)

	var failures int
	for _, ns := range sources {
		meta, err := ns.Source.Resolve(ctx)
		if err != nil {
			out.AssetFail(ns.Name, ns.Source.URI(), err)
			ar := cob.AssetResult{Name: ns.Name, Source: ns.Source.URI(), Method: "spilled"}
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
			Method: "spilled",
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
		return &cliutil.ExitError{Code: cob.ExitError}
	}
	out.Summary("Dry run complete. All %d sources verified.", len(sources))
	return out.CommandResult(result)
}

// gatePublish enforces publish's precondition: a fresh version (default), an
// existing version with --force, or an Unfinished version with --resume. For
// --resume it also returns the assets already in the version (the caller
// will skip uploading those). exitCode is the code the caller should exit
// with if err is non-nil. Extracted from runPublish so it can be unit-tested
// without the full publish path.
func gatePublish(ctx context.Context, registry *cob.Registry, coords *cob.PackageCoordinates,
	status string, exists, resume, force bool) (present map[string]cob.AssetSummary, exitCode int, err error) {

	switch {
	case resume:
		if !exists {
			return nil, cob.ExitError, fmt.Errorf(
				"no version %s in %s/%s to resume — run publish without --resume",
				coords.Version, coords.Domain, coords.Repository)
		}
		if status != "Unfinished" {
			return nil, cob.ExitError, fmt.Errorf(
				"version %s is not resumable (status %q) — only an unfinished publish can be resumed; use --force to overwrite",
				coords.Version, status)
		}
		listed, lerr := registry.ListAssets(ctx, coords)
		if lerr != nil {
			return nil, cob.ExitError, fmt.Errorf("listing already-published assets: %w", lerr)
		}
		present = make(map[string]cob.AssetSummary, len(listed))
		for _, a := range listed {
			present[a.Name] = a
		}
	case exists && !force:
		return nil, cob.ExitConflict, fmt.Errorf(
			"version %s already exists in %s/%s -- use --force to overwrite, or --resume to continue an unfinished publish",
			coords.Version, coords.Domain, coords.Repository)
	}
	return present, cob.ExitOK, nil
}

// buildPublishProvenance assembles the cob-provenance.json document for a
// publish: one publish chain event plus, per asset, where it physically came
// from. Origin is omitted for an asset that was skipped on --resume — its
// upload-time source state is not knowable at resume time.
func buildPublishProvenance(ctx context.Context, m *manifest.Manifest, version string,
	sources []cliutil.NamedSource, results []*cob.AssetResult, client *cob.Client, cobVersion string) *cob.Provenance {

	prov := &cob.Provenance{Package: fmt.Sprintf("%s/%s", m.Namespace, m.Package)}
	for i, ns := range sources {
		entry := cob.ProvenanceEntry{
			Key:    ns.Name,
			Source: ns.Source.URI(),
			Asset:  ns.Source.Filename(),
			SHA256: results[i].SHA256,
			Size:   results[i].Size,
		}
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
		CobVersion:     cobVersion,
		Region:         client.Region,
		ManifestSHA256: m.SHA256(),
		Actor:          client.CallerIdentity(ctx),
	}}
	return prov
}
