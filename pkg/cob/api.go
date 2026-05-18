package cob

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/service/codeartifact"
)

// CodeArtifactAPI is the subset of *codeartifact.Client that cob uses.
// Depending on this interface instead of the concrete client lets the
// registry/publisher/puller/promoter and the ca:// source be unit-tested
// with an in-memory fake — no AWS calls. The concrete SDK client satisfies
// it (see the compile-time assertion below).
type CodeArtifactAPI interface {
	ListDomains(context.Context, *codeartifact.ListDomainsInput, ...func(*codeartifact.Options)) (*codeartifact.ListDomainsOutput, error)
	ListRepositoriesInDomain(context.Context, *codeartifact.ListRepositoriesInDomainInput, ...func(*codeartifact.Options)) (*codeartifact.ListRepositoriesInDomainOutput, error)
	ListPackages(context.Context, *codeartifact.ListPackagesInput, ...func(*codeartifact.Options)) (*codeartifact.ListPackagesOutput, error)
	ListPackageVersions(context.Context, *codeartifact.ListPackageVersionsInput, ...func(*codeartifact.Options)) (*codeartifact.ListPackageVersionsOutput, error)
	ListPackageVersionAssets(context.Context, *codeartifact.ListPackageVersionAssetsInput, ...func(*codeartifact.Options)) (*codeartifact.ListPackageVersionAssetsOutput, error)
	DescribePackageVersion(context.Context, *codeartifact.DescribePackageVersionInput, ...func(*codeartifact.Options)) (*codeartifact.DescribePackageVersionOutput, error)
	GetPackageVersionAsset(context.Context, *codeartifact.GetPackageVersionAssetInput, ...func(*codeartifact.Options)) (*codeartifact.GetPackageVersionAssetOutput, error)
	PublishPackageVersion(context.Context, *codeartifact.PublishPackageVersionInput, ...func(*codeartifact.Options)) (*codeartifact.PublishPackageVersionOutput, error)
	DeletePackageVersions(context.Context, *codeartifact.DeletePackageVersionsInput, ...func(*codeartifact.Options)) (*codeartifact.DeletePackageVersionsOutput, error)
}

// Compile-time guarantee that the real SDK client implements the interface.
var _ CodeArtifactAPI = (*codeartifact.Client)(nil)
