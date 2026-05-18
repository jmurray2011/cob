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
		Use:   "ls <coordinates>",
		Short: "List packages, versions, or assets",
		Long:  "Drill into CodeArtifact: domain/repo (packages), .../ns/pkg (versions), ...@ver (assets).",
		Args:  cobra.MaximumNArgs(1),
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

func runLs(ctx context.Context, target string, allRepos bool) error {
	out := output.New(flagJSON)

	client, err := cob.NewClient(ctx, cob.ClientOptions{
		Profile: flagProfile,
		Region:  flagRegion,
	})
	if err != nil {
		return fail(out, "ls", cob.ExitError, "%s", err)
	}

	registry := cob.NewRegistry(client)

	// No argument -> list domains.
	if target == "" {
		return runLsDomains(ctx, registry, out)
	}

	coords, err := manifest.ParseCoordinates(target)
	if err != nil {
		return fail(out, "ls", cob.ExitError, "%s", err)
	}

	// domain only -> list repositories in that domain.
	if coords.Repository == "" && coords.Namespace == "" {
		return runLsRepos(ctx, registry, coords.Domain, out)
	}

	// Handle wildcard repo for promotion status.
	// Only valid with full coordinates (domain/repo/ns/pkg), not domain/repo.
	if (coords.Repository == "*" || allRepos) && coords.Namespace == "" {
		return fail(out, "ls", cob.ExitError, "--all-repos requires full coordinates (domain/*/namespace/package@version)")
	}
	if coords.Repository == "*" || (allRepos && coords.Namespace != "" && coords.Package != "") {
		if coords.Version == "" {
			return fail(out, "ls", cob.ExitError, "version is required for wildcard repo listing (use domain/*/ns/pkg@version or @latest)")
		}
		// Resolve @latest by trying each repo in the domain until one has the package.
		if coords.Version == "latest" {
			repos, err := registry.ListRepositories(ctx, coords.Domain)
			if err != nil || len(repos) == 0 {
				return fail(out, "ls", cob.ExitNotFound, "cannot resolve @latest: no repositories found in %s", coords.Domain)
			}
			var resolved bool
			for _, repo := range repos {
				resolveCoords := *coords
				resolveCoords.Repository = repo
				version, err := registry.ResolveLatest(ctx, &resolveCoords)
				if err == nil {
					coords.Version = version
					resolved = true
					out.Header("Resolved latest -> %s (from %s)", version, repo)
					break
				}
			}
			if !resolved {
				return fail(out, "ls", cob.ExitNotFound, "no published versions of %s/%s found in any repository in %s",
					coords.Namespace, coords.Package, coords.Domain)
			}
		}
		return runLsPromotionStatus(ctx, registry, coords, out)
	}

	// domain/repo only -> list packages
	if coords.Namespace == "" && coords.Package == "" {
		return runLsPackages(ctx, registry, coords, out)
	}

	// Resolve @latest for version-specific operations.
	if coords.Version == "latest" {
		if err := resolveLatestIfNeeded(ctx, coords, registry, out); err != nil {
			return fail(out, "ls", cob.ExitNotFound, "%s", err)
		}
	}

	// domain/repo/ns/pkg without version -> list versions
	if coords.Version == "" {
		return runLsVersions(ctx, registry, coords, out)
	}

	// domain/repo/ns/pkg@version -> list assets
	return runLsAssets(ctx, registry, coords, out)
}

func runLsPackages(ctx context.Context, registry *cob.Registry, coords *cob.PackageCoordinates, out *output.Writer) error {
	packages, err := registry.ListPackages(ctx, coords.Domain, coords.Repository)
	if err != nil {
		return fail(out, "ls", cob.ExitError, "%s", err)
	}

	if len(packages) == 0 {
		return fail(out, "ls", cob.ExitNotFound, "no packages found in %s/%s", coords.Domain, coords.Repository)
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
		return fail(out, "ls", cob.ExitNotFound, "no versions found for %s/%s in %s/%s",
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
		return fail(out, "ls", cob.ExitNotFound, "no assets found for %s/%s@%s",
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
		return fail(out, "ls", cob.ExitNotFound, "no domains found")
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
		return fail(out, "ls", cob.ExitNotFound, "no repositories found in %s", domain)
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
		exists, _ := registry.CheckVersionExists(ctx, checkCoords)
		status := cob.PromotionStatus{
			Repository: repo,
			Version:    "-",
			Status:     "-",
		}
		if exists {
			status.Version = coords.Version
			status.Status = "Published"
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
