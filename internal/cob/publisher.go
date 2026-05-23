package cob

import (
	"context"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/codeartifact"
	catypes "github.com/aws/aws-sdk-go-v2/service/codeartifact/types"
)

// Publisher handles publishing assets to CodeArtifact.
type Publisher struct {
	client *Client
	// Progress, when set, receives (name, delta) byte updates as each
	// asset spills to the temp file — name lets one shared callback drive
	// a multi-row live display without races.
	Progress func(name string, delta int64)
}

// NewPublisher creates a Publisher.
func NewPublisher(client *Client) *Publisher {
	return &Publisher{client: client}
}

// PublishAsset resolves a source, reads its bytes, and publishes to CodeArtifact.
//
// The CodeArtifact AssetName is derived from the source's filename (e.g. the
// last segment of an s3:// key, or the asset name in a ca:// URI), not from
// the manifest's YAML key. The YAML key is just a label for display and for
// consumers looking up assets by a stable handle in higher-level tooling.
//
// PublishPackageVersion requires an io.ReadSeeker (for Content-Length +
// retries), so the source is streamed to a temporary file rather than held
// in memory — memory stays bounded regardless of asset size and there is no
// size ceiling. SHA-256 is computed during that single streaming pass; if
// the source advertised a hash and it disagrees, that's an integrity error.
//
// When unfinished is true, the version is kept in Unfinished status so more
// assets can be added. The final asset should be published with unfinished=false
// to move the version to Published status.
func (p *Publisher) PublishAsset(ctx context.Context, coords *PackageCoordinates, name string, src AssetSource, unfinished bool) (*AssetResult, error) {
	start := time.Now()

	assetName := src.Filename()
	if assetName == "" {
		err := fmt.Errorf("asset %q: could not derive filename from source %s", name, src.URI())
		return &AssetResult{Name: name, Source: src.URI(), ErrorMsg: err.Error(), Error: err}, err
	}

	result := &AssetResult{
		Name:   name,
		Source: src.URI(),
		Method: "spilled",
	}

	// Step 1: Resolve — get metadata (size, possibly hash).
	meta, err := src.Resolve(ctx)
	if err != nil {
		result.SetError(err)
		return result, err
	}
	result.Size = meta.Size

	// Step 2: Stream the body to a temp file (bounded memory, no size cap),
	// hashing in the same pass.
	reader, err := src.Open(ctx)
	if err != nil {
		result.SetError(err)
		return result, err
	}
	ta, err := spillToTemp(reader, p.client.TmpDir, assetName, p.Progress)
	reader.Close()
	if err != nil {
		result.SetError(err)
		return result, fmt.Errorf("reading %s: %w", name, err)
	}
	defer ta.Close()

	// Step 3: The computed hash is authoritative. If the source advertised
	// one and it disagrees, the bytes changed in flight — refuse.
	if meta.SHA256 != "" && meta.SHA256 != ta.SHA256 {
		err := fmt.Errorf("asset %q: source SHA-256 %s != streamed %s", name, meta.SHA256, ta.SHA256)
		result.SetError(err)
		return result, err
	}
	result.Size = ta.Size
	result.SHA256 = ta.SHA256

	// Step 4: Publish to CodeArtifact.
	input := &codeartifact.PublishPackageVersionInput{
		Domain:         aws.String(coords.Domain),
		Repository:     aws.String(coords.Repository),
		Namespace:      aws.String(coords.Namespace),
		Package:        aws.String(coords.Package),
		PackageVersion: aws.String(coords.Version),
		Format:         FormatGeneric,
		AssetName:      aws.String(assetName),
		AssetSHA256:    aws.String(ta.SHA256),
		AssetContent:   ta.f,
	}
	if unfinished {
		input.Unfinished = aws.Bool(true)
	}
	_, err = p.client.CodeArtifact.PublishPackageVersion(ctx, input)
	if err != nil {
		result.SetError(err)
		return result, fmt.Errorf("publishing asset %q: %w", name, err)
	}

	result.DurationMs = time.Since(start).Milliseconds()
	return result, nil
}

// DeleteVersion deletes a package version (used by --force).
//
// DeletePackageVersions returns HTTP 200 even when it refuses to delete a
// version: the per-version outcome lands in FailedVersions, not the
// top-level error. We must inspect it, otherwise --force would silently
// republish on top of a version it never actually removed. A NOT_FOUND
// failure is benign here — the version is already gone, which is the state
// --force wants.
func (p *Publisher) DeleteVersion(ctx context.Context, coords *PackageCoordinates) error {
	out, err := p.client.CodeArtifact.DeletePackageVersions(ctx, &codeartifact.DeletePackageVersionsInput{
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

	if verr, failed := out.FailedVersions[coords.Version]; failed && verr.ErrorCode != catypes.PackageVersionErrorCodeNotFound {
		if msg := aws.ToString(verr.ErrorMessage); msg != "" {
			return fmt.Errorf("deleting version %s: %s: %s", coords.Version, verr.ErrorCode, msg)
		}
		return fmt.Errorf("deleting version %s: %s", coords.Version, verr.ErrorCode)
	}
	return nil
}
