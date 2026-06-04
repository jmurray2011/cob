package pull

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/jmurray2011/cob/internal/cob"
	"github.com/jmurray2011/cob/internal/manifest"
	"github.com/jmurray2011/cob/internal/output"

	"github.com/jmurray2011/cob/internal/cliutil"
)

// generatedManifestFile is the manifest cob writes next to assets on a
// whole-package pull (kept in sync with the cli/manifest_cmd.go constant).
const generatedManifestFile = "cob-manifest.yaml"

func NewCmd(cfg *cliutil.Config) *cobra.Command {
	var (
		flagVersion     string
		flagConcurrency int
	)

	cmd := &cobra.Command{
		Use:   "pull <manifest|coords>[:asset[,asset...]] [destination]",
		Short: "Download assets from CodeArtifact",
		Long: "Downloads assets to a local destination. Shape mirrors cp/scp/rsync:\n\n" +
			"  cob pull <SOURCE> [DESTINATION]\n\n" +
			"SOURCE is either a manifest file or compact coordinates. Compact\n" +
			"coordinates may carry an inline asset filter — `:asset1,asset2,...`\n" +
			"after the coords — to download only the named files instead of the\n" +
			"whole version. Manifest mode pulls every asset the manifest\n" +
			"declares; inline filter on a manifest is rejected (the manifest\n" +
			"is the source of truth for which assets exist).\n\n" +
			"DESTINATION defaults to '.' (the current directory). For a multi-\n" +
			"asset pull, DESTINATION must be a directory (or end in '/' so cob\n" +
			"creates it). For a single-asset pull, DESTINATION may be a file\n" +
			"path or a directory.\n\n" +
			"Caveat: `:` and `,` are technically legal in CodeArtifact generic\n" +
			"asset names; the inline filter syntax can't express filters for\n" +
			"asset names that contain either. Those names work fine without a\n" +
			"filter (you'd pull the whole version) but can't be selectively\n" +
			"filtered through this CLI shape.",
		Example: `  # pull every asset of a version into a directory
  cob pull acme/dev/tools/my-app@2.1.0 ./assets/

  # pull one asset of the latest version to a specific file path
  cob pull acme/dev/tools/my-app@latest:app.tar.gz ./app.tar.gz

  # pull two named assets, resolving latest, to ~/downloads/
  cob pull acme/dev/tools/my-app@latest:app.tar.gz,sha256.txt ~/downloads/

  # combined with cob use: in a session where the current package is set
  cob pull @latest:app.tar.gz ~/staging/   # @version override + filter + dest
  cob pull vtdocs/vtdocs-installer@6.1.3 ~/d/  # ns/pkg shorthand + dest`,
		Args: cobra.MaximumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			// 0 args → empty target (Run falls back to current package).
			// 1 arg  → source only.
			// 2 args → source + destination.
			var target, destination string
			if len(args) > 0 {
				target = args[0]
			}
			if len(args) > 1 {
				destination = args[1]
			}
			return Run(cmd.Context(), cfg, target, flagVersion, destination, flagConcurrency)
		},
	}

	cmd.Flags().StringVar(&flagVersion, "version", "", "Specific version (required with manifest)")
	cmd.Flags().IntVar(&flagConcurrency, "concurrency", cliutil.DefaultConcurrency, "Max assets downloaded in parallel (1 = sequential; clamped to [1,32] to avoid CodeArtifact throttling — a warning prints if a passed value was changed)")

	return cmd
}

// splitSourceAndFilter divides the SOURCE positional into the
// coords/manifest part and the inline asset filter. A manifest path is
// returned whole — it may carry a drive-letter ':' on Windows
// (C:\dir\m.yaml) that is not a filter separator, and a manifest never
// takes an inline filter anyway (Run rejects that combination). Otherwise
// the first ':' splits: coordinate strings don't legally contain ':'
// (segments are alphanumeric +.-_, version is [a-zA-Z0-9.+-]+ per
// CodeArtifact), so the first ':' is unambiguously the filter separator.
// Returns (source, filter) where filter is "" when no inline filter was given.
func splitSourceAndFilter(s string) (source, filter string) {
	if cliutil.IsManifestPath(s) {
		return s, ""
	}
	i := strings.IndexByte(s, ':')
	if i < 0 {
		return s, ""
	}
	return s[:i], s[i+1:]
}

func Run(ctx context.Context, cfg *cliutil.Config, target, versionFlag, destination string, concurrency int) error {
	out := cliutil.NewWriter(cfg)
	defer out.Close()
	ctx, cancel := cliutil.Interruptable(ctx, cfg, out)
	defer cancel()

	// Peel the inline asset filter off the source first — the coords/
	// manifest part is what ResolveTarget and ParseCoordinates need.
	source, assetsFilter := splitSourceAndFilter(target)

	// Manifest mode owns its own asset list (the manifest is the source
	// of truth for what gets published); an inline filter would just
	// confuse the contract.
	if assetsFilter != "" && cliutil.IsManifestPath(source) {
		return cliutil.Fail(out, "pull", cob.ExitError,
			"inline asset filter (:%s) is not supported in manifest mode — the manifest determines which assets are pulled", assetsFilter)
	}

	// Always route the source through ResolveTarget so the
	// shorthand merges apply when a current package is set:
	//   - empty source → CurrentPackage fallback (--package /
	//     COB_PACKAGE_COORDS / .cob/current / ~/.config/cob/current)
	//   - "@<version>" alone → version override on current package
	//   - "ns/pkg" or "ns/pkg@version" → merge onto current's dom/repo
	//   - full 4-segment coords → passes through verbatim
	// Without this, `cob pull vtdocs/vtdocs-installer@6.1.3` (a
	// shorthand against the current package) would short-circuit past
	// the merge and ship the partial coords straight to AWS.
	{
		var rargs []string
		if source != "" {
			rargs = []string{source}
		}
		var err error
		source, err = cliutil.ResolveTarget(cfg, out, rargs, "pull")
		if err != nil {
			return cliutil.Fail(out, "pull", cob.ExitError, "%s", err)
		}
	}
	target = source

	client, err := cliutil.DialClient(ctx, cfg)
	if err != nil {
		return cliutil.Fail(out, "pull", cob.ExitError, "%s", err)
	}

	puller := cob.NewPuller(client)
	var coords *cob.PackageCoordinates

	if cliutil.IsManifestPath(target) {
		// Manifest mode.
		version, err := cliutil.ResolveVersion(versionFlag)
		if err != nil {
			return cliutil.Fail(out, "pull", cob.ExitError, "%s", err)
		}

		m, err := manifest.Load(target)
		if err != nil {
			return cliutil.Fail(out, "pull", cob.ExitError, "%s", err)
		}
		cliutil.WarnManifestOverrides(m, out)
		// Implicit pre-flight lint — see runPublish for rationale.
		// Pull only reads m.Domain/Repository/Namespace/Package to
		// resolve coords, but a malformed manifest (bad URI syntax,
		// reserved asset name, basename collision) should still cliutil.Fail
		// the same way every other manifest-based command does.
		if err := cliutil.ValidateManifest(m, version); err != nil {
			return cliutil.Fail(out, "pull", cob.ExitError, "%s", err)
		}

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
			return cliutil.Fail(out, "pull", cob.ExitError, "%s", err)
		}
		// ParseCoordinates accepts 1- and 2-segment forms (used by
		// `cob ls dom/repo` to list packages). Pull operates on a
		// specific package: short-circuit here with a clear error
		// rather than shipping empty namespace/package fields to
		// CodeArtifact and getting back a cryptic validation
		// exception. Mirrors the guard in log/manifest.
		if coords.Namespace == "" || coords.Package == "" {
			return cliutil.Fail(out, "pull", cob.ExitError,
				"full coordinates required (domain/repo/namespace/package[@version]); got %q", target)
		}
		if coords.Version == "" {
			return cliutil.Fail(out, "pull", cob.ExitError, "version is required for pull (use domain/repo/ns/pkg@version or @latest)")
		}
	}

	// Resolve @latest if needed.
	registry := cob.NewRegistry(client)
	if err := cliutil.ResolveLatestIfNeeded(ctx, coords, registry, out); err != nil {
		return cliutil.Fail(out, "pull", cliutil.CodeFor(err), "%s", err)
	}

	// Fetch all asset metadata in a single API call.
	allAssets, err := puller.FetchAssetInfo(ctx, coords)
	if err != nil {
		return cliutil.Fail(out, "pull", cliutil.CodeFor(err), "listing assets: %s", err)
	}

	// Narrow to the requested assets via the inline filter (if any).
	assets, unmatched, err := selectAssets(allAssets, assetsFilter)
	for _, name := range unmatched {
		out.Warn("asset %q not found in %s/%s@%s, skipping", name, coords.Namespace, coords.Package, coords.Version)
	}
	if err != nil {
		return cliutil.Fail(out, "pull", cob.ExitNotFound, "%s in %s/%s@%s", err, coords.Namespace, coords.Package, coords.Version)
	}

	outputPath := destination
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
			return cliutil.Fail(out, "pull", cob.ExitError, "creating output directory %s: %s", mkdir, err)
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
	cliutil.FillClientMeta(ctx, client, result)

	out.AssetsExpected(len(assets), totalAssetSize(assets))
	puller.Progress = out.AssetProgress

	concurrency = cliutil.ResolveConcurrency(concurrency, out)
	results, ok := cliutil.RunConcurrent(ctx, len(assets), concurrency, func(ctx context.Context, i int) (*cob.AssetResult, error) {
		info := assets[i]
		dest := outputPath
		if dirTarget {
			d, jerr := cliutil.SafeJoin(outputPath, info.Name)
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

	// Record every asset that ran — successes and failures — so a --json
	// consumer can see which one failed. Downloaded vs skipped is split
	// so an interrupted-pull summary can report accurately ("I had to
	// fetch 1, the other 11 were already there, 3 didn't finish") rather
	// than a single misleading "Pulled N" line.
	var downloaded, skipped, failed int
	for _, r := range results {
		if r == nil {
			continue // never scheduled: an earlier task failed first
		}
		result.Assets = append(result.Assets, *r)
		switch {
		case r.Error != nil:
			failed++
		case r.Method == "skipped":
			skipped++
			result.TotalSize += r.Size
		default:
			downloaded++
			result.TotalSize += r.Size
		}
	}
	if !ok {
		result.Status = "error"
		result.Error = cliutil.FirstResultError(results)
	}

	result.DurationMs = time.Since(start).Milliseconds()

	// Interrupted path: the user hit Ctrl-C inside the live TUI, which
	// cancels the runXxx ctx and lets the in-flight goroutines abort.
	// They appear in result.Assets with Error set; we report them as
	// "incomplete" rather than swallowed and exit 130 (POSIX SIGINT
	// convention) so a CI step can distinguish "user canceled" from
	// "operation failed".
	if out.Interrupted() {
		result.Status = "interrupted"
		incomplete := len(assets) - downloaded - skipped
		out.Summary("Interrupted: %d downloaded, %d already present, %d incomplete — re-run to resume (present files with matching SHA-256 will be skipped)",
			downloaded, skipped, incomplete)
		out.CommandResult(result)
		return &cliutil.ExitError{Code: cob.ExitInterrupted}
	}

	out.Summary("Pulled %d assets to %s", downloaded+skipped, outputPath)

	// A failed asset must surface as a non-zero exit (a CI step that does
	// `cob pull && deploy` otherwise deploys with missing/partial assets).
	// JSON is still emitted so pipelines can parse the partial result.
	if result.Status == "error" {
		out.CommandResult(result)
		return &cliutil.ExitError{Code: cob.ExitError}
	}

	// A whole-package pull into a directory also gets a recoverable
	// manifest (reconstructed from provenance, or inferred). Best-effort:
	// the assets are already down, so a manifest hiccup only warns.
	wholePackage := assetsFilter == ""
	if wholePackage && dirTarget {
		writePulledManifest(ctx, client, coords, assets, outputPath, out)
	}
	return out.CommandResult(result)
}

// totalAssetSize sums known asset sizes, for the transfer progress meter.
func totalAssetSize(assets []cob.AssetInfo) int64 {
	var n int64
	for _, a := range assets {
		n += a.Size
	}
	return n
}

// selectAssets narrows the full asset list to what the caller asked for: a
// comma-separated inline filter, or — given none — everything. unmatched
// holds any filter names that matched no asset (the caller warns on each).
// A non-nil error means nothing matched at all; it carries no coordinates,
// so the caller frames it. Blank elements (from a trailing or doubled comma)
// are dropped rather than reported as misses.
func selectAssets(all []cob.AssetInfo, assetsFilter string) (selected []cob.AssetInfo, unmatched []string, err error) {
	wanted := make(map[string]bool)
	for _, name := range strings.Split(assetsFilter, ",") {
		if name = strings.TrimSpace(name); name != "" {
			wanted[name] = true
		}
	}
	if len(wanted) == 0 {
		// No (or only-blank) filter → everything.
		return all, nil, nil
	}
	matched := make(map[string]bool)
	for _, a := range all {
		if wanted[a.Name] {
			selected = append(selected, a)
			matched[a.Name] = true
		}
	}
	for name := range wanted {
		if !matched[name] {
			unmatched = append(unmatched, name)
		}
	}
	if len(selected) == 0 {
		return nil, unmatched, fmt.Errorf("none of the requested assets found")
	}
	return selected, unmatched, nil
}

func writePulledManifest(ctx context.Context, client *cob.Client, coords *cob.PackageCoordinates, assets []cob.AssetInfo, dir string, out *output.Writer) {
	for _, a := range assets {
		if a.Name == generatedManifestFile {
			out.Warn("an asset is named %s; skipping generated manifest", generatedManifestFile)
			return
		}
	}
	y, err := cliutil.ManifestYAMLFor(ctx, client, coords)
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

func isDir(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return info.IsDir()
}
