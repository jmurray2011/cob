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
	client    CodeArtifactAPI
	domain    string
	repo      string
	namespace string
	pkg       string
	version   string
	asset     string
	uri       string
}

// NewCASource creates a CASource from a URI like ca://domain/repo/ns/pkg@ver/asset.
func NewCASource(client CodeArtifactAPI, uri string) (*CASource, error) {
	trimmed := strings.TrimPrefix(uri, "ca://")

	// domain / repo / namespace / package@version / asset...
	// The asset is everything after the version and may itself contain
	// slashes — CodeArtifact generic asset names are path-like — so cap the
	// split at 5 fields and treat the remainder as the asset name.
	parts := strings.SplitN(trimmed, "/", 5)
	if len(parts) < 5 || parts[4] == "" {
		return nil, fmt.Errorf("invalid CodeArtifact URI %q: expected ca://domain/repo/namespace/package@version/asset", uri)
	}

	pkgVer := parts[3]
	at := strings.LastIndex(pkgVer, "@")
	if at < 0 {
		return nil, fmt.Errorf("invalid CodeArtifact URI %q: missing @version", uri)
	}
	pkg := pkgVer[:at]
	version := pkgVer[at+1:]
	if pkg == "" || version == "" {
		return nil, fmt.Errorf("invalid CodeArtifact URI %q: empty package or version", uri)
	}
	if version == "latest" {
		return nil, fmt.Errorf("invalid CodeArtifact URI %q: @latest is not supported in source URIs (use ${VERSION} instead)", uri)
	}

	return &CASource{
		client:    client,
		domain:    parts[0],
		repo:      parts[1],
		namespace: parts[2],
		pkg:       pkg,
		version:   version,
		asset:     parts[4],
		uri:       uri,
	}, nil
}

func (c *CASource) URI() string { return c.uri }

func (c *CASource) Filename() string { return c.asset }

func (c *CASource) Resolve(ctx context.Context) (*AssetMetadata, error) {
	var nextToken *string
	pages := 0
	for {
		out, err := c.client.ListPackageVersionAssets(ctx, &codeartifact.ListPackageVersionAssetsInput{
			Domain:         aws.String(c.domain),
			Repository:     aws.String(c.repo),
			Namespace:      aws.String(c.namespace),
			Package:        aws.String(c.pkg),
			PackageVersion: aws.String(c.version),
			Format:         FormatGeneric,
			NextToken:      nextToken,
		})
		if err != nil {
			return nil, fmt.Errorf("listing assets for %s: %w", c.uri, err)
		}

		for _, a := range out.Assets {
			if aws.ToString(a.Name) == c.asset {
				meta := &AssetMetadata{Size: aws.ToInt64(a.Size)}
				for k, v := range a.Hashes {
					if k == "SHA-256" {
						meta.SHA256 = v
						break
					}
				}
				return meta, nil
			}
		}

		if out.NextToken == nil {
			break
		}
		pages++
		if pages >= maxPaginationIterations {
			return nil, fmt.Errorf("listing assets for %s: hit pagination safety cap of %d pages", c.uri, maxPaginationIterations)
		}
		nextToken = out.NextToken
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

func (c *CASource) Origin(ctx context.Context) (*Origin, error) {
	o := &Origin{
		Type:         "ca",
		Domain:       c.domain,
		CARepository: c.repo,
		Namespace:    c.namespace,
		Package:      c.pkg,
		CAVersion:    c.version,
		CAAsset:      c.asset,
	}
	// Embed the upstream's own (already-complete) provenance. One fetch per
	// direct ca:// source; deep history rides along inside it. An upstream
	// without provenance (non-cob / pre-provenance publisher) is recorded
	// honestly rather than treated as an error.
	//
	// PruneUpstreamProvenance caps the embedded chain at maxUpstreamDepth
	// counting from the new root: this source's `up` is depth 1, so we
	// permit maxUpstreamDepth-1 further levels below it. Deeper history
	// stays reachable via cob log but doesn't balloon the document we're
	// about to publish.
	up, err := FetchProvenance(ctx, c.client, &PackageCoordinates{
		Domain: c.domain, Repository: c.repo, Namespace: c.namespace,
		Package: c.pkg, Version: c.version,
	})
	switch {
	case err == nil && up != nil:
		PruneUpstreamProvenance(up, maxUpstreamDepth-1)
		o.UpstreamStatus = UpstreamEmbedded
		o.UpstreamProvenance = up
	default:
		o.UpstreamStatus = UpstreamNoProv
	}
	return o, nil
}
