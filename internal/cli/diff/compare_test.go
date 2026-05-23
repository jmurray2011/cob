package diff

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/codeartifact"
	catypes "github.com/aws/aws-sdk-go-v2/service/codeartifact/types"
	"github.com/jmurray2011/cob/internal/cob"

	"github.com/jmurray2011/cob/internal/cliutil"
)

// caStub implements cob.CodeArtifactAPI; only ListPackageVersionAssets matters
// here (registry.ListAssets). The rest satisfy the interface.
type caStub struct{ assets []catypes.AssetSummary }

func (c *caStub) ListPackageVersionAssets(context.Context, *codeartifact.ListPackageVersionAssetsInput, ...func(*codeartifact.Options)) (*codeartifact.ListPackageVersionAssetsOutput, error) {
	return &codeartifact.ListPackageVersionAssetsOutput{Assets: c.assets}, nil
}
func (c *caStub) ListDomains(context.Context, *codeartifact.ListDomainsInput, ...func(*codeartifact.Options)) (*codeartifact.ListDomainsOutput, error) {
	return &codeartifact.ListDomainsOutput{}, nil
}
func (c *caStub) ListRepositoriesInDomain(context.Context, *codeartifact.ListRepositoriesInDomainInput, ...func(*codeartifact.Options)) (*codeartifact.ListRepositoriesInDomainOutput, error) {
	return &codeartifact.ListRepositoriesInDomainOutput{}, nil
}
func (c *caStub) ListPackages(context.Context, *codeartifact.ListPackagesInput, ...func(*codeartifact.Options)) (*codeartifact.ListPackagesOutput, error) {
	return &codeartifact.ListPackagesOutput{}, nil
}
func (c *caStub) ListPackageVersions(context.Context, *codeartifact.ListPackageVersionsInput, ...func(*codeartifact.Options)) (*codeartifact.ListPackageVersionsOutput, error) {
	return &codeartifact.ListPackageVersionsOutput{}, nil
}
func (c *caStub) DescribePackageVersion(context.Context, *codeartifact.DescribePackageVersionInput, ...func(*codeartifact.Options)) (*codeartifact.DescribePackageVersionOutput, error) {
	return &codeartifact.DescribePackageVersionOutput{}, nil
}
func (c *caStub) GetPackageVersionAsset(context.Context, *codeartifact.GetPackageVersionAssetInput, ...func(*codeartifact.Options)) (*codeartifact.GetPackageVersionAssetOutput, error) {
	return &codeartifact.GetPackageVersionAssetOutput{}, nil
}
func (c *caStub) PublishPackageVersion(context.Context, *codeartifact.PublishPackageVersionInput, ...func(*codeartifact.Options)) (*codeartifact.PublishPackageVersionOutput, error) {
	return &codeartifact.PublishPackageVersionOutput{}, nil
}
func (c *caStub) DeletePackageVersions(context.Context, *codeartifact.DeletePackageVersionsInput, ...func(*codeartifact.Options)) (*codeartifact.DeletePackageVersionsOutput, error) {
	return &codeartifact.DeletePackageVersionsOutput{}, nil
}

type stubSrc struct {
	uri, name, sha, body string
	origin               *cob.Origin
	originErr            error
}

func (s stubSrc) URI() string      { return s.uri }
func (s stubSrc) Filename() string { return s.name }
func (s stubSrc) Resolve(context.Context) (*cob.AssetMetadata, error) {
	return &cob.AssetMetadata{Size: int64(len(s.body)), SHA256: s.sha}, nil
}
func (s stubSrc) Open(context.Context) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader(s.body)), nil
}
func (s stubSrc) Origin(context.Context) (*cob.Origin, error) { return s.origin, s.originErr }

func pubAsset(name, sha string) catypes.AssetSummary {
	return catypes.AssetSummary{Name: aws.String(name), Hashes: map[string]string{"SHA-256": sha}}
}

func TestCompareSHAPrecedence(t *testing.T) {
	ctx := context.Background()
	body := "the-bytes"
	bodySHA := func() string { s := sha256.Sum256([]byte(body)); return hex.EncodeToString(s[:]) }()

	ca := &caStub{assets: []catypes.AssetSummary{
		pubAsset("known.bin", "KNOWNSHA"),
		pubAsset("deep.bin", bodySHA),
		pubAsset("prov.bin", "PROVSHA"),
		pubAsset("none.bin", "ZZZ"),
		pubAsset(cob.ProvenanceFile, "whatever"), // must be ignored, not "extra"
		pubAsset("orphan.bin", "ORPHAN"),         // published, not in manifest
	}}
	reg := cob.NewRegistry(&cob.Client{CodeArtifact: ca})
	coords := &cob.PackageCoordinates{Domain: "d", Repository: "r", Namespace: "n", Package: "p", Version: "1"}

	sources := []cliutil.NamedSource{
		{Name: "k", Source: stubSrc{uri: "s3://b/known.bin", name: "known.bin", sha: "KNOWNSHA"}},
		{Name: "d", Source: stubSrc{uri: "s3://b/deep.bin", name: "deep.bin", body: body}},
		{Name: "pv", Source: stubSrc{uri: "s3://b/prov.bin", name: "prov.bin"}},
		{Name: "nn", Source: stubSrc{uri: "s3://b/none.bin", name: "none.bin"}},
	}
	prov := &cob.Provenance{Assets: []cob.ProvenanceEntry{{Asset: "prov.bin", SHA256: "PROVSHA"}}}

	t.Run("deep=false uses provenance fallback, ignores provenance asset", func(t *testing.T) {
		cmps, err := compareManifestToPublished(ctx, sources, reg, coords, false, prov)
		if err != nil {
			t.Fatal(err)
		}
		by := map[string]assetCompare{}
		for _, c := range cmps {
			by[c.Name] = c
		}
		if c := by["known.bin"]; c.SrcFrom != "source" || c.SrcSHA != "KNOWNSHA" {
			t.Errorf("known: %+v", c)
		}
		if c := by["prov.bin"]; c.SrcFrom != "provenance" || c.SrcSHA != "PROVSHA" {
			t.Errorf("prov: %+v want provenance/PROVSHA", c)
		}
		if c := by["none.bin"]; c.SrcFrom != "" || c.SrcSHA != "" {
			t.Errorf("none: %+v want unknown", c)
		}
		if c := by["deep.bin"]; c.SrcSHA != "" {
			t.Errorf("deep.bin without --deep must stay unknown, got %+v", c)
		}
		if _, ok := by[cob.ProvenanceFile]; ok {
			t.Error("cob-provenance.json must not appear as a compared/extra asset")
		}
		if c, ok := by["orphan.bin"]; !ok || c.InManifest || !c.InPublished {
			t.Errorf("orphan.bin should be published-only, got %+v ok=%v", c, ok)
		}
	})

	t.Run("deep=true downloads+hashes the unchecksummed source", func(t *testing.T) {
		cmps, err := compareManifestToPublished(ctx, sources, reg, coords, true, nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range cmps {
			if c.Name == "deep.bin" {
				if c.SrcFrom != "deep" || c.SrcSHA != bodySHA {
					t.Fatalf("deep.bin: got %+v, want deep/%s", c, bodySHA)
				}
				if c.SrcSHA != c.PubSHA {
					t.Fatalf("deep hash should match published: %s vs %s", c.SrcSHA, c.PubSHA)
				}
				return
			}
		}
		t.Fatal("deep.bin not found in comparison")
	})
}

func TestCompareOriginDrift(t *testing.T) {
	ctx := context.Background()
	ca := &caStub{assets: []catypes.AssetSummary{
		pubAsset("drift.bin", "PSHA"),
		pubAsset("stable.bin", "PSHA2"),
	}}
	reg := cob.NewRegistry(&cob.Client{CodeArtifact: ca})
	coords := &cob.PackageCoordinates{Domain: "d", Repository: "r", Namespace: "n", Package: "p", Version: "1"}

	prov := &cob.Provenance{Assets: []cob.ProvenanceEntry{
		{Asset: "drift.bin", SHA256: "PSHA", Origin: &cob.Origin{Type: "s3", ETag: "old"}},
		{Asset: "stable.bin", SHA256: "PSHA2", Origin: &cob.Origin{Type: "s3", ETag: "same"}},
	}}
	sources := []cliutil.NamedSource{
		// no content checksum (sha:"") -> falls to recorded-origin drift check
		{Name: "d", Source: stubSrc{name: "drift.bin", origin: &cob.Origin{Type: "s3", ETag: "NEW"}}},
		{Name: "s", Source: stubSrc{name: "stable.bin", origin: &cob.Origin{Type: "s3", ETag: "same"}}},
	}

	cmps, err := compareManifestToPublished(ctx, sources, reg, coords, false, prov)
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]assetCompare{}
	for _, c := range cmps {
		by[c.Name] = c
	}
	if d := by["drift.bin"]; !d.OriginDrift || d.SrcFrom != "origin" {
		t.Errorf("drift.bin: want OriginDrift+origin, got %+v", d)
	}
	if s := by["stable.bin"]; s.OriginDrift || s.SrcFrom != "origin" || s.SrcSHA != "PSHA2" || s.SrcSHA != s.PubSHA {
		t.Errorf("stable.bin: want origin-confirmed match, got %+v", s)
	}
}

func TestCompareOriginUnreadableIsDrift(t *testing.T) {
	ctx := context.Background()
	ca := &caStub{assets: []catypes.AssetSummary{pubAsset("x.bin", "PSHA")}}
	reg := cob.NewRegistry(&cob.Client{CodeArtifact: ca})
	coords := &cob.PackageCoordinates{Domain: "d", Repository: "r", Namespace: "n", Package: "p", Version: "1"}
	prov := &cob.Provenance{Assets: []cob.ProvenanceEntry{
		{Asset: "x.bin", SHA256: "PSHA", Origin: &cob.Origin{Type: "s3", ETag: "old"}},
	}}
	sources := []cliutil.NamedSource{
		{Name: "x", Source: stubSrc{name: "x.bin", originErr: errors.New("NoSuchKey")}},
	}
	cmps, _ := compareManifestToPublished(ctx, sources, reg, coords, false, prov)
	if !cmps[0].OriginDrift {
		t.Fatalf("unreadable origin must be treated as drift, got %+v", cmps[0])
	}
}
