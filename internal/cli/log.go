package cli

import (
	"context"

	"github.com/spf13/cobra"

	"github.com/jmurray2011/cob/internal/cob"
	"github.com/jmurray2011/cob/internal/manifest"

	"github.com/jmurray2011/cob/internal/cliutil"
)

func newLogCmd(cfg *cliutil.Config) *cobra.Command {
	var flagCheckRefs bool

	cmd := &cobra.Command{
		Use:   "log <coordinates>",
		Short: "Print a version's chain of evidence (publish/promote history)",
		Long: "Reads the recorded cob-provenance.json for a published version " +
			"and prints the chain of evidence — who published it, who " +
			"promoted it, when, and where — plus per-asset origins (where " +
			"each file came from). Read-only; no integrity check (use " +
			"`cob diff <coords>` for that). For a version not published " +
			"by cob, exits with an error rather than fabricating a chain.\n\n" +
			"With --check-references, each chain event's repository " +
			"reference is probed; entries whose referenced version has " +
			"been deleted are annotated inline with (deleted). Adds one " +
			"VersionStatus call per unique referenced repository — opt-in " +
			"because automation that runs log across many versions " +
			"shouldn't pay it by default.",
		Example: `  # log a specific version
  cob log acme/dev/tools/my-app@2.1.0

  # log the latest version
  cob log acme/dev/tools/my-app@latest

  # surface chain references that have been deleted
  cob log acme/staging/tools/my-app@2.1.0 --check-references

  # machine-readable: emits the full Provenance struct as JSON
  cob log acme/dev/tools/my-app@2.1.0 --json`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runLog(cmd.Context(), cfg, args, flagCheckRefs)
		},
	}
	cmd.Flags().BoolVar(&flagCheckRefs, "check-references", false, "Probe each chain event's referenced repository; annotate any that no longer hold this version (text mode only)")
	return cmd
}

func runLog(ctx context.Context, cfg *cliutil.Config, args []string, checkRefs bool) error {
	out := cliutil.NewWriter(cfg)
	defer out.Close()

	target, err := cliutil.ResolveTarget(cfg, out, args, "log")
	if err != nil {
		return cliutil.Fail(out, "log", cob.ExitError, "%s", err)
	}

	coords, err := manifest.ParseCoordinates(target)
	if err != nil {
		return cliutil.Fail(out, "log", cob.ExitError, "%s", err)
	}
	if coords.Namespace == "" || coords.Package == "" {
		return cliutil.Fail(out, "log", cob.ExitError,
			"full coordinates required (domain/repo/namespace/package[@version])")
	}
	if coords.Version == "" {
		// Mirror manifest/verify: version is required, no implicit
		// COB_VERSION fallback (a chain is per-version, not per-package).
		return cliutil.Fail(out, "log", cob.ExitError, "version is required (use @version or @latest)")
	}

	client, err := cliutil.DialClient(ctx, cfg, out)
	if err != nil {
		return cliutil.Fail(out, "log", cob.ExitError, "%s", err)
	}
	registry := cob.NewRegistry(client)
	if err := cliutil.ResolveLatestIfNeeded(ctx, coords, registry, out); err != nil {
		return cliutil.Fail(out, "log", cliutil.CodeFor(err), "%s", err)
	}

	prov, err := cob.FetchProvenance(ctx, client.CodeArtifact, coords)
	if err != nil {
		return cliutil.Fail(out, "log", cob.ExitError, "reading %s: %s", cob.ProvenanceFile, err)
	}
	if prov == nil {
		// Distinguishing "no provenance" from a hash mismatch matters —
		// verify treats "no provenance" as a check that couldn't run, log
		// as a hard precondition. The chain is the artifact log knows
		// about; nothing to render without it.
		return cliutil.Fail(out, "log", cob.ExitError,
			"no %s for %s/%s@%s — version was not published with cob",
			cob.ProvenanceFile, coords.Namespace, coords.Package, coords.Version)
	}

	// --check-references is a text-mode-only flag: probing is for inline
	// annotation, and JSON consumers can run their own VersionStatus loop
	// over prov.Chain without the wrapper drift a structured "references"
	// field would introduce.
	var refStatuses map[string]cliutil.ChainRefStatus
	if checkRefs && !cfg.JSON {
		refStatuses = cliutil.ProbeChainReferences(ctx, registry, coords, prov)
	}

	if out.JSON(prov) {
		return nil
	}

	out.Header("Chain of evidence for %s/%s@%s in %s/%s",
		coords.Namespace, coords.Package, coords.Version, coords.Domain, coords.Repository)
	cliutil.RenderChain(out, prov, refStatuses)
	cliutil.RenderOrigins(out, prov, "")

	// Count statuses for the trailing line — silent on a clean chain so
	// the common case stays a single-line summary.
	missing, unknown := 0, 0
	for _, s := range refStatuses {
		switch s {
		case cliutil.ChainRefMissing:
			missing++
		case cliutil.ChainRefUnknown:
			unknown++
		}
	}
	if missing > 0 || unknown > 0 {
		out.Warn("chain references: %d deleted, %d could not be probed", missing, unknown)
	}
	out.Summary("%d chain event(s), %d asset(s).", len(prov.Chain), len(prov.Assets))
	return nil
}
