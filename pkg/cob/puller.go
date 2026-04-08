package cob

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/codeartifact"
)

// AssetInfo holds pre-fetched metadata for a single asset.
type AssetInfo struct {
	Name   string
	Size   int64
	SHA256 string
}

// Puller handles downloading assets from CodeArtifact.
type Puller struct {
	client *Client
}

// NewPuller creates a Puller.
func NewPuller(client *Client) *Puller {
	return &Puller{client: client}
}

// FetchAssetInfo makes a single ListPackageVersionAssets call and returns
// metadata for all assets in the version. Callers pass individual entries
// into PullAsset to avoid redundant API calls.
func (p *Puller) FetchAssetInfo(ctx context.Context, coords *PackageCoordinates) ([]AssetInfo, error) {
	var all []AssetInfo
	var nextToken *string

	for {
		out, err := p.client.CodeArtifact.ListPackageVersionAssets(ctx, &codeartifact.ListPackageVersionAssetsInput{
			Domain:         aws.String(coords.Domain),
			Repository:     aws.String(coords.Repository),
			Namespace:      aws.String(coords.Namespace),
			Package:        aws.String(coords.Package),
			PackageVersion: aws.String(coords.Version),
			Format:         FormatGeneric,
			NextToken:      nextToken,
		})
		if err != nil {
			return nil, fmt.Errorf("listing assets for %s/%s@%s: %w",
				coords.Namespace, coords.Package, coords.Version, err)
		}

		for _, a := range out.Assets {
			hash := ""
			for k, v := range a.Hashes {
				if k == "SHA-256" {
					hash = v
					break
				}
			}
			all = append(all, AssetInfo{
				Name:   aws.ToString(a.Name),
				Size:   aws.ToInt64(a.Size),
				SHA256: hash,
			})
		}

		if out.NextToken == nil {
			break
		}
		nextToken = out.NextToken
	}

	return all, nil
}

// PullAsset downloads a single asset from CodeArtifact to a local path.
// If the file already exists with a matching SHA-256, it is skipped and
// the result has Method "skipped".
func (p *Puller) PullAsset(ctx context.Context, coords *PackageCoordinates, info AssetInfo, outputPath string) (*AssetResult, error) {
	start := time.Now()
	result := &AssetResult{
		Name:   info.Name,
		Size:   info.Size,
		SHA256: info.SHA256,
		Method: "buffered",
	}

	// Check if already exists with matching hash.
	if existingHash, err := hashFile(outputPath); err == nil && existingHash == info.SHA256 {
		result.Method = "skipped"
		result.Duration = time.Since(start)
		result.DurationMs = result.Duration.Milliseconds()
		return result, nil
	}

	// Download the asset.
	out, err := p.client.CodeArtifact.GetPackageVersionAsset(ctx, &codeartifact.GetPackageVersionAssetInput{
		Domain:         aws.String(coords.Domain),
		Repository:     aws.String(coords.Repository),
		Namespace:      aws.String(coords.Namespace),
		Package:        aws.String(coords.Package),
		PackageVersion: aws.String(coords.Version),
		Format:         FormatGeneric,
		Asset:          aws.String(info.Name),
	})
	if err != nil {
		result.SetError(err)
		return result, fmt.Errorf("downloading asset %q: %w", info.Name, err)
	}
	defer out.Asset.Close()

	// Ensure output directory exists.
	if err := os.MkdirAll(filepath.Dir(outputPath), 0o755); err != nil {
		result.SetError(err)
		return result, err
	}

	f, err := os.Create(outputPath)
	if err != nil {
		result.SetError(err)
		return result, err
	}
	defer f.Close()

	if _, err := io.Copy(f, out.Asset); err != nil {
		result.SetError(err)
		return result, fmt.Errorf("writing %s: %w", outputPath, err)
	}

	result.Duration = time.Since(start)
	result.DurationMs = result.Duration.Milliseconds()
	return result, nil
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
