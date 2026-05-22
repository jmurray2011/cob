package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

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
	byAsset := make(map[string]string, len(m.Sources)) // stored name -> manifest key
	for _, entry := range m.Sources {
		src, err := buildSource(entry.URI, m.Dir, client)
		if err != nil {
			return nil, fmt.Errorf("asset %q: %w", entry.Name, err)
		}
		// The stored CodeArtifact asset name is the source's basename, not
		// the manifest key — two sources with the same basename would
		// collide (one silently clobbering the other).
		if name := src.Filename(); name != "" {
			if prev, dup := byAsset[name]; dup {
				return nil, fmt.Errorf("sources %q and %q both publish as asset %q", prev, entry.Name, name)
			}
			byAsset[name] = entry.Name
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
		// A "scheme://" we don't recognise is a mistake, not a local file —
		// reject it rather than silently turning gs://b/x into a path.
		if i := strings.Index(uri, "://"); i > 0 {
			return nil, fmt.Errorf("unsupported source scheme in %q (use s3://, ca://, or a file path)", uri)
		}
		// Otherwise a relative path from the manifest directory — covers
		// bare filenames like "README.md" or "subdir/file.bin".
		path := filepath.Join(manifestDir, uri)
		return cob.NewFileSource(path, uri), nil
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

// confirmAction decides whether a mutating action may proceed.
//
//   - --yes: always proceeds.
//   - interactive TTY: prompts; only a "y"/"yes" answer proceeds.
//   - non-interactive (piped / CI) without --yes: refuses with an error
//     instead of silently auto-confirming. Auto-confirming a delete or
//     publish in a pipeline is a footgun; callers must pass --yes to opt in.
//
// A non-nil error means the action must not run and the command should fail
// loudly. (false, nil) means the user declined at the prompt — a clean,
// expected abort.
func confirmAction(yes bool, prompt string) (bool, error) {
	if yes {
		return true, nil
	}
	if !isInteractive() {
		return false, fmt.Errorf("refusing to proceed without confirmation: stdin is not a TTY; pass --yes to confirm")
	}
	fmt.Fprintf(os.Stderr, "%s [y/N] ", prompt)
	var response string
	fmt.Scanln(&response)
	return strings.HasPrefix(strings.ToLower(response), "y"), nil
}

func isInteractive() bool {
	info, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return (info.Mode() & os.ModeCharDevice) != 0
}

// finalizeProvenance publishes the cob-provenance.json finalizer for a
// freshly published or promoted version and folds the result into cmdResult.
// Publishing it with unfinished=false also flips the CodeArtifact version to
// Published. On failure it emits the operator guidance — failureHint is the
// command-specific middle line, since publish and promote describe the
// resulting half-written state differently — and returns a non-zero
// ExitError for the caller to return as-is.
func finalizeProvenance(ctx context.Context, publisher *cob.Publisher, coords *cob.PackageCoordinates,
	prov *cob.Provenance, out *output.Writer, cmdResult *cob.CommandResult, start time.Time, failureHint string) error {

	provSrc := cob.NewBytesSource(cob.ProvenanceFile, prov.Marshal())
	out.AssetStart(cob.ProvenanceFile, "", 0)
	par, err := publisher.PublishAsset(ctx, coords, cob.ProvenanceFile, provSrc, false)
	if err != nil {
		out.AssetFail(cob.ProvenanceFile, "", err)
		cmdResult.DurationMs = time.Since(start).Milliseconds()
		cmdResult.Status = "error"
		cmdResult.Error = err.Error()
		out.Error("%s\n  %s\n  Re-run with --force to delete and retry.", err, failureHint)
		out.CommandResult(cmdResult)
		return &ExitError{Code: cob.ExitError}
	}
	out.AssetOK(par, "")
	cmdResult.Assets = append(cmdResult.Assets, *par)
	cmdResult.TotalSize += par.Size
	cmdResult.DurationMs = time.Since(start).Milliseconds()
	return nil
}
