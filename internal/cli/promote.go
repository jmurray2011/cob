package cli

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/jmurray2011/cob/internal/cob"
	"github.com/jmurray2011/cob/internal/manifest"
	"github.com/jmurray2011/cob/internal/output"
)

func newPromoteCmd() *cobra.Command {
	var (
		flagVersion     string
		flagTo          string
		flagForce       bool
		flagYes         bool
		flagDryRun      bool
		flagConcurrency int
	)

	cmd := &cobra.Command{
		Use:   "promote <manifest|coordinates>",
		Short: "Copy a package version between repositories",
		Long:  "Copies a package version from one repo to another, streaming in constant memory (via a temp file).",
		Example: `  # promote a version to the next stage
  cob promote acme/dev/tools/my-app@2.1.0 --to staging

  # preview the move without copying anything
  cob promote acme/dev/tools/my-app@2.1.0 --to staging --dry-run`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPromote(cmd.Context(), args[0], flagVersion, flagTo, flagForce, flagYes, flagDryRun, flagConcurrency)
		},
	}

	cmd.Flags().StringVar(&flagVersion, "version", "", "Specific version (required with manifest)")
	cmd.Flags().StringVar(&flagTo, "to", "", "Destination repository (required)")
	cmd.Flags().BoolVar(&flagForce, "force", false, "Overwrite if version exists in destination")
	cmd.Flags().BoolVar(&flagYes, "yes", false, "Skip confirmation")
	cmd.Flags().BoolVar(&flagDryRun, "dry-run", false, "Show what would be promoted, copy nothing")
	cmd.Flags().IntVar(&flagConcurrency, "concurrency", defaultConcurrency, "Max assets transferred in parallel (1 = sequential)")
	cmd.MarkFlagRequired("to")

	return cmd
}

func runPromote(ctx context.Context, target, versionFlag, toRepo string, force, yes, dryRun bool, concurrency int) error {
	out := newWriter(flagJSON)

	client, err := dialClient(ctx)
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
		return fail(out, "promote", codeFor(err), "%s", err)
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

	// Dry-run before the conflict gate: a preview mutates nothing and is
	// most useful precisely when the destination version already exists.
	if dryRun {
		return runPromoteDryRun(ctx, cob.NewPromoter(client), coords, srcRepo, toRepo, exists, out)
	}

	if exists && !force {
		return fail(out, "promote", cob.ExitConflict, "version %s already exists in %s. Use --force to overwrite.", coords.Version, toRepo)
	}

	out.Header("Promoting %s/%s@%s: %s -> %s",
		coords.Namespace, coords.Package, coords.Version, srcRepo, toRepo)

	proceed, err := confirmAction(ctx, yes, fmt.Sprintf("Promote to %s?", toRepo))
	if err != nil {
		return fail(out, "promote", cob.ExitError, "%s", err)
	}
	if !proceed {
		out.Aborted("promote")
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

	// Promote doesn't know asset sizes up front, so the meter shows bytes/rate.
	meter := newProgressMeter(out, 0)
	promoter.Progress = meter.add
	defer meter.finish()

	cmdResult := &cob.CommandResult{
		Command:    "promote",
		Package:    fmt.Sprintf("%s/%s@%s", coords.Namespace, coords.Package, coords.Version),
		Repository: fmt.Sprintf("%s -> %s", srcRepo, toRepo),
		Status:     "ok",
	}

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
	concurrency = resolveConcurrency(concurrency, out)
	results, ok := runConcurrent(len(realNames), concurrency,
		func(i int) (*cob.AssetResult, error) {
			name := realNames[i]
			out.AssetStart(name, "", 0)
			ar, err := promoter.PromoteAsset(ctx, coords, srcRepo, toRepo, name, true)
			if err != nil {
				out.AssetFail(name, "", err)
				return ar, err
			}
			out.AssetOK(ar, "")
			return ar, nil
		})

	// Record every asset that ran — successes and failures — so a --json
	// consumer can see which one failed. Only successes count toward bytes.
	succeeded := 0
	for _, r := range results {
		if r == nil {
			continue // never scheduled: an earlier task failed first
		}
		cmdResult.Assets = append(cmdResult.Assets, *r)
		if r.Error == nil {
			cmdResult.TotalSize += r.Size
			succeeded++
		}
	}

	if !ok {
		cmdResult.DurationMs = time.Since(start).Milliseconds()
		cmdResult.Status = "error"
		cmdResult.Error = firstResultError(results)
		out.Error("%s\n  Promoted %d of %d assets to %s before failure. Version is in partial state.\n  Re-run with --force to delete and retry.",
			cmdResult.Error, succeeded, len(realNames), toRepo)
		out.CommandResult(cmdResult)
		return &ExitError{Code: cob.ExitError}
	}

	prov, err := carryForwardProvenance(ctx, client, coords, srcRepo, toRepo, realNames, results)
	if err != nil {
		return fail(out, "promote", cob.ExitError, "%s", err)
	}

	if err := finalizeProvenance(ctx, cob.NewPublisher(client), destCoords, prov, out, cmdResult, start,
		"Assets promoted but provenance/finalize failed. Version is in a partial state — re-run with --force to delete and retry."); err != nil {
		return err
	}

	out.Summary("Promoted %d assets in %s", len(cmdResult.Assets), output.FormatDuration(cmdResult.DurationMs))
	return out.CommandResult(cmdResult)
}

// runPromoteDryRun lists what a promote would copy and exits without
// mutating anything. The destination conflict is already checked, so
// destExists here means the real run would proceed under --force.
func runPromoteDryRun(ctx context.Context, promoter *cob.Promoter, coords *cob.PackageCoordinates,
	srcRepo, toRepo string, destExists bool, out *output.Writer) error {

	out.Header("Promote (dry run) %s/%s@%s: %s -> %s",
		coords.Namespace, coords.Package, coords.Version, srcRepo, toRepo)

	assetNames, err := promoter.ListAssetsToPromote(ctx, coords, srcRepo)
	if err != nil {
		return fail(out, "promote", codeFor(err), "%s", err)
	}

	result := &cob.CommandResult{
		Command:    "promote",
		Package:    fmt.Sprintf("%s/%s@%s", coords.Namespace, coords.Package, coords.Version),
		Repository: fmt.Sprintf("%s -> %s", srcRepo, toRepo),
		Status:     "ok",
	}
	for _, name := range assetNames {
		if name == cob.ProvenanceFile {
			continue // regenerated at promote time, not copied verbatim
		}
		out.Plain("  would promote %s", name)
		result.Assets = append(result.Assets, cob.AssetResult{Name: name, Method: "dry-run"})
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
	srcRepo, toRepo string, realNames []string, results []*cob.AssetResult) (*cob.Provenance, error) {

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
		CobVersion: buildVersion,
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
