package cob

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/codeartifact"
)

const maxBufferSize = 512 * 1024 * 1024 // 512MB

// Publisher handles publishing assets to CodeArtifact.
type Publisher struct {
	client *Client
}

// NewPublisher creates a Publisher.
func NewPublisher(client *Client) *Publisher {
	return &Publisher{client: client}
}

// PublishAsset resolves a source, reads its bytes, and publishes to CodeArtifact.
//
// All transfers buffer in memory because PublishPackageVersion requires an
// io.ReadSeeker (for Content-Length + retries), not just an io.Reader. The
// distinction between "hash known upfront" and "hash computed" still matters:
// when the source provides a hash we skip the sha256 pass over the buffer.
// But the bytes always land in memory either way.
//
// When unfinished is true, the version is kept in Unfinished status so more
// assets can be added. The final asset should be published with unfinished=false
// to move the version to Published status.
func (p *Publisher) PublishAsset(ctx context.Context, coords *PackageCoordinates, name string, src AssetSource, unfinished bool) (*AssetResult, error) {
	start := time.Now()

	result := &AssetResult{
		Name:   name,
		Source: src.URI(),
		Method: "buffered",
	}

	// Step 1: Resolve — get metadata (size, possibly hash).
	meta, err := src.Resolve(ctx)
	if err != nil {
		result.SetError(err)
		return result, err
	}
	result.Size = meta.Size

	// Refuse to buffer huge assets that don't have a hash — same as before.
	// (Assets *with* a hash still buffer, but at least the user opted in
	// by uploading a large object to S3 with a checksum.)
	if meta.SHA256 == "" && meta.Size > maxBufferSize {
		err := fmt.Errorf("asset %q is %d bytes without SHA-256 metadata; max buffer is %d bytes. Add SHA-256 checksum to the S3 object", name, meta.Size, maxBufferSize)
		result.SetError(err)
		return result, err
	}

	// Step 2: Read the full body into memory.
	reader, err := src.Open(ctx)
	if err != nil {
		result.SetError(err)
		return result, err
	}
	buf, err := io.ReadAll(reader)
	reader.Close()
	if err != nil {
		result.SetError(err)
		return result, fmt.Errorf("reading %s: %w", name, err)
	}

	// Step 3: Determine the hash.
	hash := meta.SHA256
	if hash == "" {
		h := sha256.Sum256(buf)
		hash = hex.EncodeToString(h[:])
	}
	result.SHA256 = hash

	// Step 4: Publish to CodeArtifact.
	input := &codeartifact.PublishPackageVersionInput{
		Domain:         aws.String(coords.Domain),
		Repository:     aws.String(coords.Repository),
		Namespace:      aws.String(coords.Namespace),
		Package:        aws.String(coords.Package),
		PackageVersion: aws.String(coords.Version),
		Format:         FormatGeneric,
		AssetName:      aws.String(name),
		AssetSHA256:    aws.String(hash),
		AssetContent:   bytes.NewReader(buf),
	}
	if unfinished {
		input.Unfinished = aws.Bool(true)
	}
	_, err = p.client.CodeArtifact.PublishPackageVersion(ctx, input)
	if err != nil {
		result.SetError(err)
		return result, fmt.Errorf("publishing asset %q: %w", name, err)
	}

	result.Duration = time.Since(start)
	result.DurationMs = result.Duration.Milliseconds()
	return result, nil
}

// DeleteVersion deletes a package version (used by --force).
func (p *Publisher) DeleteVersion(ctx context.Context, coords *PackageCoordinates) error {
	_, err := p.client.CodeArtifact.DeletePackageVersions(ctx, &codeartifact.DeletePackageVersionsInput{
		Domain:     aws.String(coords.Domain),
		Repository: aws.String(coords.Repository),
		Namespace:  aws.String(coords.Namespace),
		Package:    aws.String(coords.Package),
		Format:     FormatGeneric,
		Versions:   []string{coords.Version},
	})
	if err != nil {
		return fmt.Errorf("deleting version %s: %w", coords.Version, err)
	}
	return nil
}
