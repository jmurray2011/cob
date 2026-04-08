package cob

import (
	"context"
	"io"
)

// AssetMetadata is the information needed before publishing: hash and size.
type AssetMetadata struct {
	Size   int64
	SHA256 string // hex-encoded
}

// AssetSource abstracts where an asset's bytes come from.
// Implementations exist for S3, CodeArtifact, and local files.
type AssetSource interface {
	// URI returns the original source URI for display purposes.
	URI() string

	// Resolve checks that the source exists and returns metadata.
	// For sources with a known hash (S3 with checksum, CodeArtifact),
	// this returns the hash. For others, SHA256 may be empty.
	Resolve(ctx context.Context) (*AssetMetadata, error)

	// Open returns a reader for the asset bytes.
	// The caller always buffers the full body because PublishPackageVersion
	// requires an io.ReadSeeker. When Resolve returned a SHA256, the caller
	// can skip re-hashing the buffer.
	Open(ctx context.Context) (io.ReadCloser, error)
}
