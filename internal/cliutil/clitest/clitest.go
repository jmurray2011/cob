// Package clitest provides the per-test scaffolding every cob command
// test needs: an in-memory CodeArtifactAPI fake, a one-line seam-swap
// (UseFake) that wires the fake into cliutil's NewClient / NewWriter
// package vars and restores them on test cleanup, and tiny assertion
// helpers (WantExit, WriteFile). Lives under cliutil so any per-command
// test package (internal/cli/diff, /publish, /promote, /pull, …) can
// import it without duplicating the boilerplate.
package clitest

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/codeartifact"
	catypes "github.com/aws/aws-sdk-go-v2/service/codeartifact/types"

	"github.com/jmurray2011/cob/internal/cliutil"
	"github.com/jmurray2011/cob/internal/cob"
	"github.com/jmurray2011/cob/internal/output"
)

// FakeCA is an in-memory cob.CodeArtifactAPI for cli-level tests. Only
// the methods a test exercises need a *Fn hook; the rest return empty,
// no-error responses. (internal/cob has its own equivalent — Go test
// fakes don't cross package boundaries, so each package carries one.)
type FakeCA struct {
	DescribeFn     func(*codeartifact.DescribePackageVersionInput) (*codeartifact.DescribePackageVersionOutput, error)
	ListVersionsFn func(*codeartifact.ListPackageVersionsInput) (*codeartifact.ListPackageVersionsOutput, error)
	ListAssetsFn   func(*codeartifact.ListPackageVersionAssetsInput) (*codeartifact.ListPackageVersionAssetsOutput, error)
	DeleteFn       func(*codeartifact.DeletePackageVersionsInput) (*codeartifact.DeletePackageVersionsOutput, error)
	PublishFn      func(*codeartifact.PublishPackageVersionInput) (*codeartifact.PublishPackageVersionOutput, error)
	GetAssetFn     func(*codeartifact.GetPackageVersionAssetInput) (*codeartifact.GetPackageVersionAssetOutput, error)
	ListDomainsFn  func(*codeartifact.ListDomainsInput) (*codeartifact.ListDomainsOutput, error)
	ListReposFn    func(*codeartifact.ListRepositoriesInDomainInput) (*codeartifact.ListRepositoriesInDomainOutput, error)
	ListPackagesFn func(*codeartifact.ListPackagesInput) (*codeartifact.ListPackagesOutput, error)
}

var _ cob.CodeArtifactAPI = (*FakeCA)(nil)

func (f *FakeCA) DescribePackageVersion(_ context.Context, in *codeartifact.DescribePackageVersionInput, _ ...func(*codeartifact.Options)) (*codeartifact.DescribePackageVersionOutput, error) {
	if f.DescribeFn != nil {
		return f.DescribeFn(in)
	}
	return &codeartifact.DescribePackageVersionOutput{}, nil
}

func (f *FakeCA) ListPackageVersions(_ context.Context, in *codeartifact.ListPackageVersionsInput, _ ...func(*codeartifact.Options)) (*codeartifact.ListPackageVersionsOutput, error) {
	if f.ListVersionsFn != nil {
		return f.ListVersionsFn(in)
	}
	return &codeartifact.ListPackageVersionsOutput{}, nil
}

func (f *FakeCA) ListPackageVersionAssets(_ context.Context, in *codeartifact.ListPackageVersionAssetsInput, _ ...func(*codeartifact.Options)) (*codeartifact.ListPackageVersionAssetsOutput, error) {
	if f.ListAssetsFn != nil {
		return f.ListAssetsFn(in)
	}
	return &codeartifact.ListPackageVersionAssetsOutput{}, nil
}

func (f *FakeCA) DeletePackageVersions(_ context.Context, in *codeartifact.DeletePackageVersionsInput, _ ...func(*codeartifact.Options)) (*codeartifact.DeletePackageVersionsOutput, error) {
	if f.DeleteFn != nil {
		return f.DeleteFn(in)
	}
	return &codeartifact.DeletePackageVersionsOutput{}, nil
}

func (f *FakeCA) PublishPackageVersion(_ context.Context, in *codeartifact.PublishPackageVersionInput, _ ...func(*codeartifact.Options)) (*codeartifact.PublishPackageVersionOutput, error) {
	if f.PublishFn != nil {
		return f.PublishFn(in)
	}
	return &codeartifact.PublishPackageVersionOutput{}, nil
}

func (f *FakeCA) GetPackageVersionAsset(_ context.Context, in *codeartifact.GetPackageVersionAssetInput, _ ...func(*codeartifact.Options)) (*codeartifact.GetPackageVersionAssetOutput, error) {
	if f.GetAssetFn != nil {
		return f.GetAssetFn(in)
	}
	// Default to "asset doesn't exist": an empty output has a nil Asset
	// reader, which would panic in any caller that streamed it.
	return nil, &catypes.ResourceNotFoundException{}
}

func (f *FakeCA) ListDomains(_ context.Context, in *codeartifact.ListDomainsInput, _ ...func(*codeartifact.Options)) (*codeartifact.ListDomainsOutput, error) {
	if f.ListDomainsFn != nil {
		return f.ListDomainsFn(in)
	}
	return &codeartifact.ListDomainsOutput{}, nil
}

func (f *FakeCA) ListRepositoriesInDomain(_ context.Context, in *codeartifact.ListRepositoriesInDomainInput, _ ...func(*codeartifact.Options)) (*codeartifact.ListRepositoriesInDomainOutput, error) {
	if f.ListReposFn != nil {
		return f.ListReposFn(in)
	}
	return &codeartifact.ListRepositoriesInDomainOutput{}, nil
}

func (f *FakeCA) ListPackages(_ context.Context, in *codeartifact.ListPackagesInput, _ ...func(*codeartifact.Options)) (*codeartifact.ListPackagesOutput, error) {
	if f.ListPackagesFn != nil {
		return f.ListPackagesFn(in)
	}
	return &codeartifact.ListPackagesOutput{}, nil
}

// UseFake installs an in-memory CodeArtifact client and buffer-backed
// output for one test, restoring cliutil's NewClient / NewWriter seams
// on cleanup. Per-test settings (JSON/quiet/profile/etc.) live on the
// returned *cliutil.Config, not on package-level globals, so each test
// gets a fresh Config instead of save/restore boilerplate. The returned
// buffers receive everything the command would have printed.
func UseFake(t *testing.T, ca cob.CodeArtifactAPI) (cfg *cliutil.Config, stdout, stderr *bytes.Buffer) {
	t.Helper()
	stdout, stderr = &bytes.Buffer{}, &bytes.Buffer{}
	cfg = &cliutil.Config{}

	// Isolate cwd and HOME so a developer's local .cob/current (cwd or
	// home global) can't leak into test runs via ResolveTarget's
	// walk-up. Without this, every cli/* test running from inside the
	// repo would inherit whatever pointer was set by an earlier smoke
	// test — turning "no args + no current package" assertions into
	// flaky "got merged with whatever happens to be on disk" failures.
	// Each call to UseFake gets a fresh tmpdir-as-cwd and tmpdir-as-HOME.
	prevCwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	isoCwd := t.TempDir()
	if err := os.Chdir(isoCwd); err != nil {
		t.Fatalf("chdir %s: %v", isoCwd, err)
	}
	isoHome := t.TempDir()
	prevHome, hadHome := os.LookupEnv("HOME")
	prevXDG, hadXDG := os.LookupEnv("XDG_CONFIG_HOME")
	os.Setenv("HOME", isoHome)
	os.Setenv("XDG_CONFIG_HOME", isoHome)

	origClient, origWriter := cliutil.NewClient, cliutil.NewWriter
	cliutil.NewClient = func(context.Context, cob.ClientOptions) (*cob.Client, error) {
		return &cob.Client{CodeArtifact: ca, Region: "us-east-2"}, nil
	}
	cliutil.NewWriter = func(c *cliutil.Config) *output.Writer {
		// Tests always use the stream-mode renderer (NewWithWriters
		// forces isTTY=false), so the live bubbletea path is never
		// engaged from a unit test — deterministic output, no TUI
		// teardown to wait on.
		return output.NewWithWriters(stdout, stderr, output.Mode{
			JSON:  c.JSON,
			Quiet: c.Quiet,
			NoTUI: c.NoTUI,
		})
	}

	t.Cleanup(func() {
		cliutil.NewClient, cliutil.NewWriter = origClient, origWriter
		_ = os.Chdir(prevCwd)
		if hadHome {
			os.Setenv("HOME", prevHome)
		} else {
			os.Unsetenv("HOME")
		}
		if hadXDG {
			os.Setenv("XDG_CONFIG_HOME", prevXDG)
		} else {
			os.Unsetenv("XDG_CONFIG_HOME")
		}
	})
	return cfg, stdout, stderr
}

// WantExit asserts err is an *cliutil.ExitError carrying the given code.
func WantExit(t *testing.T, err error, code int) {
	t.Helper()
	var ee *cliutil.ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("expected *cliutil.ExitError(%d), got %v (%T)", code, err, err)
	}
	if ee.Code != code {
		t.Fatalf("exit code = %d, want %d", ee.Code, code)
	}
}

// WriteFile creates a file under dir and returns its path.
func WriteFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}
