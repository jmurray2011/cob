package cob

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/codeartifact"
)

// CASource reads an asset from another CodeArtifact package.
// URI format: ca://domain/repo/namespace/package@version/asset-name
type CASource struct {
	client    *codeartifact.Client
	domain    string
	repo      string
	namespace string
	pkg       string
	version   string
	asset     string
	uri       string

	meta *AssetMetadata
}

// NewCASource creates a CASource from a URI like ca://domain/repo/ns/pkg@ver/asset.
func NewCASource(client *codeartifact.Client, uri string) (*CASource, error) {
	trimmed := strings.TrimPrefix(uri, "ca://")

	// Split off the asset name (last segment after the version).
	// Format: domain/repo/namespace/package@version/asset
	parts := strings.Split(trimmed, "/")
	if len(parts) != 5 {
		return nil, fmt.Errorf("invalid CodeArtifact URI %q: expected ca://domain/repo/namespace/package@version/asset", uri)
	}

	// Parse package@version from parts[3]
	pkgVer := parts[3]
	at := strings.LastIndex(pkgVer, "@")
	if at < 0 {
		return nil, fmt.Errorf("invalid CodeArtifact URI %q: missing @version", uri)
	}

	version := pkgVer[at+1:]
	if version == "latest" {
		return nil, fmt.Errorf("invalid CodeArtifact URI %q: @latest is not supported in source URIs (use ${VERSION} instead)", uri)
	}

	return &CASource{
		client:    client,
		domain:    parts[0],
		repo:      parts[1],
		namespace: parts[2],
		pkg:       pkgVer[:at],
		version:   pkgVer[at+1:],
		asset:     parts[4],
		uri:       uri,
	}, nil
}

func (c *CASource) URI() string { return c.uri }

func (c *CASource) Resolve(ctx context.Context) (*AssetMetadata, error) {
	out, err := c.client.ListPackageVersionAssets(ctx, &codeartifact.ListPackageVersionAssetsInput{
		Domain:         aws.String(c.domain),
		Repository:     aws.String(c.repo),
		Namespace:      aws.String(c.namespace),
		Package:        aws.String(c.pkg),
		PackageVersion: aws.String(c.version),
		Format:         FormatGeneric,
	})
	if err != nil {
		return nil, fmt.Errorf("listing assets for %s: %w", c.uri, err)
	}

	for _, a := range out.Assets {
		if aws.ToString(a.Name) == c.asset {
			meta := &AssetMetadata{
				Size: aws.ToInt64(a.Size),
			}
			// The SHA-256 is available from the hashes map.
			for k, v := range a.Hashes {
				if k == "SHA-256" {
					meta.SHA256 = v
					break
				}
			}
			c.meta = meta
			return meta, nil
		}
	}

	return nil, fmt.Errorf("asset %q not found in %s/%s@%s", c.asset, c.namespace, c.pkg, c.version)
}

func (c *CASource) Open(ctx context.Context) (io.ReadCloser, error) {
	out, err := c.client.GetPackageVersionAsset(ctx, &codeartifact.GetPackageVersionAssetInput{
		Domain:         aws.String(c.domain),
		Repository:     aws.String(c.repo),
		Namespace:      aws.String(c.namespace),
		Package:        aws.String(c.pkg),
		PackageVersion: aws.String(c.version),
		Format:         FormatGeneric,
		Asset:          aws.String(c.asset),
	})
	if err != nil {
		return nil, fmt.Errorf("getting asset %s: %w", c.uri, err)
	}
	return out.Asset, nil
}
