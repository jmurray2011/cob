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
	// Progress, when set, receives byte-count deltas during each download.
	Progress func(int64)
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
//
// The download is hashed while streaming and the SHA-256 is checked against
// the expected value — a corrupt or truncated transfer is rejected, never
// silently accepted. Bytes land in a temp file in the destination directory
// and are renamed into place only after the hash verifies, so an
// interrupted pull can't leave a truncated file (or destroy the prior one).
// If the file already exists with a matching SHA-256 the download is
// skipped (Method "skipped").
func (p *Puller) PullAsset(ctx context.Context, coords *PackageCoordinates, info AssetInfo, outputPath string) (*AssetResult, error) {
	start := time.Now()
	result := &AssetResult{Name: info.Name, Size: info.Size, Method: "buffered"}

	// Skip if it already exists with the expected hash.
	if info.SHA256 != "" {
		if existing, err := hashFile(outputPath); err == nil && existing == info.SHA256 {
			result.Method = "skipped"
			result.SHA256 = existing
			result.DurationMs = time.Since(start).Milliseconds()
			return result, nil
		}
	}

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

	dir := filepath.Dir(outputPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		result.SetError(err)
		return result, err
	}

	// Stream to a temp file in the destination dir (same filesystem → atomic
	// rename), hashing as we go.
	tmp, err := os.CreateTemp(dir, ".cob-pull-*")
	if err != nil {
		result.SetError(err)
		return result, err
	}
	tmpName := tmp.Name()
	h := sha256.New()
	var src io.Reader = out.Asset
	if p.Progress != nil {
		src = countingReader{r: src, report: p.Progress}
	}
	n, copyErr := io.Copy(io.MultiWriter(tmp, h), src)
	closeErr := tmp.Close()
	if copyErr != nil || closeErr != nil {
		os.Remove(tmpName)
		err := copyErr
		if err == nil {
			err = closeErr
		}
		result.SetError(err)
		return result, fmt.Errorf("writing %s: %w", outputPath, err)
	}

	got := hex.EncodeToString(h.Sum(nil))
	if info.SHA256 != "" && got != info.SHA256 {
		os.Remove(tmpName)
		err := fmt.Errorf("asset %q: downloaded SHA-256 %s != expected %s", info.Name, got, info.SHA256)
		result.SetError(err)
		return result, err
	}
	// os.CreateTemp makes the file 0600; restore the conventional 0644 a
	// direct download would have, so pulled assets stay readable.
	if err := os.Chmod(tmpName, 0o644); err != nil {
		os.Remove(tmpName)
		result.SetError(err)
		return result, err
	}
	if err := os.Rename(tmpName, outputPath); err != nil {
		os.Remove(tmpName)
		result.SetError(err)
		return result, err
	}

	result.SHA256 = got
	result.Size = n
	result.DurationMs = time.Since(start).Milliseconds()
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
