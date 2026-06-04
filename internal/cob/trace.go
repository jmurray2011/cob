package cob

import (
	"context"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/codeartifact"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// TraceFunc receives one entry per AWS API call cob issues: the operation
// name and a short human detail (the coordinates / object it targets). It
// is wired to --verbose, which renders each as a "verbose: aws ..." line on
// stderr — a coarse call trace without the AWS SDK's full request/response
// dump (that's --debug). A nil TraceFunc disables tracing entirely.
type TraceFunc func(op, detail string)

// pkgPath renders the populated coordinate prefix of an API input as
// domain/repo/namespace/package@version, dropping segments that are empty
// (a ListPackages call has no version; ListDomains has nothing).
func pkgPath(domain, repo, ns, pkg, version string) string {
	var b strings.Builder
	b.WriteString(domain)
	for _, seg := range []string{repo, ns, pkg} {
		if seg != "" {
			b.WriteByte('/')
			b.WriteString(seg)
		}
	}
	if version != "" {
		b.WriteByte('@')
		b.WriteString(version)
	}
	return b.String()
}

// tracedCA wraps a CodeArtifactAPI, emitting one trace entry per call before
// delegating. Installed by NewClient when ClientOptions.Trace is set.
type tracedCA struct {
	inner CodeArtifactAPI
	trace TraceFunc
}

var _ CodeArtifactAPI = tracedCA{}

func (t tracedCA) ListDomains(ctx context.Context, in *codeartifact.ListDomainsInput, o ...func(*codeartifact.Options)) (*codeartifact.ListDomainsOutput, error) {
	t.trace("ListDomains", "")
	return t.inner.ListDomains(ctx, in, o...)
}

func (t tracedCA) ListRepositoriesInDomain(ctx context.Context, in *codeartifact.ListRepositoriesInDomainInput, o ...func(*codeartifact.Options)) (*codeartifact.ListRepositoriesInDomainOutput, error) {
	t.trace("ListRepositoriesInDomain", pkgPath(aws.ToString(in.Domain), "", "", "", ""))
	return t.inner.ListRepositoriesInDomain(ctx, in, o...)
}

func (t tracedCA) ListPackages(ctx context.Context, in *codeartifact.ListPackagesInput, o ...func(*codeartifact.Options)) (*codeartifact.ListPackagesOutput, error) {
	t.trace("ListPackages", pkgPath(aws.ToString(in.Domain), aws.ToString(in.Repository), "", "", ""))
	return t.inner.ListPackages(ctx, in, o...)
}

func (t tracedCA) ListPackageVersions(ctx context.Context, in *codeartifact.ListPackageVersionsInput, o ...func(*codeartifact.Options)) (*codeartifact.ListPackageVersionsOutput, error) {
	t.trace("ListPackageVersions", pkgPath(aws.ToString(in.Domain), aws.ToString(in.Repository), aws.ToString(in.Namespace), aws.ToString(in.Package), ""))
	return t.inner.ListPackageVersions(ctx, in, o...)
}

func (t tracedCA) ListPackageVersionAssets(ctx context.Context, in *codeartifact.ListPackageVersionAssetsInput, o ...func(*codeartifact.Options)) (*codeartifact.ListPackageVersionAssetsOutput, error) {
	t.trace("ListPackageVersionAssets", pkgPath(aws.ToString(in.Domain), aws.ToString(in.Repository), aws.ToString(in.Namespace), aws.ToString(in.Package), aws.ToString(in.PackageVersion)))
	return t.inner.ListPackageVersionAssets(ctx, in, o...)
}

func (t tracedCA) DescribePackageVersion(ctx context.Context, in *codeartifact.DescribePackageVersionInput, o ...func(*codeartifact.Options)) (*codeartifact.DescribePackageVersionOutput, error) {
	t.trace("DescribePackageVersion", pkgPath(aws.ToString(in.Domain), aws.ToString(in.Repository), aws.ToString(in.Namespace), aws.ToString(in.Package), aws.ToString(in.PackageVersion)))
	return t.inner.DescribePackageVersion(ctx, in, o...)
}

func (t tracedCA) GetPackageVersionAsset(ctx context.Context, in *codeartifact.GetPackageVersionAssetInput, o ...func(*codeartifact.Options)) (*codeartifact.GetPackageVersionAssetOutput, error) {
	t.trace("GetPackageVersionAsset", pkgPath(aws.ToString(in.Domain), aws.ToString(in.Repository), aws.ToString(in.Namespace), aws.ToString(in.Package), aws.ToString(in.PackageVersion))+" "+aws.ToString(in.Asset))
	return t.inner.GetPackageVersionAsset(ctx, in, o...)
}

func (t tracedCA) PublishPackageVersion(ctx context.Context, in *codeartifact.PublishPackageVersionInput, o ...func(*codeartifact.Options)) (*codeartifact.PublishPackageVersionOutput, error) {
	t.trace("PublishPackageVersion", pkgPath(aws.ToString(in.Domain), aws.ToString(in.Repository), aws.ToString(in.Namespace), aws.ToString(in.Package), aws.ToString(in.PackageVersion))+" "+aws.ToString(in.AssetName))
	return t.inner.PublishPackageVersion(ctx, in, o...)
}

func (t tracedCA) DeletePackageVersions(ctx context.Context, in *codeartifact.DeletePackageVersionsInput, o ...func(*codeartifact.Options)) (*codeartifact.DeletePackageVersionsOutput, error) {
	t.trace("DeletePackageVersions", pkgPath(aws.ToString(in.Domain), aws.ToString(in.Repository), aws.ToString(in.Namespace), aws.ToString(in.Package), "")+" versions="+strings.Join(in.Versions, ","))
	return t.inner.DeletePackageVersions(ctx, in, o...)
}

// tracedS3 wraps an S3API the same way. Options() is a local config read,
// not a network call, so it passes through untraced.
type tracedS3 struct {
	inner S3API
	trace TraceFunc
}

var _ S3API = tracedS3{}

func (t tracedS3) HeadObject(ctx context.Context, in *s3.HeadObjectInput, o ...func(*s3.Options)) (*s3.HeadObjectOutput, error) {
	t.trace("s3:HeadObject", "s3://"+aws.ToString(in.Bucket)+"/"+aws.ToString(in.Key))
	return t.inner.HeadObject(ctx, in, o...)
}

func (t tracedS3) GetObject(ctx context.Context, in *s3.GetObjectInput, o ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	t.trace("s3:GetObject", "s3://"+aws.ToString(in.Bucket)+"/"+aws.ToString(in.Key))
	return t.inner.GetObject(ctx, in, o...)
}

func (t tracedS3) Options() s3.Options { return t.inner.Options() }
