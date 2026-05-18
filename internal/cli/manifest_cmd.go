package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/jmurray2011/cob/internal/manifest"
	"github.com/jmurray2011/cob/internal/output"
	"github.com/jmurray2011/cob/pkg/cob"
)

func newManifestCmd() *cobra.Command {
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
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runManifest(cmd.Context(), args[0], flagVersion)
		},
	}
	cmd.Flags().StringVar(&flagVersion, "version", "", "Package version (or use @version / COB_VERSION)")
	return cmd
}

func runManifest(ctx context.Context, target, versionFlag string) error {
	out := output.New(flagJSON)

	coords, err := manifest.ParseCoordinates(target)
	if err != nil {
		return fail(out, "manifest", cob.ExitError, "%s", err)
	}
	if coords.Namespace == "" || coords.Package == "" {
		return fail(out, "manifest", cob.ExitError, "full coordinates required (domain/repo/namespace/package[@version])")
	}
	if coords.Version == "" {
		v, verr := resolveVersion(versionFlag)
		if verr != nil {
			return fail(out, "manifest", cob.ExitError, "%s", verr)
		}
		coords.Version = v
	}

	client, err := cob.NewClient(ctx, cob.ClientOptions{Profile: flagProfile, Region: flagRegion})
	if err != nil {
		return fail(out, "manifest", cob.ExitError, "%s", err)
	}
	registry := cob.NewRegistry(client)
	if err := resolveLatestIfNeeded(ctx, coords, registry, out); err != nil {
		return fail(out, "manifest", cob.ExitNotFound, "%s", err)
	}

	yaml, err := manifestYAMLFor(ctx, client, coords)
	if err != nil {
		return fail(out, "manifest", cob.ExitNotFound, "%s", err)
	}
	fmt.Print(yaml)
	return nil
}

// generatedManifestFile is the manifest cob writes next to assets on a
// full-package pull.
const generatedManifestFile = "cob-manifest.yaml"

// manifestYAMLFor reconstructs (from cob-provenance.json) or infers (ca://
// self-references) the manifest YAML for a resolved version. Shared by the
// `manifest` command and `pull`.
func manifestYAMLFor(ctx context.Context, client *cob.Client, coords *cob.PackageCoordinates) (string, error) {
	prov, _ := cob.FetchProvenance(ctx, client.CodeArtifact, coords)

	var assets []cob.AssetSummary
	if prov == nil || len(prov.Assets) == 0 {
		a, err := cob.NewRegistry(client).ListAssets(ctx, coords)
		if err != nil {
			return "", err
		}
		assets = a
	}

	yaml, ok := renderManifest(coords, prov, assets)
	if !ok {
		return "", fmt.Errorf("no assets found for %s/%s@%s",
			coords.Namespace, coords.Package, coords.Version)
	}
	return yaml, nil
}

// caRef is a ca:// URI for an asset of the version itself — the only sound
// "source" for a non-cob version (its real origins aren't recorded).
func caRef(c *cob.PackageCoordinates, asset string) string {
	return fmt.Sprintf("ca://%s/%s/%s/%s@%s/%s",
		c.Domain, c.Repository, c.Namespace, c.Package, c.Version, asset)
}

// renderManifest builds the YAML. Provenance mode (prov has assets) is a
// faithful pinned reconstruction; otherwise assets are inferred as ca://
// self-references. Returns ok=false if there are no sources to emit. Pure
// (no I/O) so it is unit-tested directly.
func renderManifest(c *cob.PackageCoordinates, prov *cob.Provenance, assets []cob.AssetSummary) (string, bool) {
	type src struct{ key, uri string }
	var sources []src

	fromProv := prov != nil && len(prov.Assets) > 0
	if fromProv {
		for _, a := range prov.Assets {
			key := a.Key
			if key == "" {
				key = a.Asset
			}
			uri := a.Source
			if uri == "" {
				uri = caRef(c, a.Asset)
			}
			sources = append(sources, src{key, uri})
		}
	} else {
		for _, a := range assets {
			if a.Name == cob.ProvenanceFile {
				continue
			}
			sources = append(sources, src{a.Name, caRef(c, a.Name)})
		}
	}
	if len(sources) == 0 {
		return "", false
	}

	var b strings.Builder
	if fromProv {
		fmt.Fprintf(&b, "# Reconstructed by cob from %s\n", cob.ProvenanceFile)
		fmt.Fprintf(&b, "# %s/%s@%s in %s/%s\n", c.Namespace, c.Package, c.Version, c.Domain, c.Repository)
		for _, e := range prov.Chain {
			if e.Event == "publish" {
				fmt.Fprintf(&b, "# originally published %s by %s\n", e.Time, actorStr(e.Actor))
				break
			}
		}
		b.WriteString("# Pinned snapshot: source URIs are resolved (no ${VERSION}); promote.stages not recoverable.\n")
	} else {
		fmt.Fprintf(&b, "# Inferred by cob — %s/%s@%s has no %s (non-cob or pre-provenance).\n",
			c.Namespace, c.Package, c.Version, cob.ProvenanceFile)
		b.WriteString("# True source origins are unknown; each asset is sourced from the package itself.\n")
		b.WriteString("# Re-publishing this manifest reproduces the same bytes.\n")
	}

	fmt.Fprintf(&b, "domain: %s\n", c.Domain)
	fmt.Fprintf(&b, "repository: %s\n", c.Repository)
	fmt.Fprintf(&b, "namespace: %s\n", c.Namespace)
	fmt.Fprintf(&b, "package: %s\n", c.Package)
	b.WriteString("sources:\n")
	for _, s := range sources {
		fmt.Fprintf(&b, "  %s: %s\n", s.key, s.uri)
	}
	return b.String(), true
}
