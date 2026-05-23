package cli

import (
	"context"
	"fmt"
	"sync"

	"github.com/spf13/cobra"

	"github.com/jmurray2011/cob/internal/cob"
	"github.com/jmurray2011/cob/internal/manifest"
	"github.com/jmurray2011/cob/internal/output"
)

// promotionStatusConcurrency bounds the parallel per-repo VersionStatus calls
// in `ls dom/*/ns/pkg@v` (mirrors registry.versionMetaConcurrency).
const promotionStatusConcurrency = 8

func newLsCmd(cfg *Config) *cobra.Command {
	var (
		flagRecursive bool
		flagDepth     string
	)

	cmd := &cobra.Command{
		Use:   "ls [coordinates]",
		Short: "List packages, versions, or assets",
		Long: "Drill into CodeArtifact: domain/repo (packages), .../ns/pkg " +
			"(versions), ...@ver (assets). A repo wildcard (`*`) listing a " +
			"specific version reports promotion status across repos. With -R, " +
			"walks the hierarchy under the target and prints one " +
			"fully-qualified coordinate per line (use --depth to control how " +
			"deep; see `cob tree` for a tree-shaped view of the same walk).",
		Example: `  cob ls                              # domains
  cob ls acme/dev                     # packages in a repo
  cob ls acme/dev/tools/my-app        # versions of a package
  cob ls 'acme/*/tools/my-app@2.1.0'  # promotion status across repos (quote in zsh)
  cob ls -R                           # every package, fully-qualified
  cob ls -R acme/dev --depth versions # every version under a repo`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			target := ""
			if len(args) > 0 {
				target = args[0]
			}
			if flagRecursive {
				return runLsRecursive(cmd.Context(), cfg, cmd, target, flagDepth)
			}
			return runLs(cmd.Context(), cfg, target)
		},
	}

	cmd.Flags().BoolVarP(&flagRecursive, "recursive", "R", false, "Flat recursive listing (fully-qualified coordinates, one per line)")
	cmd.Flags().StringVar(&flagDepth, "depth", "packages", "With -R: walk depth (domains|repos|packages|versions|assets)")

	return cmd
}

// runLsRecursive walks the hierarchy under target and emits each leaf as a
// fully-qualified coordinate. JSON mode emits the same leaves as an array
// (not the tree — that's what `cob tree --json` is for); text mode emits
// one path per line, suitable for piping.
func runLsRecursive(ctx context.Context, cfg *Config, cmd *cobra.Command, target, depthFlag string) error {
	out := newWriter(cfg)

	depth, err := parseTreeDepth(depthFlag)
	if err != nil {
		return fail(out, "ls", cob.ExitError, "%s", err)
	}

	var start *cob.PackageCoordinates
	if target != "" {
		start, err = manifest.ParseCoordinates(target)
		if err != nil {
			return fail(out, "ls", cob.ExitError, "%s", err)
		}
	}
	// Mirror tree's "descend one level into a targeted node" default.
	if start != nil && !cmd.Flags().Changed("depth") {
		startKind := startDepthOf(start)
		if depth <= startKind && startKind < DepthAssets {
			depth = startKind + 1
		}
	}
	if start != nil && cmd.Flags().Changed("depth") {
		startKind := startDepthOf(start)
		if depth < startKind {
			return fail(out, "ls", cob.ExitError,
				"--depth %s is shallower than the target (level %s); pick a deeper depth or drop the target",
				depthName(depth), depthName(startKind))
		}
	}

	client, err := dialClient(ctx, cfg)
	if err != nil {
		return fail(out, "ls", cob.ExitError, "%s", err)
	}
	registry := cob.NewRegistry(client)

	root := walkHierarchy(ctx, registry, start, depth)
	leaves := flattenLeaves(root)
	// An empty walk under a valid target is a not-found, not a bug: emit
	// the documented empty array in JSON mode so consumers see [] (not the
	// command-result object) and surface ExitNotFound for the shell.
	if len(leaves) == 0 {
		return failEmptyList(out, []string{}, "no entries found under %q at depth %s", target, depthName(depth))
	}
	if out.JSON(leaves) {
		return nil
	}
	for _, p := range leaves {
		out.Plain("%s", p)
	}
	if errs := countErrors(root); errs > 0 {
		out.Warn("%d branch(es) could not be listed", errs)
	}
	return nil
}

// lsKind is the kind of listing a parsed ls target requests.
type lsKind int

const (
	lsKindInvalid lsKind = iota // zero value: returned only with a message
	lsKindDomains
	lsKindRepos
	lsKindPackages
	lsKindVersions
	lsKindAssets
	lsKindPromotion
)

// classifyLs maps a parsed target to the listing it requests. It does no
// I/O, so it is unit-testable in isolation. The precedence below is the one
// ls has always used and must not change:
//
//	(no target)                     -> domains
//	domain                          -> repos
//	domain/*/ns/pkg@version         -> promotion status across repos
//	domain/repo                     -> packages
//	domain/repo/ns/pkg              -> versions
//	domain/repo/ns/pkg@version      -> assets
//
// A non-empty second return value is a user-facing validation message for an
// invalid coordinate combination; the caller turns it into an error.
func classifyLs(coords *cob.PackageCoordinates, target string) (lsKind, string) {
	if target == "" {
		return lsKindDomains, ""
	}
	// Domain only (single segment) → repos.
	if coords.Repository == "" && coords.Namespace == "" {
		return lsKindRepos, ""
	}

	if coords.Repository == "*" {
		if coords.Namespace == "" || coords.Package == "" {
			return lsKindInvalid, "wildcard repo (`*`) requires full coordinates (domain/*/namespace/package@version)"
		}
		if coords.Version == "" {
			return lsKindInvalid, "version is required for wildcard repo listing (use domain/*/ns/pkg@version or @latest)"
		}
		return lsKindPromotion, ""
	}
	if coords.Namespace == "" && coords.Package == "" {
		return lsKindPackages, ""
	}
	if coords.Version == "" {
		return lsKindVersions, ""
	}
	return lsKindAssets, ""
}

func runLs(ctx context.Context, cfg *Config, target string) error {
	out := newWriter(cfg)

	client, err := dialClient(ctx, cfg)
	if err != nil {
		return fail(out, "ls", cob.ExitError, "%s", err)
	}
	registry := cob.NewRegistry(client)

	var coords *cob.PackageCoordinates
	if target != "" {
		coords, err = manifest.ParseCoordinates(target)
		if err != nil {
			return fail(out, "ls", cob.ExitError, "%s", err)
		}
	}

	kind, invalid := classifyLs(coords, target)
	if invalid != "" {
		return fail(out, "ls", cob.ExitError, "%s", invalid)
	}

	switch kind {
	case lsKindDomains:
		return runLsDomains(ctx, registry, out)
	case lsKindRepos:
		return runLsRepos(ctx, registry, coords.Domain, out)
	case lsKindPackages:
		return runLsPackages(ctx, registry, coords, out)
	case lsKindPromotion:
		if err := resolvePromotionLatest(ctx, registry, coords, out); err != nil {
			return err
		}
		return runLsPromotionStatus(ctx, registry, coords, out)
	case lsKindVersions:
		return runLsVersions(ctx, registry, coords, out)
	default: // lsKindAssets
		if coords.Version == "latest" {
			if err := resolveLatestIfNeeded(ctx, coords, registry, out); err != nil {
				return fail(out, "ls", cob.ExitNotFound, "%s", err)
			}
		}
		return runLsAssets(ctx, registry, coords, out)
	}
}

// resolvePromotionLatest resolves @latest for a wildcard-repo promotion
// listing by probing each repository in the domain until one has
// the package, then pinning coords.Version. A non-latest version is left
// untouched.
func resolvePromotionLatest(ctx context.Context, registry *cob.Registry, coords *cob.PackageCoordinates, out *output.Writer) error {
	if coords.Version != "latest" {
		return nil
	}
	repos, err := registry.ListRepositories(ctx, coords.Domain)
	if err != nil || len(repos) == 0 {
		return fail(out, "ls", cob.ExitNotFound, "cannot resolve @latest: no repositories found in %s", coords.Domain)
	}
	for _, repo := range repos {
		probe := *coords
		probe.Repository = repo
		if version, err := registry.ResolveLatest(ctx, &probe); err == nil {
			coords.Version = version
			out.Header("Resolved latest -> %s (from %s)", version, repo)
			return nil
		}
	}
	return fail(out, "ls", cob.ExitNotFound, "no published versions of %s/%s found in any repository in %s",
		coords.Namespace, coords.Package, coords.Domain)
}

// failEmptyList reports a not-found for a list command. In --json mode it
// emits an empty array of the documented element type — so a consumer always
// gets a parseable array, never an error object — and writes the message to
// stderr; the exit code (2) already signals not-found. Non-JSON behaves like
// fail. Whether output is JSON is read off the Writer itself (out.JSON
// returns true if it emitted), so this function needs no Config dependency.
func failEmptyList(out *output.Writer, emptyList any, format string, args ...any) error {
	if out.JSON(emptyList) {
		out.Error(format, args...)
		return &ExitError{Code: cob.ExitNotFound}
	}
	return fail(out, "ls", cob.ExitNotFound, format, args...)
}

func runLsPackages(ctx context.Context, registry *cob.Registry, coords *cob.PackageCoordinates, out *output.Writer) error {
	packages, err := registry.ListPackages(ctx, coords.Domain, coords.Repository)
	if err != nil {
		return fail(out, "ls", cob.ExitError, "%s", err)
	}

	if len(packages) == 0 {
		return failEmptyList(out, []cob.PackageSummary{}, "no packages found in %s/%s", coords.Domain, coords.Repository)
	}

	if out.JSON(packages) {
		return nil
	}

	headers := []string{"NAMESPACE", "PACKAGE", "LATEST", "VERSIONS"}
	var rows [][]string
	for _, p := range packages {
		rows = append(rows, []string{p.Namespace, p.Package, p.LatestVersion, fmt.Sprintf("%d", p.VersionCount)})
	}
	out.Table(headers, rows)
	return nil
}

func runLsVersions(ctx context.Context, registry *cob.Registry, coords *cob.PackageCoordinates, out *output.Writer) error {
	versions, err := registry.ListVersions(ctx, coords)
	if err != nil {
		return fail(out, "ls", cob.ExitError, "%s", err)
	}

	if len(versions) == 0 {
		return failEmptyList(out, []cob.VersionSummary{}, "no versions found for %s/%s in %s/%s",
			coords.Namespace, coords.Package, coords.Domain, coords.Repository)
	}

	if out.JSON(versions) {
		return nil
	}

	headers := []string{"VERSION", "ASSETS", "PUBLISHED"}
	var rows [][]string
	for _, v := range versions {
		pubDate := ""
		if !v.Published.IsZero() {
			pubDate = v.Published.Format("2006-01-02")
		}
		rows = append(rows, []string{v.Version, fmt.Sprintf("%d", v.Assets), pubDate})
	}
	out.Table(headers, rows)
	return nil
}

func runLsAssets(ctx context.Context, registry *cob.Registry, coords *cob.PackageCoordinates, out *output.Writer) error {
	assets, err := registry.ListAssets(ctx, coords)
	if err != nil {
		return fail(out, "ls", cob.ExitError, "%s", err)
	}

	if len(assets) == 0 {
		return failEmptyList(out, []cob.AssetSummary{}, "no assets found for %s/%s@%s",
			coords.Namespace, coords.Package, coords.Version)
	}

	if out.JSON(assets) {
		return nil
	}

	headers := []string{"ASSET", "SIZE", "SHA256"}
	var rows [][]string
	for _, a := range assets {
		hash := a.SHA256
		if len(hash) > 8 {
			hash = hash[:8] + "..."
		}
		rows = append(rows, []string{a.Name, output.FormatSize(a.Size), hash})
	}
	out.Table(headers, rows)
	return nil
}

func runLsDomains(ctx context.Context, registry *cob.Registry, out *output.Writer) error {
	domains, err := registry.ListDomains(ctx)
	if err != nil {
		return fail(out, "ls", cob.ExitError, "%s", err)
	}

	if len(domains) == 0 {
		return failEmptyList(out, []cob.DomainSummary{}, "no domains found")
	}

	if out.JSON(domains) {
		return nil
	}

	headers := []string{"DOMAIN", "OWNER", "STATUS"}
	var rows [][]string
	for _, d := range domains {
		rows = append(rows, []string{d.Name, d.Owner, d.Status})
	}
	out.Table(headers, rows)
	return nil
}

func runLsRepos(ctx context.Context, registry *cob.Registry, domain string, out *output.Writer) error {
	repos, err := registry.ListRepositories(ctx, domain)
	if err != nil {
		return fail(out, "ls", cob.ExitError, "%s", err)
	}

	if len(repos) == 0 {
		return failEmptyList(out, []string{}, "no repositories found in %s", domain)
	}

	if out.JSON(repos) {
		return nil
	}

	headers := []string{"REPOSITORY"}
	var rows [][]string
	for _, r := range repos {
		rows = append(rows, []string{r})
	}
	out.Table(headers, rows)
	return nil
}

func runLsPromotionStatus(ctx context.Context, registry *cob.Registry, coords *cob.PackageCoordinates, out *output.Writer) error {
	repos, err := registry.ListRepositories(ctx, coords.Domain)
	if err != nil {
		return fail(out, "ls", cob.ExitError, "%s", err)
	}

	// Probe each repo's VersionStatus in parallel — a domain with many
	// repos was needlessly slow when this ran one-at-a-time. Bounded so a
	// busy ls doesn't hammer CodeArtifact into throttling.
	//
	// Render the version's real status. CheckVersionExists (used by
	// publish/--force) reports any status as "exists"; hardcoding
	// "Published" here would mislabel an Unfinished/Archived version. A
	// transient failure (throttle, network, access-denied) is distinct
	// from "absent" — surface it as "?" so an operator never reads a
	// check that never completed as "not promoted to this repo".
	statuses := make([]cob.PromotionStatus, len(repos))
	sem := make(chan struct{}, promotionStatusConcurrency)
	var wg sync.WaitGroup
	for i, repo := range repos {
		if ctx.Err() != nil {
			break
		}
		sem <- struct{}{}
		wg.Add(1)
		go func(i int, repo string) {
			defer wg.Done()
			defer func() { <-sem }()
			checkCoords := &cob.PackageCoordinates{
				Domain:     coords.Domain,
				Repository: repo,
				Namespace:  coords.Namespace,
				Package:    coords.Package,
				Version:    coords.Version,
			}
			s := cob.PromotionStatus{Repository: repo, Version: "-", Status: "-"}
			st, found, err := registry.VersionStatus(ctx, checkCoords)
			switch {
			case err != nil:
				s.Version, s.Status = "?", "?"
			case found:
				s.Version = coords.Version
				if st == "" {
					st = "-"
				}
				s.Status = st
			}
			statuses[i] = s
		}(i, repo)
	}
	wg.Wait()

	if out.JSON(statuses) {
		return nil
	}

	headers := []string{"REPOSITORY", "VERSION", "STATUS"}
	var rows [][]string
	for _, s := range statuses {
		rows = append(rows, []string{s.Repository, s.Version, s.Status})
	}
	out.Table(headers, rows)
	return nil
}
