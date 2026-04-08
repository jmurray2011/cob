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

// Promoter handles copying package versions between repositories.
type Promoter struct {
	client *Client
}

// NewPromoter creates a Promoter.
func NewPromoter(client *Client) *Promoter {
	return &Promoter{client: client}
}

// Promote copies all assets from srcRepo to dstRepo for the given package version.
func (p *Promoter) Promote(ctx context.Context, coords *PackageCoordinates, srcRepo, dstRepo string) ([]AssetResult, error) {
	// Collect all asset names with pagination.
	var assetNames []string
	var nextToken *string

	for {
		out, err := p.client.CodeArtifact.ListPackageVersionAssets(ctx, &codeartifact.ListPackageVersionAssetsInput{
			Domain:         aws.String(coords.Domain),
			Repository:     aws.String(srcRepo),
			Namespace:      aws.String(coords.Namespace),
			Package:        aws.String(coords.Package),
			PackageVersion: aws.String(coords.Version),
			Format:         FormatGeneric,
			NextToken:      nextToken,
		})
		if err != nil {
			return nil, fmt.Errorf("listing assets in %s: %w", srcRepo, err)
		}
		for _, asset := range out.Assets {
			assetNames = append(assetNames, aws.ToString(asset.Name))
		}
		if out.NextToken == nil {
			break
		}
		nextToken = out.NextToken
	}

	if len(assetNames) == 0 {
		return nil, fmt.Errorf("%s/%s@%s not found in %s. Promote to %s first.",
			coords.Namespace, coords.Package, coords.Version, srcRepo, srcRepo)
	}

	var results []AssetResult
	for i, name := range assetNames {
		isLast := i == len(assetNames)-1
		result, err := p.promoteAsset(ctx, coords, srcRepo, dstRepo, name, !isLast)
		if err != nil {
			return results, err
		}
		results = append(results, *result)
	}

	return results, nil
}

func (p *Promoter) promoteAsset(ctx context.Context, coords *PackageCoordinates, srcRepo, dstRepo, assetName string, unfinished bool) (*AssetResult, error) {
	start := time.Now()
	result := &AssetResult{Name: assetName, Method: "buffered"}

	// Download from source repo.
	getOut, err := p.client.CodeArtifact.GetPackageVersionAsset(ctx, &codeartifact.GetPackageVersionAssetInput{
		Domain:         aws.String(coords.Domain),
		Repository:     aws.String(srcRepo),
		Namespace:      aws.String(coords.Namespace),
		Package:        aws.String(coords.Package),
		PackageVersion: aws.String(coords.Version),
		Format:         FormatGeneric,
		Asset:          aws.String(assetName),
	})
	if err != nil {
		result.SetError(err)
		return result, fmt.Errorf("reading %q from %s: %w", assetName, srcRepo, err)
	}

	// Buffer the content to compute hash and get a ReadSeeker.
	buf, err := io.ReadAll(getOut.Asset)
	getOut.Asset.Close()
	if err != nil {
		result.SetError(err)
		return result, fmt.Errorf("buffering %q: %w", assetName, err)
	}

	h := sha256.Sum256(buf)
	hash := hex.EncodeToString(h[:])

	result.Size = int64(len(buf))
	result.SHA256 = hash

	// Publish to destination repo.
	input := &codeartifact.PublishPackageVersionInput{
		Domain:         aws.String(coords.Domain),
		Repository:     aws.String(dstRepo),
		Namespace:      aws.String(coords.Namespace),
		Package:        aws.String(coords.Package),
		PackageVersion: aws.String(coords.Version),
		Format:         FormatGeneric,
		AssetName:      aws.String(assetName),
		AssetSHA256:    aws.String(hash),
		AssetContent:   bytes.NewReader(buf),
	}
	if unfinished {
		input.Unfinished = aws.Bool(true)
	}
	_, err = p.client.CodeArtifact.PublishPackageVersion(ctx, input)
	if err != nil {
		result.SetError(err)
		return result, fmt.Errorf("publishing %q to %s: %w", assetName, dstRepo, err)
	}

	result.Duration = time.Since(start)
	result.DurationMs = result.Duration.Milliseconds()
	return result, nil
}
