package cob

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/codeartifact"
	catypes "github.com/aws/aws-sdk-go-v2/service/codeartifact/types"
)

// versionMetaConcurrency bounds the parallel per-version metadata lookups
// done by ListVersions, to keep `cob ls <pkg>` responsive without hammering
// the CodeArtifact API into throttling.
const versionMetaConcurrency = 8

// isNotFound returns true if the error is a CodeArtifact ResourceNotFoundException.
func isNotFound(err error) bool {
	var rnf *catypes.ResourceNotFoundException
	return errors.As(err, &rnf)
}

// Registry handles listing and querying CodeArtifact.
type Registry struct {
	client *Client
}

// NewRegistry creates a Registry.
func NewRegistry(client *Client) *Registry {
	return &Registry{client: client}
}

// DomainSummary is returned by domain listing.
type DomainSummary struct {
	Name   string `json:"name"`
	Owner  string `json:"owner"`
	Status string `json:"status"`
}

// ListDomains returns all domains accessible to the caller.
func (r *Registry) ListDomains(ctx context.Context) ([]DomainSummary, error) {
	var results []DomainSummary
	var nextToken *string

	for {
		out, err := r.client.CodeArtifact.ListDomains(ctx, &codeartifact.ListDomainsInput{
			NextToken: nextToken,
		})
		if err != nil {
			return nil, fmt.Errorf("listing domains: %w", err)
		}
		for _, d := range out.Domains {
			results = append(results, DomainSummary{
				Name:   aws.ToString(d.Name),
				Owner:  aws.ToString(d.Owner),
				Status: string(d.Status),
			})
		}
		if out.NextToken == nil {
			break
		}
		nextToken = out.NextToken
	}

	return results, nil
}

// ListRepositories returns all repository names in a domain.
func (r *Registry) ListRepositories(ctx context.Context, domain string) ([]string, error) {
	var repos []string
	var nextToken *string

	for {
		out, err := r.client.CodeArtifact.ListRepositoriesInDomain(ctx, &codeartifact.ListRepositoriesInDomainInput{
			Domain:    aws.String(domain),
			NextToken: nextToken,
		})
		if err != nil {
			return nil, fmt.Errorf("listing repositories in %s: %w", domain, err)
		}
		for _, repo := range out.Repositories {
			repos = append(repos, aws.ToString(repo.Name))
		}
		if out.NextToken == nil {
			break
		}
		nextToken = out.NextToken
	}

	return repos, nil
}

// ListPackages returns packages in a repository.
func (r *Registry) ListPackages(ctx context.Context, domain, repo string) ([]PackageSummary, error) {
	var results []PackageSummary
	var nextToken *string

	for {
		out, err := r.client.CodeArtifact.ListPackages(ctx, &codeartifact.ListPackagesInput{
			Domain:     aws.String(domain),
			Repository: aws.String(repo),
			Format:     FormatGeneric,
			NextToken:  nextToken,
		})
		if err != nil {
			return nil, fmt.Errorf("listing packages in %s/%s: %w", domain, repo, err)
		}

		for _, p := range out.Packages {
			ns := ""
			if p.Namespace != nil {
				ns = *p.Namespace
			}
			pkg := aws.ToString(p.Package)

			latest, count, err := r.getLatestVersion(ctx, domain, repo, ns, pkg)
			if err != nil {
				latest = "?"
				count = 0
			}

			results = append(results, PackageSummary{
				Namespace:     ns,
				Package:       pkg,
				LatestVersion: latest,
				VersionCount:  count,
			})
		}

		if out.NextToken == nil {
			break
		}
		nextToken = out.NextToken
	}

	return results, nil
}

// ListVersions returns versions of a specific package, newest first, with
// Assets and Published populated. Those two columns are not available from
// ListPackageVersions, so each version costs two extra calls
// (DescribePackageVersion + ListPackageVersionAssets); the lookups are
// fanned out with bounded concurrency.
func (r *Registry) ListVersions(ctx context.Context, coords *PackageCoordinates) ([]VersionSummary, error) {
	var results []VersionSummary
	var nextToken *string

	for {
		out, err := r.client.CodeArtifact.ListPackageVersions(ctx, &codeartifact.ListPackageVersionsInput{
			Domain:     aws.String(coords.Domain),
			Repository: aws.String(coords.Repository),
			Namespace:  aws.String(coords.Namespace),
			Package:    aws.String(coords.Package),
			Format:     FormatGeneric,
			Status:     catypes.PackageVersionStatusPublished,
			SortBy:     "PUBLISHED_TIME",
			NextToken:  nextToken,
		})
		if err != nil {
			if isNotFound(err) {
				return nil, nil
			}
			return nil, fmt.Errorf("listing versions of %s/%s: %w", coords.Namespace, coords.Package, err)
		}

		for _, v := range out.Versions {
			results = append(results, VersionSummary{
				Version: aws.ToString(v.Version),
			})
		}

		if out.NextToken == nil {
			break
		}
		nextToken = out.NextToken
	}

	if err := r.populateVersionMeta(ctx, coords, results); err != nil {
		return nil, err
	}
	return results, nil
}

// populateVersionMeta fills Assets and Published for every version in place,
// using a bounded-concurrency worker fan-out. Returns the first error seen.
func (r *Registry) populateVersionMeta(ctx context.Context, coords *PackageCoordinates, versions []VersionSummary) error {
	sem := make(chan struct{}, versionMetaConcurrency)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error

	for i := range versions {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			assets, published, err := r.versionMeta(ctx, coords, versions[i].Version)
			if err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				mu.Unlock()
				return
			}
			// Distinct index per goroutine: no lock needed for the write.
			versions[i].Assets = assets
			versions[i].Published = published
		}(i)
	}

	wg.Wait()
	return firstErr
}

// versionMeta returns the asset count and publish time for a single version.
func (r *Registry) versionMeta(ctx context.Context, coords *PackageCoordinates, version string) (int, time.Time, error) {
	desc, err := r.client.CodeArtifact.DescribePackageVersion(ctx, &codeartifact.DescribePackageVersionInput{
		Domain:         aws.String(coords.Domain),
		Repository:     aws.String(coords.Repository),
		Namespace:      aws.String(coords.Namespace),
		Package:        aws.String(coords.Package),
		PackageVersion: aws.String(version),
		Format:         FormatGeneric,
	})
	if err != nil {
		return 0, time.Time{}, fmt.Errorf("describing %s/%s@%s: %w", coords.Namespace, coords.Package, version, err)
	}

	var published time.Time
	if desc.PackageVersion != nil && desc.PackageVersion.PublishedTime != nil {
		published = *desc.PackageVersion.PublishedTime
	}

	count := 0
	var nextToken *string
	for {
		out, err := r.client.CodeArtifact.ListPackageVersionAssets(ctx, &codeartifact.ListPackageVersionAssetsInput{
			Domain:         aws.String(coords.Domain),
			Repository:     aws.String(coords.Repository),
			Namespace:      aws.String(coords.Namespace),
			Package:        aws.String(coords.Package),
			PackageVersion: aws.String(version),
			Format:         FormatGeneric,
			NextToken:      nextToken,
		})
		if err != nil {
			return 0, time.Time{}, fmt.Errorf("listing assets for %s/%s@%s: %w", coords.Namespace, coords.Package, version, err)
		}
		count += len(out.Assets)
		if out.NextToken == nil {
			break
		}
		nextToken = out.NextToken
	}

	return count, published, nil
}

// ListAssets returns assets in a specific package version.
func (r *Registry) ListAssets(ctx context.Context, coords *PackageCoordinates) ([]AssetSummary, error) {
	var results []AssetSummary
	var nextToken *string

	for {
		out, err := r.client.CodeArtifact.ListPackageVersionAssets(ctx, &codeartifact.ListPackageVersionAssetsInput{
			Domain:         aws.String(coords.Domain),
			Repository:     aws.String(coords.Repository),
			Namespace:      aws.String(coords.Namespace),
			Package:        aws.String(coords.Package),
			PackageVersion: aws.String(coords.Version),
			Format:         FormatGeneric,
			NextToken:      nextToken,
		})
		if err != nil {
			if isNotFound(err) {
				return nil, nil
			}
			return nil, fmt.Errorf("listing assets for %s/%s@%s: %w", coords.Namespace, coords.Package, coords.Version, err)
		}

		for _, a := range out.Assets {
			hash := ""
			for k, v := range a.Hashes {
				if k == "SHA-256" {
					hash = v
					break
				}
			}
			results = append(results, AssetSummary{
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

	return results, nil
}

// CheckVersionExists returns true if a version exists in the given repo.
func (r *Registry) CheckVersionExists(ctx context.Context, coords *PackageCoordinates) (bool, error) {
	// DescribePackageVersion returns the version regardless of status, so an
	// Unfinished version left behind by a failed publish is reported as
	// existing — which is what --force / conflict handling needs. (The old
	// ListPackageVersions+Status:Published scan was blind to those and would
	// let publish collide with or strand a half-written version.) It is also
	// a single call instead of a full paginated listing.
	_, err := r.client.CodeArtifact.DescribePackageVersion(ctx, &codeartifact.DescribePackageVersionInput{
		Domain:         aws.String(coords.Domain),
		Repository:     aws.String(coords.Repository),
		Namespace:      aws.String(coords.Namespace),
		Package:        aws.String(coords.Package),
		PackageVersion: aws.String(coords.Version),
		Format:         FormatGeneric,
	})
	if err != nil {
		// No such package or version → it doesn't exist.
		if isNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// ResolveLatest returns the most recently published version of a package by
// publication timestamp. Returns ExitNotFound-style error if no versions exist.
func (r *Registry) ResolveLatest(ctx context.Context, coords *PackageCoordinates) (string, error) {
	out, err := r.client.CodeArtifact.ListPackageVersions(ctx, &codeartifact.ListPackageVersionsInput{
		Domain:     aws.String(coords.Domain),
		Repository: aws.String(coords.Repository),
		Namespace:  aws.String(coords.Namespace),
		Package:    aws.String(coords.Package),
		Format:     FormatGeneric,
		Status:     catypes.PackageVersionStatusPublished,
		SortBy:     "PUBLISHED_TIME",
		MaxResults: aws.Int32(1),
	})
	if err != nil {
		if isNotFound(err) {
			return "", fmt.Errorf("no published versions of %s/%s in %s/%s",
				coords.Namespace, coords.Package, coords.Domain, coords.Repository)
		}
		return "", fmt.Errorf("resolving latest version of %s/%s in %s/%s: %w",
			coords.Namespace, coords.Package, coords.Domain, coords.Repository, err)
	}
	if len(out.Versions) == 0 {
		return "", fmt.Errorf("no published versions of %s/%s in %s/%s",
			coords.Namespace, coords.Package, coords.Domain, coords.Repository)
	}
	return aws.ToString(out.Versions[0].Version), nil
}

func (r *Registry) getLatestVersion(ctx context.Context, domain, repo, ns, pkg string) (string, int, error) {
	var allVersions []string
	var nextToken *string

	for {
		out, err := r.client.CodeArtifact.ListPackageVersions(ctx, &codeartifact.ListPackageVersionsInput{
			Domain:     aws.String(domain),
			Repository: aws.String(repo),
			Namespace:  aws.String(ns),
			Package:    aws.String(pkg),
			Format:     FormatGeneric,
			Status:     catypes.PackageVersionStatusPublished,
			SortBy:     "PUBLISHED_TIME",
			NextToken:  nextToken,
		})
		if err != nil {
			if isNotFound(err) {
				return "-", 0, nil
			}
			return "", 0, err
		}
		for _, v := range out.Versions {
			allVersions = append(allVersions, aws.ToString(v.Version))
		}
		if out.NextToken == nil {
			break
		}
		nextToken = out.NextToken
	}

	if len(allVersions) == 0 {
		return "-", 0, nil
	}
	return allVersions[0], len(allVersions), nil
}
