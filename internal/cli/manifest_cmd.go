package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/jmurray2011/cob/internal/manifest"
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
		Example: `  # reconstruct a manifest from a published version
  cob manifest acme/dev/tools/my-app@2.1.0`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runManifest(cmd.Context(), args[0], flagVersion)
		},
	}
	cmd.Flags().StringVar(&flagVersion, "version", "", "Package version (or use @version / COB_VERSION)")
	return cmd
}

func runManifest(ctx context.Context, target, versionFlag string) error {
	out := newWriter(flagJSON)

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

	client, err := dialClient(ctx)
	if err != nil {
		return fail(out, "manifest", cob.ExitError, "%s", err)
	}
	registry := cob.NewRegistry(client)
	if err := resolveLatestIfNeeded(ctx, coords, registry, out); err != nil {
		return fail(out, "manifest", codeFor(err), "%s", err)
	}

	yaml, err := manifestYAMLFor(ctx, client, coords)
	if err != nil {
		return fail(out, "manifest", codeFor(err), "%s", err)
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
	prov, err := cob.FetchProvenance(ctx, client.CodeArtifact, coords)
	if err != nil {
		return "", fmt.Errorf("reading %s: %w", cob.ProvenanceFile, err)
	}

	var assets []cob.AssetSummary
	if prov == nil || len(prov.Assets) == 0 {
		a, err := cob.NewRegistry(client).ListAssets(ctx, coords)
		if err != nil {
			return "", err
		}
		assets = a
	}

	return renderManifest(coords, prov, assets)
}

// caRef is a ca:// URI for an asset of the version itself — the only sound
// "source" for a non-cob version (its real origins aren't recorded).
func caRef(c *cob.PackageCoordinates, asset string) string {
	return fmt.Sprintf("ca://%s/%s/%s/%s@%s/%s",
		c.Domain, c.Repository, c.Namespace, c.Package, c.Version, asset)
}

// renderManifest builds the manifest YAML. Provenance mode (prov has assets)
// is a faithful pinned reconstruction; otherwise assets are inferred as ca://
// self-references. The document is assembled as a yaml.Node and encoded, so
// every key and URI is correctly quoted — a value containing #, *, a leading
// [, a newline, etc. round-trips instead of corrupting the output or
// injecting a line. Errors if there are no sources. Pure (no I/O), so it is
// unit-tested directly.
func renderManifest(c *cob.PackageCoordinates, prov *cob.Provenance, assets []cob.AssetSummary) (string, error) {
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
		return "", fmt.Errorf("no assets found for %s/%s@%s: %w", c.Namespace, c.Package, c.Version, cob.ErrNotFound)
	}

	sourcesNode := &yaml.Node{Kind: yaml.MappingNode}
	for _, s := range sources {
		sourcesNode.Content = append(sourcesNode.Content, yamlString(s.key), yamlString(s.uri))
	}
	root := &yaml.Node{Kind: yaml.MappingNode, HeadComment: manifestHeader(c, prov, fromProv)}
	for _, kv := range []struct {
		k string
		v *yaml.Node
	}{
		{"domain", yamlString(c.Domain)},
		{"repository", yamlString(c.Repository)},
		{"namespace", yamlString(c.Namespace)},
		{"package", yamlString(c.Package)},
		{"sources", sourcesNode},
	} {
		root.Content = append(root.Content, yamlString(kv.k), kv.v)
	}

	var b strings.Builder
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(root); err != nil {
		return "", err
	}
	if err := enc.Close(); err != nil {
		return "", err
	}
	return b.String(), nil
}

// yamlString builds a scalar node, letting the encoder pick a quoting style
// that keeps the value safe regardless of which characters it contains.
func yamlString(s string) *yaml.Node {
	n := &yaml.Node{}
	n.SetString(s)
	return n
}

// manifestHeader is the comment block for a reconstructed/inferred manifest,
// one line per entry (the encoder adds the "# " prefixes).
func manifestHeader(c *cob.PackageCoordinates, prov *cob.Provenance, fromProv bool) string {
	var lines []string
	if fromProv {
		lines = append(lines,
			fmt.Sprintf("Reconstructed by cob from %s", cob.ProvenanceFile),
			fmt.Sprintf("%s/%s@%s in %s/%s", c.Namespace, c.Package, c.Version, c.Domain, c.Repository))
		for _, e := range prov.Chain {
			if e.Event == "publish" {
				lines = append(lines, fmt.Sprintf("originally published %s by %s", e.Time, actorStr(e.Actor)))
				break
			}
		}
		lines = append(lines, "Pinned snapshot: source URIs are resolved (no ${VERSION}); promote.stages not recoverable.")
	} else {
		lines = append(lines,
			fmt.Sprintf("Inferred by cob — %s/%s@%s has no %s (non-cob or pre-provenance).",
				c.Namespace, c.Package, c.Version, cob.ProvenanceFile),
			"True source origins are unknown; each asset is sourced from the package itself.",
			"Re-publishing this manifest reproduces the same bytes.")
	}
	return strings.Join(lines, "\n")
}
