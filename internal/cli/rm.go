package cli

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/jmurray2011/cob/internal/cob"
	"github.com/jmurray2011/cob/internal/concurrency"
	"github.com/jmurray2011/cob/internal/manifest"
)

func newRmCmd(cfg *Config) *cobra.Command {
	var (
		flagForce      bool
		flagEverywhere bool
		flagYes        bool
	)

	cmd := &cobra.Command{
		Use:   "rm <coordinates>",
		Short: "Delete a published version (immutable by default; --force to override)",
		Long: "Deletes a package version. cob treats published versions as " +
			"immutable: by default only Unfinished versions (failed or " +
			"abandoned publishes) are deletable. A Published version requires " +
			"--force; if it has been promoted to other repos in the same " +
			"domain their chain-of-evidence would reference a missing source, " +
			"so --force also refuses that case — use --force --everywhere to " +
			"override after deciding the broken chain is acceptable.\n\n" +
			"@latest is not accepted — only an explicit version, as " +
			"typo-resistance for the only destructive verb. Deletion is " +
			"irreversible; the provenance document is destroyed along with " +
			"the assets.",
		Example: `  # clean up an Unfinished publish
  cob rm acme/dev/tools/my-app@2.1.0-rc1

  # delete a real Published version with no downstream copies
  cob rm acme/dev/tools/my-app@2.1.0 --force

  # delete and accept that staging/prod chains will dangle
  cob rm acme/dev/tools/my-app@2.1.0 --force --everywhere`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRm(cmd.Context(), cfg, args[0], flagForce, flagEverywhere, flagYes)
		},
	}
	cmd.Flags().BoolVarP(&flagForce, "force", "f", false, "Delete a Published version (default refuses)")
	cmd.Flags().BoolVar(&flagEverywhere, "everywhere", false, "With --force: delete even if other repos have promoted from this version")
	cmd.Flags().BoolVarP(&flagYes, "yes", "y", false, "Skip confirmation")
	return cmd
}

func runRm(ctx context.Context, cfg *Config, target string, force, everywhere, yes bool) error {
	out := newWriter(cfg)
	defer out.Close()

	coords, err := manifest.ParseCoordinates(target)
	if err != nil {
		return fail(out, "rm", cob.ExitError, "%s", err)
	}
	if coords.Namespace == "" || coords.Package == "" {
		return fail(out, "rm", cob.ExitError, "full coordinates required (domain/repo/namespace/package@version)")
	}
	if coords.Version == "" {
		return fail(out, "rm", cob.ExitError, "version is required for rm")
	}
	// Refusing @latest is a deliberate typo-shield: every other command
	// resolves @latest, but the only destructive verb forces the operator
	// to name the bytes they intend to destroy.
	if coords.Version == "latest" {
		return fail(out, "rm", cob.ExitError, "rm refuses @latest — name the version explicitly")
	}
	if everywhere && !force {
		return fail(out, "rm", cob.ExitError, "--everywhere requires --force")
	}

	client, err := dialClient(ctx, cfg)
	if err != nil {
		return fail(out, "rm", cob.ExitError, "%s", err)
	}

	registry := cob.NewRegistry(client)
	status, exists, err := registry.VersionStatus(ctx, coords)
	if err != nil {
		return fail(out, "rm", cob.ExitError, "checking version: %s", err)
	}
	if !exists {
		return fail(out, "rm", cob.ExitNotFound, "no version %s of %s/%s in %s/%s",
			coords.Version, coords.Namespace, coords.Package, coords.Domain, coords.Repository)
	}

	// Tier gates.
	//   Tier 1 (default): Unfinished only — operational hygiene, no chain
	//   to protect.
	//   Tier 2 (--force): a Published version that no other repo has
	//   promoted from; lone deletion of a real release.
	//   Tier 3 (--force --everywhere): a Published version with
	//   downstream copies; the destination chains will dangle.
	isPublished := status != "Unfinished"
	if isPublished && !force {
		return fail(out, "rm", cob.ExitConflict,
			"version %s is %s (cob treats Published as immutable). Use --force to delete a real release.",
			coords.Version, status)
	}
	var downstream []string
	if isPublished && force {
		downstream, err = findDownstreamCopies(ctx, registry, coords)
		if err != nil {
			return fail(out, "rm", cob.ExitError, "checking downstream copies: %s", err)
		}
		if len(downstream) > 0 && !everywhere {
			return fail(out, "rm", cob.ExitConflict,
				"version %s is also published in %s/{%s} — their chain-of-evidence references this source. "+
					"Use --force --everywhere to delete anyway, or delete those copies first.",
				coords.Version, coords.Domain, strings.Join(downstream, ","))
		}
	}

	out.Header("Delete %s/%s@%s from %s/%s (%s)",
		coords.Namespace, coords.Package, coords.Version,
		coords.Domain, coords.Repository, status)

	prompt := buildRmPrompt(coords, status, downstream)
	proceed, err := confirmAction(ctx, yes, prompt)
	if err != nil {
		return fail(out, "rm", cob.ExitError, "%s", err)
	}
	if !proceed {
		out.Aborted("rm")
		return nil
	}

	publisher := cob.NewPublisher(client)
	if err := publisher.DeleteVersion(ctx, coords); err != nil {
		return fail(out, "rm", codeFor(err), "%s", err)
	}

	result := &cob.CommandResult{
		Command:    "rm",
		Package:    fmt.Sprintf("%s/%s@%s", coords.Namespace, coords.Package, coords.Version),
		Repository: fmt.Sprintf("%s/%s", coords.Domain, coords.Repository),
		Status:     "ok",
	}
	fillClientMeta(ctx, client, result)
	out.Summary("Deleted %s/%s@%s from %s/%s",
		coords.Namespace, coords.Package, coords.Version,
		coords.Domain, coords.Repository)
	return out.CommandResult(result)
}

// buildRmPrompt phrases the confirmation according to the tier the
// deletion lands in. The Unfinished line is short — operators clean those
// up often; the Published lines spell out what's at stake.
func buildRmPrompt(coords *cob.PackageCoordinates, status string, downstream []string) string {
	if status == "Unfinished" {
		return fmt.Sprintf("Delete Unfinished version %s? (irreversible)", coords.Version)
	}
	if len(downstream) > 0 {
		return fmt.Sprintf(
			"Delete Published version %s? %s/{%s} have promoted from it; their chain-of-evidence will reference a missing source. (irreversible)",
			coords.Version, coords.Domain, strings.Join(downstream, ","))
	}
	return fmt.Sprintf("Delete Published version %s? (irreversible)", coords.Version)
}

// findDownstreamCopies returns the repositories in coords.Domain that hold
// the same version of the same package, excluding coords.Repository
// itself. Probes are fanned out concurrently — matching the
// promotion-status pattern — so a 50-repo domain doesn't make rm wait
// minutes for its safety gate.
func findDownstreamCopies(ctx context.Context, registry *cob.Registry, coords *cob.PackageCoordinates) ([]string, error) {
	repos, err := registry.ListRepositories(ctx, coords.Domain)
	if err != nil {
		return nil, err
	}
	// Filter self before fanning out: skipping inside the fn would leave a
	// "" hole in the result slice that the post-loop already drops, but
	// pre-filtering keeps the per-item callback uncluttered.
	others := make([]string, 0, len(repos))
	for _, repo := range repos {
		if repo != coords.Repository {
			others = append(others, repo)
		}
	}
	found := concurrency.ForEach(ctx, others, promotionStatusConcurrency, func(ctx context.Context, _ int, repo string) string {
		probe := *coords
		probe.Repository = repo
		// A transient error per repo is intentionally ignored — a probe
		// failure is not a green light to delete; the worst case is a
		// missed downstream entry, which is bounded by --everywhere
		// being the next required flag.
		if _, exists, err := registry.VersionStatus(ctx, &probe); err == nil && exists {
			return repo
		}
		return ""
	})
	var out []string
	for _, r := range found {
		if r != "" {
			out = append(out, r)
		}
	}
	sort.Strings(out)
	return out, nil
}
