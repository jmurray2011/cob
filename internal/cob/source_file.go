package cob

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// FileSource reads an asset from the local filesystem.
type FileSource struct {
	path string
	uri  string
}

// NewFileSource creates a FileSource. The path should already be resolved
// relative to the manifest directory.
func NewFileSource(path, uri string) *FileSource {
	return &FileSource{path: path, uri: uri}
}

func (f *FileSource) URI() string { return f.uri }

func (f *FileSource) Filename() string { return filepath.Base(f.path) }

// Resolve stats the file for its size and returns an empty SHA256. A local
// file has no advertised hash to cross-check against — it is ground truth —
// so the authoritative hash is the one spillToTemp computes while streaming
// in the publish path. Hashing here too would read every local asset from
// disk twice. (Remote sources still return their advertised hash, which the
// streamed hash is checked against.)
func (f *FileSource) Resolve(_ context.Context) (*AssetMetadata, error) {
	info, err := os.Stat(f.path)
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", f.path, err)
	}
	if info.IsDir() {
		return nil, fmt.Errorf("%s is a directory, not a file", f.path)
	}
	return &AssetMetadata{Size: info.Size()}, nil
}

func (f *FileSource) Open(_ context.Context) (io.ReadCloser, error) {
	file, err := os.Open(f.path)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", f.path, err)
	}
	return file, nil
}

func (f *FileSource) Origin(_ context.Context) (*Origin, error) {
	// Record the manifest-relative URI rather than the absolute filesystem
	// path: provenance ships to a shared registry, and absolute paths leak
	// the publisher's username and directory layout.
	o := &Origin{Type: "file", Path: f.uri}
	if info, err := os.Stat(f.path); err == nil {
		o.Mtime = info.ModTime().UTC().Format(time.RFC3339)
	}
	return o, nil
}
