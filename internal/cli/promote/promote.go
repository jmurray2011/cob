package promote

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

func NewCmd(cfg *cliutil.Config) *cobra.Command {
	var (
		flagVersion     string
		flagTo          string
		flagForce       bool
		flagYes         bool
		flagDryRun      bool
		flagResume      bool
		flagConcurrency int
	)

	cmd := &cobra.Command{
		Use:   "promote <manifest|coordinates>",
		Short: "Copy a package version between repositories",
		Long:  "Copies a package version from one repo to another, streaming in constant memory (via a temp file).",
		Example: `  # promote a version to the next stage
  cob promote acme/dev/tools/my-app@2.1.0 --to staging

  # preview the move without copying anything
  cob promote acme/dev/tools/my-app@2.1.0 --to staging --dry-run

  # finish an interrupted promote without re-copying what already landed
  cob promote acme/dev/tools/my-app@2.1.0 --to staging --resume`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return Run(cmd.Context(), cfg, args[0], flagVersion, flagTo, flagForce, flagYes, flagDryRun, flagResume, flagConcurrency)
		},
	}

	cmd.Flags().StringVar(&flagVersion, "version", "", "Specific version (required with manifest)")
	cmd.Flags().StringVar(&flagTo, "to", "", "Destination repository (required)")
	cmd.Flags().BoolVarP(&flagForce, "force", "f", false, "Overwrite if version exists in destination")
	cmd.Flags().BoolVarP(&flagYes, "yes", "y", false, "Skip confirmation")
	cmd.Flags().BoolVar(&flagDryRun, "dry-run", false, "Show what would be promoted, copy nothing")
	cmd.Flags().BoolVar(&flagResume, "resume", false, "Continue an unfinished promote: copy only the missing assets")
	cmd.Flags().IntVar(&flagConcurrency, "concurrency", cliutil.DefaultConcurrency, "Max assets transferred in parallel (1 = sequential; clamped to [1,32] to avoid CodeArtifact throttling — a warning prints if a passed value was changed)")
	cmd.MarkFlagRequired("to")

	return cmd
}

func Run(ctx context.Context, cfg *cliutil.Config, target, versionFlag, toRepo string, force, yes, dryRun, resume bool, concurrency int) error {
	out := cliutil.NewWriter(cfg)
	defer out.Close()
	ctx, cancel := cliutil.Interruptable(ctx, cfg, out)
	defer cancel()

	if resume && force {
		return cliutil.Fail(out, "promote", cob.ExitError, "--resume and --force are mutually exclusive (one continues a version, the other replaces it)")
	}

	client, err := cliutil.DialClient(ctx, cfg, out)
	if err != nil {
		return cliutil.Fail(out, "promote", cob.ExitError, "%s", err)
	}

	var coords *cob.PackageCoordinates
	var srcRepo string

	if cliutil.IsManifestPath(target) {
		version, err := cliutil.ResolveVersion(versionFlag)
		if err != nil {
			return cliutil.Fail(out, "promote", cob.ExitError, "%s", err)
		}

		m, err := manifest.Load(target)
		if err != nil {
			return cliutil.Fail(out, "promote", cob.ExitError, "%s", err)
		}
		cliutil.WarnManifestOverrides(m, out)
		// Implicit pre-flight lint — see runPublish for rationale. The
		// manifest is the source of truth for the source repo, so a
		// broken one shouldn't even reach promote stage inference.
		if err := cliutil.ValidateManifest(m, version); err != nil {
			return cliutil.Fail(out, "promote", cob.ExitError, "%s", err)
		}

		// Infer source repo from promote stages.
		srcRepo, err = m.InferPromoteSource(toRepo)
		if err != nil {
			return cliutil.Fail(out, "promote", cob.ExitError, "%s", err)
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
			return cliutil.Fail(out, "promote", cob.ExitError, "%s", err)
		}
		if coords.Version == "" {
			return cliutil.Fail(out, "promote", cob.ExitError, "version is required for promote (use domain/repo/ns/pkg@version or @latest)")
		}
		srcRepo = coords.Repository
	}

	registry := cob.NewRegistry(client)

	// Resolve @latest from the source repo.
	coords.Repository = srcRepo
	if err := cliutil.ResolveLatestIfNeeded(ctx, coords, registry, out); err != nil {
		return cliutil.Fail(out, "promote", cliutil.CodeFor(err), "%s", err)
	}
	out.Verbosef("promote %s/%s@%s: %s -> %s", coords.Namespace, coords.Package, coords.Version, srcRepo, toRepo)

	// Check if version exists in destination.
	destCoords := &cob.PackageCoordinates{
		Domain:     coords.Domain,
		Repository: toRepo,
		Namespace:  coords.Namespace,
		Package:    coords.Package,
		Version:    coords.Version,
	}
	status, exists, err := registry.VersionStatus(ctx, destCoords)
	if err != nil {
		return cliutil.Fail(out, "promote", cob.ExitError, "checking destination: %s", err)
	}

	// Dry-run before the conflict/resume gate: a preview mutates nothing and
	// is most useful precisely when the destination version already exists.
	if dryRun {
		return runPromoteDryRun(ctx, client, cob.NewPromoter(client), coords, srcRepo, toRepo, exists, out)
	}

	present, code, gerr := gatePromote(ctx, registry, destCoords, toRepo, status, exists, resume, force)
	if gerr != nil {
		return cliutil.Fail(out, "promote", code, "%s", gerr)
	}

	verb := "Promoting"
	prompt := fmt.Sprintf("Promote to %s?", toRepo)
	if resume {
		verb = "Resuming promote of"
		prompt = fmt.Sprintf("Resume promote to %s?", toRepo)
	}
	out.Header("%s %s/%s@%s: %s -> %s",
		verb, coords.Namespace, coords.Package, coords.Version, srcRepo, toRepo)

	proceed, err := cliutil.ConfirmAction(ctx, yes, prompt)
	if err != nil {
		return cliutil.Fail(out, "promote", cob.ExitError, "%s", err)
	}
	if !proceed {
		out.Aborted("promote")
		return nil
	}

	if exists && force {
		publisher := cob.NewPublisher(client)
		if err := publisher.DeleteVersion(ctx, destCoords); err != nil {
			return cliutil.Fail(out, "promote", cob.ExitError, "deleting existing version in destination: %s", err)
		}
	}

	promoter := cob.NewPromoter(client)
	assetNames, err := promoter.ListAssetsToPromote(ctx, coords, srcRepo)
	if err != nil {
		return cliutil.Fail(out, "promote", cob.ExitError, "%s", err)
	}

	// Promote doesn't know asset sizes up front; each row sizes itself
	// as bytes flow. AssetsExpected scopes the live view's row count.
	out.AssetsExpected(len(assetNames), 0)
	promoter.Progress = out.AssetProgress

	cmdResult := &cob.CommandResult{
		Command:    "promote",
		Package:    fmt.Sprintf("%s/%s@%s", coords.Namespace, coords.Package, coords.Version),
		Repository: fmt.Sprintf("%s -> %s", srcRepo, toRepo),
		Status:     "ok",
	}
	cliutil.FillClientMeta(ctx, client, cmdResult)

	// The provenance asset is not copied verbatim — it is read, a promote
	// link is appended, and the updated document is written to the
	// destination as the finalizer.
	realNames := make([]string, 0, len(assetNames))
	for _, n := range assetNames {
		if n != cob.ProvenanceFile {
			realNames = append(realNames, n)
		}
	}

	start := time.Now()

	// Pre-fill skipped results (on --resume) and collect the names that
	// still need copying.
	results := make([]*cob.AssetResult, len(realNames))
	var todos []int
	for i, name := range realNames {
		if a, done := present[name]; done {
			out.AssetSkipped(name)
			out.Verbosef("skip %s: already present in %s (resume)", name, toRepo)
			results[i] = &cob.AssetResult{Name: name, Kind: cob.KindTransfer, SHA256: a.SHA256, Size: a.Size, Method: cob.TransferSkipped}
			continue
		}
		todos = append(todos, i)
	}

	promoteOne := func(ctx context.Context, i int) (*cob.AssetResult, error) {
		name := realNames[i]
		out.AssetStart(name, "", 0)
		ar, err := promoter.PromoteAsset(ctx, coords, srcRepo, toRepo, name, true)
		if err != nil {
			out.AssetFail(name, "", err)
			return ar, err
		}
		out.Verbosef("%s: %s %s in %dms", name, ar.Method, output.FormatSize(ar.Size), ar.DurationMs)
		out.AssetOK(ar, "")
		return ar, nil
	}

	// Sync-promote the first to-copy asset so concurrent goroutines don't
	// race CodeArtifact's implicit version creation. On --resume the dest
	// version already exists, so this is a no-op race-wise — still cheap.
	ok := true
	if len(todos) > 0 {
		first := todos[0]
		ar, err := promoteOne(ctx, first)
		results[first] = ar
		if err != nil {
			ok = false
		}
		todos = todos[1:]
	}

	if ok && len(todos) > 0 {
		concurrency = cliutil.ResolveConcurrency(concurrency, out)
		rest, restOk := cliutil.RunConcurrent(ctx, len(todos), concurrency, func(ctx context.Context, j int) (*cob.AssetResult, error) {
			return promoteOne(ctx, todos[j])
		})
		for j, r := range rest {
			results[todos[j]] = r
		}
		ok = restOk
	}

	// Record every asset that ran — copied, skipped, or failed — so a --json
	// consumer sees the full picture. Only fresh copies count toward bytes.
	copied, skipped := 0, 0
	for _, r := range results {
		if r == nil {
			continue
		}
		cmdResult.Assets = append(cmdResult.Assets, *r)
		switch {
		case r.Error != nil:
		case r.Method == "skipped":
			skipped++
		default:
			cmdResult.TotalSize += r.Size
			copied++
		}
	}

	if !ok {
		cmdResult.DurationMs = time.Since(start).Milliseconds()
		// Interrupted (Ctrl-C in the live TUI) exits 130 so CI retries
		// don't pick it up as a transient failure — see ExitInterrupted.
		if out.Interrupted() {
			cmdResult.Status = "interrupted"
			out.Error("Interrupted.\n  %d of %d assets in place in %s. Version is unfinished — re-run with --resume to continue.",
				copied+skipped, len(realNames), toRepo)
			out.CommandResult(cmdResult)
			return &cliutil.ExitError{Code: cob.ExitInterrupted}
		}
		cmdResult.Status = "error"
		cmdResult.Error = cliutil.FirstResultError(results)
		out.Error("%s\n  %d of %d assets in place in %s. Version is unfinished — re-run with --resume to continue.",
			cmdResult.Error, copied+skipped, len(realNames), toRepo)
		out.CommandResult(cmdResult)
		return &cliutil.ExitError{Code: cob.ExitError}
	}

	prov, err := carryForwardProvenance(ctx, client, coords, srcRepo, toRepo, realNames, results, cfg.Version)
	if err != nil {
		return cliutil.Fail(out, "promote", cob.ExitError, "%s", err)
	}

	if err := cliutil.FinalizeProvenance(ctx, cob.NewPublisher(client), destCoords, prov, out, cmdResult, start,
		"Assets promoted but provenance/finalize failed. Version is unfinished — re-run with --resume to finalize it."); err != nil {
		return err
	}

	if skipped > 0 {
		out.Summary("Resumed: %d copied, %d already present in %s in %s",
			copied, skipped, toRepo, output.FormatDuration(cmdResult.DurationMs))
	} else {
		out.Summary("Promoted %d assets in %s", len(cmdResult.Assets), output.FormatDuration(cmdResult.DurationMs))
	}
	return out.CommandResult(cmdResult)
}

// runPromoteDryRun lists what a promote would copy and exits without
// mutating anything. The destination conflict is already checked, so
// destExists here means the real run would proceed under --force.
func runPromoteDryRun(ctx context.Context, client *cob.Client, promoter *cob.Promoter, coords *cob.PackageCoordinates,
	srcRepo, toRepo string, destExists bool, out *output.Writer) error {

	out.Header("Promote (dry run) %s/%s@%s: %s -> %s",
		coords.Namespace, coords.Package, coords.Version, srcRepo, toRepo)

	assetNames, err := promoter.ListAssetsToPromote(ctx, coords, srcRepo)
	if err != nil {
		return cliutil.Fail(out, "promote", cliutil.CodeFor(err), "%s", err)
	}

	result := &cob.CommandResult{
		Command:    "promote",
		Package:    fmt.Sprintf("%s/%s@%s", coords.Namespace, coords.Package, coords.Version),
		Repository: fmt.Sprintf("%s -> %s", srcRepo, toRepo),
		Status:     "ok",
	}
	cliutil.FillClientMeta(ctx, client, result)
	for _, name := range assetNames {
		if name == cob.ProvenanceFile {
			continue // regenerated at promote time, not copied verbatim
		}
		out.Plain("  would promote %s", name)
		result.Assets = append(result.Assets, cob.AssetResult{Name: name, Kind: cob.KindDryRun, Method: cob.DryRunPreview})
	}
	if destExists {
		out.Warn("version already exists in %s; a real promote needs --force to overwrite it", toRepo)
	}
	out.Summary("Dry run: %d assets would be promoted to %s", len(result.Assets), toRepo)
	return out.CommandResult(result)
}

// carryForwardProvenance reads the source version's provenance, rebuilds its
// asset list to match what was actually promoted, and appends a promote
// event — producing the document to publish as the destination finalizer. A
// source that predates cob provenance yields a synthesized document; a
// transient fetch error is returned (it must not be read as "absent",
// which would discard real chain history). coords must already carry the
// source repository.
func carryForwardProvenance(ctx context.Context, client *cob.Client, coords *cob.PackageCoordinates,
	srcRepo, toRepo string, realNames []string, results []*cob.AssetResult, cobVersion string) (*cob.Provenance, error) {

	prov, err := cob.FetchProvenance(ctx, client.CodeArtifact, coords)
	if err != nil {
		return nil, fmt.Errorf("reading source provenance: %w", err)
	}
	if prov == nil {
		// Source wasn't cob-published (or pre-provenance): synthesize so the
		// chain still starts somewhere.
		prov = &cob.Provenance{Package: fmt.Sprintf("%s/%s", coords.Namespace, coords.Package)}
	}
	// The recorded asset list can drift from reality if an asset was added or
	// removed out-of-band after cob published the source. The destination
	// provenance must describe the destination, so rebuild the list from what
	// was actually promoted — keeping each recorded entry's Origin/Source
	// where the asset names still agree.
	prov.Assets = reconcilePromotedAssets(prov.Assets, realNames, results)

	if len(prov.Chain) == 0 { // pre-v2 doc or synthesized
		prov.Chain = []cob.ProvenanceEvent{{
			Event:      "publish",
			Repository: fmt.Sprintf("%s/%s", coords.Domain, srcRepo),
			Version:    coords.Version,
			Time:       cob.NowStamp(),
		}}
	}
	prov.Chain = append(prov.Chain, cob.ProvenanceEvent{
		Event:      "promote",
		From:       fmt.Sprintf("%s/%s", coords.Domain, srcRepo),
		To:         fmt.Sprintf("%s/%s", coords.Domain, toRepo),
		Time:       cob.NowStamp(),
		CobVersion: cobVersion,
		Region:     client.Region,
		Actor:      client.CallerIdentity(ctx),
	})
	return prov, nil
}

// reconcilePromotedAssets rebuilds the provenance asset list so it describes
// exactly what landed in the destination. realNames and results are
// authoritative for membership and content hash; the recorded entry, where
// the asset name still agrees, contributes the original publish's
// Key/Source/Origin (which a promote does not regenerate).
func reconcilePromotedAssets(recorded []cob.ProvenanceEntry, realNames []string, results []*cob.AssetResult) []cob.ProvenanceEntry {
	byName := make(map[string]cob.ProvenanceEntry, len(recorded))
	for _, e := range recorded {
		byName[e.Asset] = e
	}
	out := make([]cob.ProvenanceEntry, 0, len(realNames))
	for i, name := range realNames {
		entry := cob.ProvenanceEntry{Asset: name}
		if i < len(results) && results[i] != nil {
			entry.SHA256 = results[i].SHA256
			entry.Size = results[i].Size
		}
		if rec, ok := byName[name]; ok {
			entry.Key = rec.Key
			entry.Source = rec.Source
			entry.Origin = rec.Origin
		}
		out = append(out, entry)
	}
	return out
}

// gatePromote enforces promote's destination precondition: a fresh dest
// (default), an existing dest with --force, or an Unfinished dest with
// --resume. For --resume it also returns the assets already in the dest
// (the caller will skip copying those). Symmetric to gatePublish so the
// two stay in sync as resume logic evolves.
func gatePromote(ctx context.Context, registry *cob.Registry, destCoords *cob.PackageCoordinates,
	toRepo, status string, exists, resume, force bool) (present map[string]cob.AssetSummary, exitCode int, err error) {

	switch {
	case resume:
		if !exists {
			return nil, cob.ExitError, fmt.Errorf(
				"no version %s in %s to resume — run promote without --resume",
				destCoords.Version, toRepo)
		}
		if status != "Unfinished" {
			return nil, cob.ExitError, fmt.Errorf(
				"version %s in %s is not resumable (status %q) — only an unfinished promote can be resumed; use --force to overwrite",
				destCoords.Version, toRepo, status)
		}
		listed, lerr := registry.ListAssets(ctx, destCoords)
		if lerr != nil {
			return nil, cob.ExitError, fmt.Errorf("listing already-promoted assets: %w", lerr)
		}
		present = make(map[string]cob.AssetSummary, len(listed))
		for _, a := range listed {
			present[a.Name] = a
		}
	case exists && !force:
		return nil, cob.ExitConflict, fmt.Errorf(
			"version %s already exists in %s -- use --force to overwrite, or --resume to continue an unfinished promote",
			destCoords.Version, toRepo)
	}
	return present, cob.ExitOK, nil
}
