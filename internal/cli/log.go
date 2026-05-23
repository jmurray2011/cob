package cli

import (
	"context"

	"github.com/spf13/cobra"

	"github.com/jmurray2011/cob/internal/cob"
	"github.com/jmurray2011/cob/internal/manifest"
)

func newLogCmd(cfg *Config) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "log <coordinates>",
		Short: "Print a version's chain of evidence (publish/promote history)",
		Long: "Reads the recorded cob-provenance.json for a published version " +
			"and prints the chain of evidence — who published it, who " +
			"promoted it, when, and where — plus per-asset origins (where " +
			"each file came from). Read-only; no integrity check (use " +
			"`cob verify` for that). For a version not published by cob, " +
			"exits with an error rather than fabricating a chain.",
		Example: `  # log a specific version
  cob log acme/dev/tools/my-app@2.1.0

  # log the latest version
  cob log acme/dev/tools/my-app@latest

  # machine-readable: emits the full Provenance struct as JSON
  cob log acme/dev/tools/my-app@2.1.0 --json`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runLog(cmd.Context(), cfg, args[0])
		},
	}
	return cmd
}

func runLog(ctx context.Context, cfg *Config, target string) error {
	out := newWriter(cfg)

	coords, err := manifest.ParseCoordinates(target)
	if err != nil {
		return fail(out, "log", cob.ExitError, "%s", err)
	}
	if coords.Namespace == "" || coords.Package == "" {
		return fail(out, "log", cob.ExitError,
			"full coordinates required (domain/repo/namespace/package[@version])")
	}
	if coords.Version == "" {
		// Mirror manifest/verify: version is required, no implicit
		// COB_VERSION fallback (a chain is per-version, not per-package).
		return fail(out, "log", cob.ExitError, "version is required (use @version or @latest)")
	}

	client, err := dialClient(ctx, cfg)
	if err != nil {
		return fail(out, "log", cob.ExitError, "%s", err)
	}
	registry := cob.NewRegistry(client)
	if err := resolveLatestIfNeeded(ctx, coords, registry, out); err != nil {
		return fail(out, "log", codeFor(err), "%s", err)
	}

	prov, err := cob.FetchProvenance(ctx, client.CodeArtifact, coords)
	if err != nil {
		return fail(out, "log", cob.ExitError, "reading %s: %s", cob.ProvenanceFile, err)
	}
	if prov == nil {
		// Distinguishing "no provenance" from a hash mismatch matters —
		// verify treats "no provenance" as a check that couldn't run, log
		// as a hard precondition. The chain is the artifact log knows
		// about; nothing to render without it.
		return fail(out, "log", cob.ExitError,
			"no %s for %s/%s@%s — version was not published with cob",
			cob.ProvenanceFile, coords.Namespace, coords.Package, coords.Version)
	}

	if out.JSON(prov) {
		return nil
	}

	out.Header("Chain of evidence for %s/%s@%s in %s/%s",
		coords.Namespace, coords.Package, coords.Version, coords.Domain, coords.Repository)
	renderChain(out, prov)
	renderOrigins(out, prov, "")
	// Trailing summary — gives the operator a single line to grep / paste.
	out.Summary("%d chain event(s), %d asset(s).", len(prov.Chain), len(prov.Assets))
	return nil
}
