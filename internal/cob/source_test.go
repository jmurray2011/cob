package cob

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"
)

func TestNewS3Source(t *testing.T) {
	s, err := NewS3Source(nil, "s3://my-bucket/path/to/app.jar")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s.bucket != "my-bucket" || s.key != "path/to/app.jar" {
		t.Fatalf("got bucket=%q key=%q", s.bucket, s.key)
	}
	if s.Filename() != "app.jar" {
		t.Fatalf("Filename = %q, want app.jar", s.Filename())
	}
	if _, err := NewS3Source(nil, "s3://bucket-only"); err == nil {
		t.Fatal("expected error for missing key")
	}
}

func TestNewCASource(t *testing.T) {
	tests := []struct {
		name      string
		uri       string
		wantErr   bool
		wantAsset string
		wantPkg   string
		wantVer   string
	}{
		{
			name: "simple", uri: "ca://dom/repo/ns/pkg@1.0.0/asset.jar",
			wantAsset: "asset.jar", wantPkg: "pkg", wantVer: "1.0.0",
		},
		{
			// Regression for the fix: generic asset names are path-like and
			// may contain slashes; the whole remainder is the asset name.
			name: "slashed asset name", uri: "ca://dom/repo/ns/pkg@1.0.0/path/to/nested.jar",
			wantAsset: "path/to/nested.jar", wantPkg: "pkg", wantVer: "1.0.0",
		},
		{name: "missing @version", uri: "ca://dom/repo/ns/pkg/asset.jar", wantErr: true},
		{name: "latest rejected", uri: "ca://dom/repo/ns/pkg@latest/asset.jar", wantErr: true},
		{name: "too few segments", uri: "ca://dom/repo/pkg@1.0.0", wantErr: true},
		{name: "empty asset", uri: "ca://dom/repo/ns/pkg@1.0.0/", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := NewCASource(nil, tt.uri)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q", tt.uri)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if c.asset != tt.wantAsset || c.pkg != tt.wantPkg || c.version != tt.wantVer {
				t.Fatalf("got asset=%q pkg=%q ver=%q; want asset=%q pkg=%q ver=%q",
					c.asset, c.pkg, c.version, tt.wantAsset, tt.wantPkg, tt.wantVer)
			}
			if c.Filename() != tt.wantAsset {
				t.Fatalf("Filename = %q, want %q", c.Filename(), tt.wantAsset)
			}
		})
	}
}

func TestFileSourceFilename(t *testing.T) {
	f := NewFileSource("/abs/dir/local-overrides.yaml", "./local-overrides.yaml")
	if f.Filename() != "local-overrides.yaml" {
		t.Fatalf("Filename = %q", f.Filename())
	}
}

func TestAssetResultSetError(t *testing.T) {
	var r AssetResult
	r.SetError(nil)
	if r.ErrorMsg != "" {
		t.Fatalf("nil error should leave ErrorMsg empty, got %q", r.ErrorMsg)
	}
	r.SetError(errors.New("boom"))
	if r.Error == nil || r.ErrorMsg != "boom" {
		t.Fatalf("got Error=%v ErrorMsg=%q", r.Error, r.ErrorMsg)
	}
}

func TestFileSourceResolve(t *testing.T) {
	dir := t.TempDir()
	p := dir + "/x.txt"
	if err := os.WriteFile(p, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	meta, err := NewFileSource(p, "./x.txt").Resolve(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if meta.Size != 5 {
		t.Errorf("Size = %d, want 5", meta.Size)
	}
	// A local file is ground truth — no advertised hash; the streaming pass
	// in the publish path computes the authoritative one.
	if meta.SHA256 != "" {
		t.Errorf("SHA256 = %q, want empty", meta.SHA256)
	}
	if _, err := NewFileSource(dir, "./").Resolve(context.Background()); err == nil {
		t.Error("Resolve must reject a directory")
	}
}

func TestFileSourceOrigin(t *testing.T) {
	dir := t.TempDir()
	p := dir + "/x.txt"
	if err := os.WriteFile(p, []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	o, err := NewFileSource(p, "./x.txt").Origin(context.Background())
	if err != nil || o == nil {
		t.Fatalf("Origin: o=%v err=%v", o, err)
	}
	if o.Type != "file" || o.Path != p {
		t.Errorf("got %+v", o)
	}
	if _, perr := time.Parse(time.RFC3339, o.Mtime); perr != nil {
		t.Errorf("Mtime %q not RFC3339: %v", o.Mtime, perr)
	}
}
