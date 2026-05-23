package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jmurray2011/cob/internal/cob"
	"github.com/jmurray2011/cob/internal/manifest"
	"github.com/jmurray2011/cob/internal/output"
)

// fillClientMeta records the executing principal and region onto a
// CommandResult so audit pipelines can answer "which account/role ran
// this, in which region?" without grepping per-asset Origin or the
// provenance chain. Called right before out.CommandResult(result) on
// every command that has a live cob.Client; safe to call multiple times
// (CallerIdentity is cached on the Client).
func fillClientMeta(ctx context.Context, client *cob.Client, result *cob.CommandResult) {
	if client == nil || result == nil {
		return
	}
	result.Region = client.Region
	a := client.CallerIdentity(ctx)
	if a.ARN != "" || a.UserID != "" || a.Account != "" {
		result.Actor = &a
	}
}

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
			if name == cob.ProvenanceFile {
				return nil, fmt.Errorf("source %q uses the reserved asset name %q (cob writes that as the publish finalizer)", entry.Name, name)
			}
			if prev, dup := byAsset[name]; dup {
				return nil, fmt.Errorf("sources %q and %q both publish as asset %q", prev, entry.Name, name)
			}
			byAsset[name] = entry.Name
		}
		sources = append(sources, NamedSource{Name: entry.Name, Source: src})
	}
	return sources, nil
}

// uriKind is the scheme classification of a source URI.
type uriKind int

const (
	uriInvalid uriKind = iota // zero value: returned only alongside an error
	uriS3
	uriCA
	uriFile
)

// classifyURI determines what kind of source a (variable-resolved) URI is.
// For a file URI it also returns the path resolved against manifestDir. An
// unrecognised "scheme://" is an error, not a file path — buildSource (used
// by publish) and validateSourceURI (used by validate) both go through here,
// so the two commands cannot disagree on what a valid source is.
func classifyURI(uri, manifestDir string) (uriKind, string, error) {
	switch {
	case strings.HasPrefix(uri, "s3://"):
		return uriS3, "", nil
	case strings.HasPrefix(uri, "ca://"):
		return uriCA, "", nil
	case strings.HasPrefix(uri, "./"), strings.HasPrefix(uri, "/"):
		path := uri
		if !filepath.IsAbs(path) {
			path = filepath.Join(manifestDir, path)
		}
		return uriFile, path, nil
	default:
		// A "scheme://" we don't recognise is a mistake, not a local file —
		// reject it rather than silently turning gs://b/x into a path.
		if i := strings.Index(uri, "://"); i > 0 {
			return uriInvalid, "", fmt.Errorf("unsupported source scheme in %q (use s3://, ca://, or a file path)", uri)
		}
		// Otherwise a relative path from the manifest directory — covers
		// bare filenames like "README.md" or "subdir/file.bin".
		return uriFile, filepath.Join(manifestDir, uri), nil
	}
}

func buildSource(uri, manifestDir string, client *cob.Client) (cob.AssetSource, error) {
	kind, path, err := classifyURI(uri, manifestDir)
	if err != nil {
		return nil, err
	}
	switch kind {
	case uriS3:
		return cob.NewS3Source(client.S3, uri)
	case uriCA:
		return cob.NewCASource(client.CodeArtifact, uri)
	default: // uriFile
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
func confirmAction(ctx context.Context, yes bool, prompt string) (bool, error) {
	if yes {
		return true, nil
	}
	if !output.IsTerminal(os.Stdin) {
		return false, fmt.Errorf("refusing to proceed without confirmation: stdin is not a TTY; pass --yes to confirm")
	}
	fmt.Fprintf(os.Stderr, "%s [y/N] ", prompt)

	// Read in a goroutine so a SIGINT (which cancels ctx) interrupts the
	// prompt — fmt.Scanln is a blocking stdin read with no context awareness,
	// so without this Ctrl-C does nothing until the user presses Enter. The
	// buffered channel lets the goroutine finish even if ctx won the select.
	ch := make(chan string, 1)
	go func() {
		var response string
		_, _ = fmt.Scanln(&response) // a read error declines; nothing to handle
		ch <- response
	}()
	select {
	case <-ctx.Done():
		fmt.Fprintln(os.Stderr) // move off the "[y/N] " line
		return false, ctx.Err()
	case response := <-ch:
		return strings.HasPrefix(strings.ToLower(response), "y"), nil
	}
}

// finalizeProvenance publishes the cob-provenance.json finalizer for a
// freshly published or promoted version and folds the result into cmdResult.
// Publishing it with unfinished=false also flips the CodeArtifact version to
// Published. On failure it emits failureHint — the full command-specific
// operator guidance, since publish and promote recover differently — and
// returns a non-zero ExitError for the caller to return as-is.
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
		out.Error("%s\n  %s", err, failureHint)
		out.CommandResult(cmdResult)
		return &ExitError{Code: cob.ExitError}
	}
	out.AssetOK(par, "")
	cmdResult.Assets = append(cmdResult.Assets, *par)
	cmdResult.TotalSize += par.Size
	cmdResult.DurationMs = time.Since(start).Milliseconds()
	return nil
}
