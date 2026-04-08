package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jmurray2011/cob/internal/manifest"
	"github.com/jmurray2011/cob/internal/output"
	"github.com/jmurray2011/cob/pkg/cob"
)

// resolveVersion returns the version from the flag, env var, or an error.
func resolveVersion(flag string) (string, error) {
	if flag != "" {
		return flag, nil
	}
	if v := os.Getenv("COB_VERSION"); v != "" {
		return v, nil
	}
	return "", fmt.Errorf("version is required: use --version or set COB_VERSION")
}

// isManifestPath returns true if the arg looks like a file path (ends in .yaml/.yml).
func isManifestPath(arg string) bool {
	return strings.HasSuffix(arg, ".yaml") || strings.HasSuffix(arg, ".yml")
}

// NamedSource pairs an asset name with its source, preserving manifest order.
type NamedSource struct {
	Name   string
	Source cob.AssetSource
}

// buildSources creates AssetSource instances from resolved manifest source URIs.
// Returns a slice that preserves the order from the manifest file.
func buildSources(m *manifest.Manifest, client *cob.Client) ([]NamedSource, error) {
	sources := make([]NamedSource, 0, len(m.Sources))
	for _, entry := range m.Sources {
		src, err := buildSource(entry.URI, m.Dir, client)
		if err != nil {
			return nil, fmt.Errorf("asset %q: %w", entry.Name, err)
		}
		sources = append(sources, NamedSource{Name: entry.Name, Source: src})
	}
	return sources, nil
}

func buildSource(uri, manifestDir string, client *cob.Client) (cob.AssetSource, error) {
	switch {
	case strings.HasPrefix(uri, "s3://"):
		return cob.NewS3Source(client.S3, uri)
	case strings.HasPrefix(uri, "ca://"):
		return cob.NewCASource(client.CodeArtifact, uri)
	case strings.HasPrefix(uri, "./"), strings.HasPrefix(uri, "/"):
		path := uri
		if !filepath.IsAbs(path) {
			path = filepath.Join(manifestDir, path)
		}
		return cob.NewFileSource(path, uri), nil
	default:
		// Treat as a relative path from the manifest directory.
		// Covers bare filenames like "README.md" or "subdir/file.bin".
		path := filepath.Join(manifestDir, uri)
		return cob.NewFileSource(path, uri), nil
	}
}

// resolveNamespaceIfNeeded finds the namespace for a package when only
// domain/repo/package was provided (3-segment coordinates). Searches all
// packages in the repo. No-op if namespace is already set.
func resolveNamespaceIfNeeded(ctx context.Context, coords *cob.PackageCoordinates, registry *cob.Registry) error {
	if coords.Namespace != "" || coords.Package == "" {
		return nil
	}

	packages, err := registry.ListPackages(ctx, coords.Domain, coords.Repository)
	if err != nil {
		return fmt.Errorf("searching for package %q: %w", coords.Package, err)
	}

	var matches []cob.PackageSummary
	for _, p := range packages {
		if p.Package == coords.Package {
			matches = append(matches, p)
		}
	}

	switch len(matches) {
	case 0:
		return fmt.Errorf("package %q not found in %s/%s", coords.Package, coords.Domain, coords.Repository)
	case 1:
		coords.Namespace = matches[0].Namespace
		return nil
	default:
		var namespaces []string
		for _, m := range matches {
			namespaces = append(namespaces, m.Namespace)
		}
		return fmt.Errorf("package %q exists in multiple namespaces: %v — use domain/repo/namespace/package to disambiguate",
			coords.Package, namespaces)
	}
}

// warnManifestOverrides logs any env var overrides applied to the manifest.
func warnManifestOverrides(m *manifest.Manifest, out *output.Writer) {
	for _, o := range m.Overrides {
		out.Warn("using %s=%s (overrides manifest %s)", o.Env, o.Value, o.Field)
	}
}

// resolveLatestIfNeeded checks if coords.Version is "latest" and, if so,
// resolves it to the most recently published version. Prints the resolved
// version so the user knows what they got.
func resolveLatestIfNeeded(ctx context.Context, coords *cob.PackageCoordinates, registry *cob.Registry, out *output.Writer) error {
	if coords.Version != "latest" {
		return nil
	}
	version, err := registry.ResolveLatest(ctx, coords)
	if err != nil {
		return err
	}
	coords.Version = version
	out.Header("Resolved latest -> %s", version)
	return nil
}

// confirmAction asks for confirmation unless --yes or non-TTY.
func confirmAction(yes bool, prompt string) bool {
	if yes {
		return true
	}
	// Auto-confirm in non-interactive mode.
	if !isInteractive() {
		return true
	}
	fmt.Fprintf(os.Stderr, "%s [y/N] ", prompt)
	var response string
	fmt.Scanln(&response)
	return strings.HasPrefix(strings.ToLower(response), "y")
}

func isInteractive() bool {
	info, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return (info.Mode() & os.ModeCharDevice) != 0
}
