package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/codeartifact"
	catypes "github.com/aws/aws-sdk-go-v2/service/codeartifact/types"

	"github.com/jmurray2011/cob/internal/cob"
	"github.com/jmurray2011/cob/internal/output"
)

// fakeCA is an in-memory cob.CodeArtifactAPI for cli-level tests. Only the
// methods a test exercises need a *Fn hook; the rest return empty, no-error
// responses. (internal/cob has its own equivalent — Go test fakes do not cross
// package boundaries, so each package carries one.)
type fakeCA struct {
	describeFn     func(*codeartifact.DescribePackageVersionInput) (*codeartifact.DescribePackageVersionOutput, error)
	listVersionsFn func(*codeartifact.ListPackageVersionsInput) (*codeartifact.ListPackageVersionsOutput, error)
	listAssetsFn   func(*codeartifact.ListPackageVersionAssetsInput) (*codeartifact.ListPackageVersionAssetsOutput, error)
	deleteFn       func(*codeartifact.DeletePackageVersionsInput) (*codeartifact.DeletePackageVersionsOutput, error)
	publishFn      func(*codeartifact.PublishPackageVersionInput) (*codeartifact.PublishPackageVersionOutput, error)
	getAssetFn     func(*codeartifact.GetPackageVersionAssetInput) (*codeartifact.GetPackageVersionAssetOutput, error)
	listDomainsFn  func(*codeartifact.ListDomainsInput) (*codeartifact.ListDomainsOutput, error)
	listReposFn    func(*codeartifact.ListRepositoriesInDomainInput) (*codeartifact.ListRepositoriesInDomainOutput, error)
	listPackagesFn func(*codeartifact.ListPackagesInput) (*codeartifact.ListPackagesOutput, error)
}

var _ cob.CodeArtifactAPI = (*fakeCA)(nil)

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
	// Default to "asset doesn't exist": an empty output has a nil Asset
	// reader, which would panic in any caller that streamed it.
	return nil, &catypes.ResourceNotFoundException{}
}

func (f *fakeCA) ListDomains(_ context.Context, in *codeartifact.ListDomainsInput, _ ...func(*codeartifact.Options)) (*codeartifact.ListDomainsOutput, error) {
	if f.listDomainsFn != nil {
		return f.listDomainsFn(in)
	}
	return &codeartifact.ListDomainsOutput{}, nil
}

func (f *fakeCA) ListRepositoriesInDomain(_ context.Context, in *codeartifact.ListRepositoriesInDomainInput, _ ...func(*codeartifact.Options)) (*codeartifact.ListRepositoriesInDomainOutput, error) {
	if f.listReposFn != nil {
		return f.listReposFn(in)
	}
	return &codeartifact.ListRepositoriesInDomainOutput{}, nil
}

func (f *fakeCA) ListPackages(_ context.Context, in *codeartifact.ListPackagesInput, _ ...func(*codeartifact.Options)) (*codeartifact.ListPackagesOutput, error) {
	if f.listPackagesFn != nil {
		return f.listPackagesFn(in)
	}
	return &codeartifact.ListPackagesOutput{}, nil
}

// useFake installs an in-memory CodeArtifact client and buffer-backed output
// for one test, restoring every package-level seam and persistent flag on
// cleanup. Saving each persistent flag is deliberate — a partial save/restore
// would let a test that flips e.g. flagQuiet leak that state into the next.
// The returned buffers receive everything the command would have printed.
func useFake(t *testing.T, ca cob.CodeArtifactAPI) (stdout, stderr *bytes.Buffer) {
	t.Helper()
	stdout, stderr = &bytes.Buffer{}, &bytes.Buffer{}

	origClient, origWriter := newClient, newWriter
	origJSON, origQuiet, origDebug := flagJSON, flagQuiet, flagDebug
	origProfile, origRegion, origTmpDir := flagProfile, flagRegion, flagTmpDir

	newClient = func(context.Context, cob.ClientOptions) (*cob.Client, error) {
		return &cob.Client{CodeArtifact: ca, Region: "us-east-2"}, nil
	}
	newWriter = func(j bool) *output.Writer {
		return output.NewWithWriters(stdout, stderr, j)
	}
	flagJSON, flagQuiet, flagDebug = false, false, false
	flagProfile, flagRegion, flagTmpDir = "", "", ""

	t.Cleanup(func() {
		newClient, newWriter = origClient, origWriter
		flagJSON, flagQuiet, flagDebug = origJSON, origQuiet, origDebug
		flagProfile, flagRegion, flagTmpDir = origProfile, origRegion, origTmpDir
	})
	return stdout, stderr
}

// wantExit asserts err is an *ExitError carrying the given code.
func wantExit(t *testing.T, err error, code int) {
	t.Helper()
	var ee *ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("expected *ExitError(%d), got %v (%T)", code, err, err)
	}
	if ee.Code != code {
		t.Fatalf("exit code = %d, want %d", ee.Code, code)
	}
}

// writeFile creates a file under dir and returns its path.
func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}
