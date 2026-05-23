package cob

import (
	"context"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/codeartifact"
)

// Promoter handles copying package versions between repositories.
type Promoter struct {
	client *Client
	// Progress, when set, receives (name, delta) byte updates as each
	// asset spills to the temp file — name lets one shared callback drive
	// a multi-row live display without races.
	Progress func(name string, delta int64)
}

// NewPromoter creates a Promoter.
func NewPromoter(client *Client) *Promoter {
	return &Promoter{client: client}
}

// ListAssetsToPromote enumerates the assets in the source repo for the given
// package version, in the order they should be re-published to the destination.
// Returns an error with guidance when the version isn't present in srcRepo.
func (p *Promoter) ListAssetsToPromote(ctx context.Context, coords *PackageCoordinates, srcRepo string) ([]string, error) {
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
		return nil, fmt.Errorf("%s/%s@%s not found in %s -- promote it to %s first",
			coords.Namespace, coords.Package, coords.Version, srcRepo, srcRepo)
	}
	return assetNames, nil
}

// PromoteAsset copies a single asset from srcRepo to dstRepo. When unfinished
// is true the destination version is kept in Unfinished status so more assets
// can be added. The final asset in a batch must be published with
// unfinished=false to move the version to Published.
func (p *Promoter) PromoteAsset(ctx context.Context, coords *PackageCoordinates, srcRepo, dstRepo, assetName string, unfinished bool) (*AssetResult, error) {
	start := time.Now()
	result := &AssetResult{Name: assetName, Kind: KindTransfer, Method: TransferSpilled}

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

	// Stream to a temp file (bounded memory, no size cap), hashing in the
	// same pass, to get the io.ReadSeeker PublishPackageVersion needs.
	ta, err := spillToTemp(getOut.Asset, p.client.TmpDir, assetName, p.Progress)
	getOut.Asset.Close()
	if err != nil {
		result.SetError(err)
		return result, fmt.Errorf("buffering %q: %w", assetName, err)
	}
	defer ta.Close()

	result.Size = ta.Size
	result.SHA256 = ta.SHA256

	// Publish to destination repo.
	input := &codeartifact.PublishPackageVersionInput{
		Domain:         aws.String(coords.Domain),
		Repository:     aws.String(dstRepo),
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
		return result, fmt.Errorf("publishing %q to %s: %w", assetName, dstRepo, err)
	}

	result.DurationMs = time.Since(start).Milliseconds()
	return result, nil
}
