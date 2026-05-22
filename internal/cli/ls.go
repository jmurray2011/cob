package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/jmurray2011/cob/internal/manifest"
	"github.com/jmurray2011/cob/internal/output"
	"github.com/jmurray2011/cob/pkg/cob"
)

func newLsCmd() *cobra.Command {
	var flagAllRepos bool

	cmd := &cobra.Command{
		Use:   "ls [coordinates]",
		Short: "List packages, versions, or assets",
		Long:  "Drill into CodeArtifact: domain/repo (packages), .../ns/pkg (versions), ...@ver (assets).",
		Example: `  cob ls                        # domains
  cob ls acme/dev               # packages in a repo
  cob ls acme/dev/tools/my-app  # versions of a package
  cob ls acme/*/tools/my-app@2.1.0   # promotion status across repos`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			target := ""
			if len(args) > 0 {
				target = args[0]
			}
			return runLs(cmd.Context(), target, flagAllRepos)
		},
	}

	cmd.Flags().BoolVar(&flagAllRepos, "all-repos", false, "Shorthand for wildcard repo")

	return cmd
}

// lsKind is the kind of listing a parsed ls target requests.
type lsKind int

const (
	lsKindDomains lsKind = iota
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
//	domain                          -> repos      (before wildcard rules, so
//	                                                `ls acme --all-repos`
//	                                                still lists repos)
//	domain/* | --all-repos (full)   -> promotion status
//	domain/repo                     -> packages
//	domain/repo/ns/pkg              -> versions
//	domain/repo/ns/pkg@version      -> assets
//
// A non-empty second return value is a user-facing validation message for an
// invalid flag/coordinate combination; the caller turns it into an error.
func classifyLs(coords *cob.PackageCoordinates, target string, allRepos bool) (lsKind, string) {
	if target == "" {
		return lsKindDomains, ""
	}
	// domain only (single segment). Checked before the wildcard rules so
	// `cob ls acme --all-repos` still lists repositories.
	if coords.Repository == "" && coords.Namespace == "" {
		return lsKindRepos, ""
	}

	wildcard := coords.Repository == "*" || allRepos
	full := coords.Namespace != "" && coords.Package != ""

	if wildcard && coords.Namespace == "" {
		return 0, "--all-repos requires full coordinates (domain/*/namespace/package@version)"
	}
	if coords.Repository == "*" || (allRepos && full) {
		if coords.Version == "" {
			return 0, "version is required for wildcard repo listing (use domain/*/ns/pkg@version or @latest)"
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

func runLs(ctx context.Context, target string, allRepos bool) error {
	out := newWriter(flagJSON)

	client, err := dialClient(ctx)
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

	kind, invalid := classifyLs(coords, target, allRepos)
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

// resolvePromotionLatest resolves @latest for a wildcard / --all-repos
// promotion listing by probing each repository in the domain until one has
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
// fail.
func failEmptyList(out *output.Writer, emptyList any, format string, args ...any) error {
	if flagJSON {
		out.JSON(emptyList)
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

	var statuses []cob.PromotionStatus
	for _, repo := range repos {
		checkCoords := &cob.PackageCoordinates{
			Domain:     coords.Domain,
			Repository: repo,
			Namespace:  coords.Namespace,
			Package:    coords.Package,
			Version:    coords.Version,
		}
		status := cob.PromotionStatus{Repository: repo, Version: "-", Status: "-"}
		// Render the version's real status. CheckVersionExists (used by
		// publish/--force) now reports any status as "exists"; hardcoding
		// "Published" here would mislabel an Unfinished/Archived version. A
		// transient failure (throttle, network, access-denied) is distinct
		// from "absent" — surface it as "?" so an operator never reads a
		// check that never completed as "not promoted to this repo".
		st, found, err := registry.VersionStatus(ctx, checkCoords)
		switch {
		case err != nil:
			status.Version, status.Status = "?", "?"
		case found:
			status.Version = coords.Version
			if st == "" {
				st = "-"
			}
			status.Status = st
		}
		statuses = append(statuses, status)
	}

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
