package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/jmurray2011/cob/internal/cob"
	"github.com/jmurray2011/cob/internal/manifest"

	"github.com/jmurray2011/cob/internal/cliutil"
)

func newManifestCmd(cfg *cliutil.Config) *cobra.Command {
	var flagVersion string

	cmd := &cobra.Command{
		Use:   "manifest <coordinates>",
		Short: "Reconstruct or infer a manifest from a published version",
		Long: "Prints a manifest (YAML, to stdout) for an existing version.\n\n" +
			"If the version has a cob-provenance.json it is reconstructed " +
			"faithfully (original keys + the source URIs as resolved at " +
			"publish — a pinned snapshot, not the templated original; " +
			"promote.stages is not recoverable).\n\n" +
			"Otherwise it is inferred: each asset is sourced from the package " +
			"itself via ca://, since a non-cob version doesn't record where " +
			"its files came from. Re-publishing that manifest reproduces the " +
			"same bytes.",
		Example: `  # reconstruct a manifest from a published version
  cob manifest acme/dev/tools/my-app@2.1.0`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runManifest(cmd.Context(), cfg, args, flagVersion)
		},
	}
	cmd.Flags().StringVar(&flagVersion, "version", "", "Package version (or use @version / COB_VERSION)")
	return cmd
}

func runManifest(ctx context.Context, cfg *cliutil.Config, args []string, versionFlag string) error {
	out := cliutil.NewWriter(cfg)
	defer out.Close()

	target, err := cliutil.ResolveTarget(cfg, out, args, "manifest")
	if err != nil {
		return cliutil.Fail(out, "manifest", cob.ExitError, "%s", err)
	}

	coords, err := manifest.ParseCoordinates(target)
	if err != nil {
		return cliutil.Fail(out, "manifest", cob.ExitError, "%s", err)
	}
	if coords.Namespace == "" || coords.Package == "" {
		return cliutil.Fail(out, "manifest", cob.ExitError, "full coordinates required (domain/repo/namespace/package[@version])")
	}
	if coords.Version == "" {
		v, verr := cliutil.ResolveVersion(versionFlag)
		if verr != nil {
			return cliutil.Fail(out, "manifest", cob.ExitError, "%s", verr)
		}
		coords.Version = v
	}

	client, err := cliutil.DialClient(ctx, cfg, out)
	if err != nil {
		return cliutil.Fail(out, "manifest", cob.ExitError, "%s", err)
	}
	registry := cob.NewRegistry(client)
	if err := cliutil.ResolveLatestIfNeeded(ctx, coords, registry, out); err != nil {
		return cliutil.Fail(out, "manifest", cliutil.CodeFor(err), "%s", err)
	}

	yaml, err := cliutil.ManifestYAMLFor(ctx, client, coords)
	if err != nil {
		return cliutil.Fail(out, "manifest", cliutil.CodeFor(err), "%s", err)
	}
	fmt.Fprint(out.Stdout(), yaml)
	return nil
}
