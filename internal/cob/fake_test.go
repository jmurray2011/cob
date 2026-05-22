package cob

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/service/codeartifact"
)

// fakeCA is an in-memory CodeArtifactAPI for unit tests. Only the methods a
// given test exercises need a *Fn set; the rest return empty, no-error
// responses. No AWS calls are ever made.
type fakeCA struct {
	describeFn     func(*codeartifact.DescribePackageVersionInput) (*codeartifact.DescribePackageVersionOutput, error)
	listVersionsFn func(*codeartifact.ListPackageVersionsInput) (*codeartifact.ListPackageVersionsOutput, error)
	listAssetsFn   func(*codeartifact.ListPackageVersionAssetsInput) (*codeartifact.ListPackageVersionAssetsOutput, error)
	deleteFn       func(*codeartifact.DeletePackageVersionsInput) (*codeartifact.DeletePackageVersionsOutput, error)
	publishFn      func(*codeartifact.PublishPackageVersionInput) (*codeartifact.PublishPackageVersionOutput, error)
	getAssetFn     func(*codeartifact.GetPackageVersionAssetInput) (*codeartifact.GetPackageVersionAssetOutput, error)
}

var _ CodeArtifactAPI = (*fakeCA)(nil)

func (f *fakeCA) DescribePackageVersion(_ context.Context, in *codeartifact.DescribePackageVersionInput, _ ...func(*codeartifact.Options)) (*codeartifact.DescribePackageVersionOutput, error) {
	if f.describeFn != nil {
		return f.describeFn(in)
	}
	return &codeartifact.DescribePackageVersionOutput{}, nil
}

func (f *fakeCA) ListPackageVersions(_ context.Context, in *codeartifact.ListPackageVersionsInput, _ ...func(*codeartifact.Options)) (*codeartifact.ListPackageVersionsOutput, error) {
	if f.listVersionsFn != nil {
		return f.listVersionsFn(in)
	}
	return &codeartifact.ListPackageVersionsOutput{}, nil
}

func (f *fakeCA) ListPackageVersionAssets(_ context.Context, in *codeartifact.ListPackageVersionAssetsInput, _ ...func(*codeartifact.Options)) (*codeartifact.ListPackageVersionAssetsOutput, error) {
	if f.listAssetsFn != nil {
		return f.listAssetsFn(in)
	}
	return &codeartifact.ListPackageVersionAssetsOutput{}, nil
}

func (f *fakeCA) DeletePackageVersions(_ context.Context, in *codeartifact.DeletePackageVersionsInput, _ ...func(*codeartifact.Options)) (*codeartifact.DeletePackageVersionsOutput, error) {
	if f.deleteFn != nil {
		return f.deleteFn(in)
	}
	return &codeartifact.DeletePackageVersionsOutput{}, nil
}

func (f *fakeCA) PublishPackageVersion(_ context.Context, in *codeartifact.PublishPackageVersionInput, _ ...func(*codeartifact.Options)) (*codeartifact.PublishPackageVersionOutput, error) {
	if f.publishFn != nil {
		return f.publishFn(in)
	}
	return &codeartifact.PublishPackageVersionOutput{}, nil
}

func (f *fakeCA) GetPackageVersionAsset(_ context.Context, in *codeartifact.GetPackageVersionAssetInput, _ ...func(*codeartifact.Options)) (*codeartifact.GetPackageVersionAssetOutput, error) {
	if f.getAssetFn != nil {
		return f.getAssetFn(in)
	}
	return &codeartifact.GetPackageVersionAssetOutput{}, nil
}

func (f *fakeCA) ListDomains(_ context.Context, _ *codeartifact.ListDomainsInput, _ ...func(*codeartifact.Options)) (*codeartifact.ListDomainsOutput, error) {
	return &codeartifact.ListDomainsOutput{}, nil
}

func (f *fakeCA) ListRepositoriesInDomain(_ context.Context, _ *codeartifact.ListRepositoriesInDomainInput, _ ...func(*codeartifact.Options)) (*codeartifact.ListRepositoriesInDomainOutput, error) {
	return &codeartifact.ListRepositoriesInDomainOutput{}, nil
}

func (f *fakeCA) ListPackages(_ context.Context, _ *codeartifact.ListPackagesInput, _ ...func(*codeartifact.Options)) (*codeartifact.ListPackagesOutput, error) {
	return &codeartifact.ListPackagesOutput{}, nil
}

func newTestClient(ca CodeArtifactAPI) *Client {
	return &Client{CodeArtifact: ca}
}
