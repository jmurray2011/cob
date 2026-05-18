package cob

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
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

func (f *FileSource) Resolve(_ context.Context) (*AssetMetadata, error) {
	file, err := os.Open(f.path)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", f.path, err)
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", f.path, err)
	}

	h := sha256.New()
	if _, err := io.Copy(h, file); err != nil {
		return nil, fmt.Errorf("hashing %s: %w", f.path, err)
	}

	return &AssetMetadata{
		Size:   info.Size(),
		SHA256: hex.EncodeToString(h.Sum(nil)),
	}, nil
}

func (f *FileSource) Open(_ context.Context) (io.ReadCloser, error) {
	file, err := os.Open(f.path)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", f.path, err)
	}
	return file, nil
}
