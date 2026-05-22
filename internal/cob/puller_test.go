package cob

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/codeartifact"
)

func getAsset(content string) func(*codeartifact.GetPackageVersionAssetInput) (*codeartifact.GetPackageVersionAssetOutput, error) {
	return func(*codeartifact.GetPackageVersionAssetInput) (*codeartifact.GetPackageVersionAssetOutput, error) {
		return &codeartifact.GetPackageVersionAssetOutput{Asset: io.NopCloser(strings.NewReader(content))}, nil
	}
}

func TestPullAssetVerifiesAndWrites(t *testing.T) {
	p := NewPuller(newTestClient(&fakeCA{getAssetFn: getAsset("abc")}))
	dst := filepath.Join(t.TempDir(), "out.bin")

	r, err := p.PullAsset(context.Background(), coords(), AssetInfo{Name: "out.bin", SHA256: sha256abc}, dst)
	if err != nil {
		t.Fatalf("PullAsset: %v", err)
	}
	if r.SHA256 != sha256abc {
		t.Errorf("result SHA256 = %q, want the verified hash %q", r.SHA256, sha256abc)
	}
	got, _ := os.ReadFile(dst)
	if string(got) != "abc" {
		t.Errorf("file content = %q", got)
	}
	// A pulled asset must land at 0600: sensitive material (GPG keys,
	// certs) is the tool's whole use case, so group/world-readable would
	// be the wrong default on a shared host. The owner can still read.
	if fi, _ := os.Stat(dst); fi.Mode().Perm() != 0o600 {
		t.Errorf("pulled file mode = %o, want 600", fi.Mode().Perm())
	}
}

func TestPullAssetRejectsHashMismatch(t *testing.T) {
	p := NewPuller(newTestClient(&fakeCA{getAssetFn: getAsset("abc")}))
	dst := filepath.Join(t.TempDir(), "out.bin")

	_, err := p.PullAsset(context.Background(), coords(), AssetInfo{Name: "out.bin", SHA256: "deadbeef"}, dst)
	if err == nil {
		t.Fatal("expected a hash-mismatch error")
	}
	if _, statErr := os.Stat(dst); !os.IsNotExist(statErr) {
		t.Error("a corrupt download must not be left on disk")
	}
}

func TestPullAssetSkipsOnMatchingHash(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "out.bin")
	if err := os.WriteFile(dst, []byte("abc"), 0o644); err != nil {
		t.Fatal(err)
	}
	called := false
	p := NewPuller(newTestClient(&fakeCA{getAssetFn: func(*codeartifact.GetPackageVersionAssetInput) (*codeartifact.GetPackageVersionAssetOutput, error) {
		called = true
		return getAsset("abc")(nil)
	}}))
	r, err := p.PullAsset(context.Background(), coords(), AssetInfo{Name: "out.bin", SHA256: sha256abc}, dst)
	if err != nil {
		t.Fatal(err)
	}
	if r.Method != "skipped" || called {
		t.Errorf("existing matching file must skip the download (method=%q called=%v)", r.Method, called)
	}
}

func TestPullAssetAtomicOnReadError(t *testing.T) {
	p := NewPuller(newTestClient(&fakeCA{getAssetFn: func(*codeartifact.GetPackageVersionAssetInput) (*codeartifact.GetPackageVersionAssetOutput, error) {
		return &codeartifact.GetPackageVersionAssetOutput{Asset: io.NopCloser(errReader{})}, nil
	}}))
	dir := t.TempDir()
	dst := filepath.Join(dir, "out.bin")
	if err := os.WriteFile(dst, []byte("ORIGINAL"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := p.PullAsset(context.Background(), coords(), AssetInfo{Name: "out.bin", SHA256: sha256abc}, dst); err == nil {
		t.Fatal("expected a read error")
	}
	// The prior file must survive a failed download; no temp left behind.
	if got, _ := os.ReadFile(dst); string(got) != "ORIGINAL" {
		t.Errorf("a failed download destroyed the existing file: %q", got)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".cob-pull-") {
			t.Errorf("temp file leaked: %s", e.Name())
		}
	}
}
