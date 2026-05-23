package clitest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/codeartifact"
	catypes "github.com/aws/aws-sdk-go-v2/service/codeartifact/types"

	"github.com/jmurray2011/cob/internal/cob"
)

// StatefulCA is an in-memory CodeArtifact fake that actually stores
// bytes — published assets stick around, pull/diff can read them back,
// promote can copy between repos within the same domain. The dispatch
// FakeCA in this package is a quick "stub one method per test" helper;
// StatefulCA is the heavier "wire up multiple commands and watch the
// whole lifecycle work" helper used by end-to-end tests.
//
// Concurrency: every public method takes the mutex; cob's publish path
// fans out to defaultConcurrency goroutines, all racing to write into
// the same version. The lock is held only for the in-memory mutation,
// never around I/O.
type StatefulCA struct {
	mu       sync.Mutex
	versions map[string]*statefulVersion // key: "domain/repo/ns/pkg@version"
}

type statefulVersion struct {
	Status    catypes.PackageVersionStatus
	Published time.Time
	Assets    map[string]statefulAsset // key: asset name
}

type statefulAsset struct {
	Bytes  []byte
	SHA256 string
}

// NewStatefulCA returns an empty in-memory CA. Pre-seed via Seed if a
// test needs an existing version (e.g. to exercise a promote source).
func NewStatefulCA() *StatefulCA {
	return &StatefulCA{versions: make(map[string]*statefulVersion)}
}

var _ cob.CodeArtifactAPI = (*StatefulCA)(nil)

func versionKey(domain, repo, ns, pkg, ver string) string {
	return fmt.Sprintf("%s/%s/%s/%s@%s", domain, repo, ns, pkg, ver)
}

// Seed inserts a version with assets at the given coords, status
// Published. Useful for tests that need a pre-existing version to act
// on (e.g. an upstream a ca:// source resolves into).
func (s *StatefulCA) Seed(domain, repo, ns, pkg, ver string, assets map[string][]byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := &statefulVersion{
		Status:    catypes.PackageVersionStatusPublished,
		Published: time.Now(),
		Assets:    make(map[string]statefulAsset, len(assets)),
	}
	for name, b := range assets {
		sum := sha256.Sum256(b)
		v.Assets[name] = statefulAsset{Bytes: b, SHA256: hex.EncodeToString(sum[:])}
	}
	s.versions[versionKey(domain, repo, ns, pkg, ver)] = v
}

// AssetBytes returns the recorded bytes for an asset (or nil if absent).
// Lets a test assert "the bytes that landed in CodeArtifact match what
// the manifest source produced" without going through the public Get
// path.
func (s *StatefulCA) AssetBytes(domain, repo, ns, pkg, ver, name string) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.versions[versionKey(domain, repo, ns, pkg, ver)]
	if !ok {
		return nil
	}
	a, ok := v.Assets[name]
	if !ok {
		return nil
	}
	return a.Bytes
}

func (s *StatefulCA) DescribePackageVersion(_ context.Context, in *codeartifact.DescribePackageVersionInput, _ ...func(*codeartifact.Options)) (*codeartifact.DescribePackageVersionOutput, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := versionKey(aws.ToString(in.Domain), aws.ToString(in.Repository), aws.ToString(in.Namespace), aws.ToString(in.Package), aws.ToString(in.PackageVersion))
	v, ok := s.versions[key]
	if !ok {
		return nil, &catypes.ResourceNotFoundException{}
	}
	pt := v.Published
	return &codeartifact.DescribePackageVersionOutput{
		PackageVersion: &catypes.PackageVersionDescription{
			Status:        v.Status,
			PublishedTime: &pt,
		},
	}, nil
}

func (s *StatefulCA) ListPackageVersions(_ context.Context, in *codeartifact.ListPackageVersionsInput, _ ...func(*codeartifact.Options)) (*codeartifact.ListPackageVersionsOutput, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	prefix := fmt.Sprintf("%s/%s/%s/%s@", aws.ToString(in.Domain), aws.ToString(in.Repository), aws.ToString(in.Namespace), aws.ToString(in.Package))
	var vs []catypes.PackageVersionSummary
	for key, v := range s.versions {
		if len(key) <= len(prefix) || key[:len(prefix)] != prefix {
			continue
		}
		if in.Status != "" && v.Status != in.Status {
			continue
		}
		ver := key[len(prefix):]
		vs = append(vs, catypes.PackageVersionSummary{
			Version: aws.String(ver),
			Status:  v.Status,
		})
	}
	return &codeartifact.ListPackageVersionsOutput{Versions: vs}, nil
}

func (s *StatefulCA) ListPackageVersionAssets(_ context.Context, in *codeartifact.ListPackageVersionAssetsInput, _ ...func(*codeartifact.Options)) (*codeartifact.ListPackageVersionAssetsOutput, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := versionKey(aws.ToString(in.Domain), aws.ToString(in.Repository), aws.ToString(in.Namespace), aws.ToString(in.Package), aws.ToString(in.PackageVersion))
	v, ok := s.versions[key]
	if !ok {
		return nil, &catypes.ResourceNotFoundException{}
	}
	var out []catypes.AssetSummary
	for name, a := range v.Assets {
		size := int64(len(a.Bytes))
		out = append(out, catypes.AssetSummary{
			Name:   aws.String(name),
			Size:   aws.Int64(size),
			Hashes: map[string]string{"SHA-256": a.SHA256},
		})
	}
	return &codeartifact.ListPackageVersionAssetsOutput{Assets: out}, nil
}

func (s *StatefulCA) DeletePackageVersions(_ context.Context, in *codeartifact.DeletePackageVersionsInput, _ ...func(*codeartifact.Options)) (*codeartifact.DeletePackageVersionsOutput, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, ver := range in.Versions {
		key := versionKey(aws.ToString(in.Domain), aws.ToString(in.Repository), aws.ToString(in.Namespace), aws.ToString(in.Package), ver)
		delete(s.versions, key)
	}
	return &codeartifact.DeletePackageVersionsOutput{}, nil
}

func (s *StatefulCA) PublishPackageVersion(_ context.Context, in *codeartifact.PublishPackageVersionInput, _ ...func(*codeartifact.Options)) (*codeartifact.PublishPackageVersionOutput, error) {
	// Slurp the content outside the lock — io.Copy on the SDK's seeker can
	// be many MB; holding the mutex would serialize what cob explicitly
	// fans out in parallel.
	body, err := io.ReadAll(in.AssetContent)
	if err != nil {
		return nil, fmt.Errorf("StatefulCA: read AssetContent: %w", err)
	}
	sum := sha256.Sum256(body)
	gotSHA := hex.EncodeToString(sum[:])
	if expected := aws.ToString(in.AssetSHA256); expected != "" && expected != gotSHA {
		return nil, fmt.Errorf("StatefulCA: AssetSHA256 mismatch — declared %s, computed %s", expected, gotSHA)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	key := versionKey(aws.ToString(in.Domain), aws.ToString(in.Repository), aws.ToString(in.Namespace), aws.ToString(in.Package), aws.ToString(in.PackageVersion))
	v, ok := s.versions[key]
	if !ok {
		v = &statefulVersion{
			Status:    catypes.PackageVersionStatusUnfinished,
			Published: time.Now(),
			Assets:    make(map[string]statefulAsset),
		}
		s.versions[key] = v
	}
	v.Assets[aws.ToString(in.AssetName)] = statefulAsset{Bytes: body, SHA256: gotSHA}
	// Unfinished=false (or unset on the finalizing call) flips the
	// version to Published — mirrors CodeArtifact's behavior.
	if in.Unfinished == nil || !*in.Unfinished {
		v.Status = catypes.PackageVersionStatusPublished
	}
	return &codeartifact.PublishPackageVersionOutput{}, nil
}

func (s *StatefulCA) GetPackageVersionAsset(_ context.Context, in *codeartifact.GetPackageVersionAssetInput, _ ...func(*codeartifact.Options)) (*codeartifact.GetPackageVersionAssetOutput, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := versionKey(aws.ToString(in.Domain), aws.ToString(in.Repository), aws.ToString(in.Namespace), aws.ToString(in.Package), aws.ToString(in.PackageVersion))
	v, ok := s.versions[key]
	if !ok {
		return nil, &catypes.ResourceNotFoundException{}
	}
	a, ok := v.Assets[aws.ToString(in.Asset)]
	if !ok {
		return nil, &catypes.ResourceNotFoundException{}
	}
	return &codeartifact.GetPackageVersionAssetOutput{
		Asset: io.NopCloser(bytes.NewReader(a.Bytes)),
	}, nil
}

func (s *StatefulCA) ListDomains(_ context.Context, _ *codeartifact.ListDomainsInput, _ ...func(*codeartifact.Options)) (*codeartifact.ListDomainsOutput, error) {
	return &codeartifact.ListDomainsOutput{}, nil
}

func (s *StatefulCA) ListRepositoriesInDomain(_ context.Context, _ *codeartifact.ListRepositoriesInDomainInput, _ ...func(*codeartifact.Options)) (*codeartifact.ListRepositoriesInDomainOutput, error) {
	return &codeartifact.ListRepositoriesInDomainOutput{}, nil
}

func (s *StatefulCA) ListPackages(_ context.Context, _ *codeartifact.ListPackagesInput, _ ...func(*codeartifact.Options)) (*codeartifact.ListPackagesOutput, error) {
	return &codeartifact.ListPackagesOutput{}, nil
}
