package cob

import (
	"context"
	"errors"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/codeartifact"
	catypes "github.com/aws/aws-sdk-go-v2/service/codeartifact/types"
)

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

// ListVersions returns versions of a specific package.
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

	return results, nil
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
	var nextToken *string

	for {
		out, err := r.client.CodeArtifact.ListPackageVersions(ctx, &codeartifact.ListPackageVersionsInput{
			Domain:     aws.String(coords.Domain),
			Repository: aws.String(coords.Repository),
			Namespace:  aws.String(coords.Namespace),
			Package:    aws.String(coords.Package),
			Format:     FormatGeneric,
			Status:     catypes.PackageVersionStatusPublished,
			NextToken:  nextToken,
		})
		if err != nil {
			// Package doesn't exist yet — that means the version doesn't either.
			if isNotFound(err) {
				return false, nil
			}
			return false, err
		}
		for _, v := range out.Versions {
			if aws.ToString(v.Version) == coords.Version {
				return true, nil
			}
		}
		if out.NextToken == nil {
			break
		}
		nextToken = out.NextToken
	}

	return false, nil
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
