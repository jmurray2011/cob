package cliutil

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jmurray2011/cob/internal/cob"
	"github.com/jmurray2011/cob/internal/manifest"
)

// NamedSource pairs an asset name with its source, preserving manifest order.
type NamedSource struct {
	Name   string
	Source cob.AssetSource
}

// BuildSources creates AssetSource instances from resolved manifest source URIs.
// Returns a slice that preserves the order from the manifest file. Goes through
// RegisterAssetName so the same uniqueness / reserved-name rules apply here
// as in ValidateManifest — the two paths can never disagree.
func BuildSources(m *manifest.Manifest, client *cob.Client) ([]NamedSource, error) {
	sources := make([]NamedSource, 0, len(m.Sources))
	byAsset := make(map[string]string, len(m.Sources)) // stored name -> manifest key
	for _, entry := range m.Sources {
		src, err := BuildSource(entry.URI, m.Dir, client)
		if err != nil {
			return nil, fmt.Errorf("asset %q: %w", entry.Name, err)
		}
		// The stored CodeArtifact asset name is the source's basename, not
		// the manifest key — two sources with the same basename would
		// collide (one silently clobbering the other).
		if name := src.Filename(); name != "" {
			if err := RegisterAssetName(byAsset, name, entry.Name); err != nil {
				return nil, fmt.Errorf("source %q: %w", entry.Name, err)
			}
		}
		sources = append(sources, NamedSource{Name: entry.Name, Source: src})
	}
	return sources, nil
}

// URIKind is the scheme classification of a source URI.
type URIKind int

const (
	URIInvalid URIKind = iota // zero value: returned only alongside an error
	URIS3
	URICA
	URIFile
)

// ClassifyURI determines what kind of source a (variable-resolved) URI is.
// For a file URI it also returns the path resolved against manifestDir. An
// unrecognised "scheme://" is an error, not a file path — BuildSource (used
// by publish) and ValidateSourceURI (used by validate) both go through here,
// so the two commands cannot disagree on what a valid source is.
func ClassifyURI(uri, manifestDir string) (URIKind, string, error) {
	switch {
	case strings.HasPrefix(uri, "s3://"):
		return URIS3, "", nil
	case strings.HasPrefix(uri, "ca://"):
		return URICA, "", nil
	case strings.HasPrefix(uri, "./"), strings.HasPrefix(uri, "/"):
		path := uri
		if !filepath.IsAbs(path) {
			path = filepath.Join(manifestDir, path)
		}
		return URIFile, path, nil
	default:
		// A "scheme://" we don't recognise is a mistake, not a local file —
		// reject it rather than silently turning gs://b/x into a path.
		if i := strings.Index(uri, "://"); i > 0 {
			return URIInvalid, "", fmt.Errorf("unsupported source scheme in %q (use s3://, ca://, or a file path)", uri)
		}
		// Otherwise a relative path from the manifest directory — covers
		// bare filenames like "README.md" or "subdir/file.bin".
		return URIFile, filepath.Join(manifestDir, uri), nil
	}
}

// BuildSource constructs a single AssetSource for a (variable-resolved) URI.
// Centralized so publish, pull, diff, and validate all agree on what a URI
// means.
func BuildSource(uri, manifestDir string, client *cob.Client) (cob.AssetSource, error) {
	kind, path, err := ClassifyURI(uri, manifestDir)
	if err != nil {
		return nil, err
	}
	switch kind {
	case URIS3:
		return cob.NewS3Source(client.S3, uri)
	case URICA:
		return cob.NewCASource(client.CodeArtifact, uri)
	default: // URIFile
		return cob.NewFileSource(path, uri), nil
	}
}

// ValidateSourceURI checks a (variable-resolved) source URI's syntax
// without any network call and returns the stored asset name (basename)
// it would publish as, its size, and the kind so callers can describe
// exactly what was verified (local existence vs remote syntax-only).
// For local files, size is the real on-disk size and existence is
// asserted; for remote sources, size is 0 — sizing them would need a
// network call validation deliberately avoids. Shares ClassifyURI with
// publish's BuildSource, so the two paths agree on which URIs are
// valid.
func ValidateSourceURI(uri, manifestDir string) (asset string, size int64, kind URIKind, err error) {
	kind, path, err := ClassifyURI(uri, manifestDir)
	if err != nil {
		return "", 0, kind, err
	}
	switch kind {
	case URIS3:
		s, sErr := cob.NewS3Source(nil, uri)
		if sErr != nil {
			return "", 0, kind, sErr
		}
		return s.Filename(), 0, kind, nil
	case URICA:
		s, cErr := cob.NewCASource(nil, uri)
		if cErr != nil {
			return "", 0, kind, cErr
		}
		return s.Filename(), 0, kind, nil
	default: // URIFile
		info, sErr := os.Stat(path)
		if sErr != nil {
			return "", 0, kind, fmt.Errorf("local source not found: %s", path)
		}
		if !info.Mode().IsRegular() {
			// FIFOs, sockets, device nodes etc. would hang the eventual
			// publish; fail at validation time instead.
			return "", 0, kind, fmt.Errorf("local source is not a regular file: %s (mode %s)", path, info.Mode())
		}
		return filepath.Base(path), info.Size(), kind, nil
	}
}
