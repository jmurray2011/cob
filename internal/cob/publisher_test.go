package cob

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/codeartifact"
	catypes "github.com/aws/aws-sdk-go-v2/service/codeartifact/types"
)

// sha256("abc")
const sha256abc = "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"

type stubSource struct {
	uri, filename string
	meta          *AssetMetadata
	body          string
}

func (s stubSource) URI() string                                     { return s.uri }
func (s stubSource) Filename() string                                { return s.filename }
func (s stubSource) Resolve(context.Context) (*AssetMetadata, error) { return s.meta, nil }
func (s stubSource) Open(context.Context) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader(s.body)), nil
}
func (s stubSource) Origin(context.Context) (*Origin, error) { return nil, nil }

func TestPublishAssetUsesFilenameNotManifestKey(t *testing.T) {
	var captured *codeartifact.PublishPackageVersionInput
	ca := &fakeCA{publishFn: func(in *codeartifact.PublishPackageVersionInput) (*codeartifact.PublishPackageVersionOutput, error) {
		captured = in
		return &codeartifact.PublishPackageVersionOutput{}, nil
	}}

	src := stubSource{uri: "s3://b/k", filename: "real-asset.bin", meta: &AssetMetadata{Size: 3}, body: "abc"}
	p := NewPublisher(newTestClient(ca))

	res, err := p.PublishAsset(context.Background(), coords(), "manifest-key", src, true)
	if err != nil {
		t.Fatalf("PublishAsset: %v", err)
	}
	if captured == nil {
		t.Fatal("PublishPackageVersion was not called")
	}
	if got := aws.ToString(captured.AssetName); got != "real-asset.bin" {
		t.Errorf("AssetName = %q, want the source filename %q (not the manifest key)", got, "real-asset.bin")
	}
	if !aws.ToBool(captured.Unfinished) {
		t.Error("Unfinished should be true when unfinished=true")
	}
	if got := aws.ToString(captured.AssetSHA256); got != sha256abc {
		t.Errorf("AssetSHA256 = %q, want %q", got, sha256abc)
	}
	if res.Name != "manifest-key" || res.SHA256 != sha256abc {
		t.Errorf("result Name=%q SHA256=%q", res.Name, res.SHA256)
	}
}

func TestPublishAssetEmptyFilenameErrors(t *testing.T) {
	src := stubSource{uri: "s3://b/", filename: "", meta: &AssetMetadata{}}
	_, err := NewPublisher(newTestClient(&fakeCA{})).PublishAsset(context.Background(), coords(), "k", src, false)
	if err == nil {
		t.Fatal("expected error when source filename is empty")
	}
}

// TestDeleteVersionFailedVersions is the regression for the FailedVersions
// fix: DeletePackageVersions returns 200 even when it refuses to delete, so
// the per-version outcome must be inspected. NOT_FOUND is benign for --force.
func TestDeleteVersionFailedVersions(t *testing.T) {
	mk := func(code catypes.PackageVersionErrorCode) *fakeCA {
		return &fakeCA{deleteFn: func(*codeartifact.DeletePackageVersionsInput) (*codeartifact.DeletePackageVersionsOutput, error) {
			return &codeartifact.DeletePackageVersionsOutput{
				FailedVersions: map[string]catypes.PackageVersionError{
					"1.0.0": {ErrorCode: code, ErrorMessage: aws.String("msg")},
				},
			}, nil
		}}
	}

	t.Run("NOT_ALLOWED -> error", func(t *testing.T) {
		err := NewPublisher(newTestClient(mk(catypes.PackageVersionErrorCodeNotAllowed))).
			DeleteVersion(context.Background(), coords())
		if err == nil {
			t.Fatal("expected error when FailedVersions reports NOT_ALLOWED")
		}
	})

	t.Run("NOT_FOUND -> benign (already gone)", func(t *testing.T) {
		err := NewPublisher(newTestClient(mk(catypes.PackageVersionErrorCodeNotFound))).
			DeleteVersion(context.Background(), coords())
		if err != nil {
			t.Fatalf("NOT_FOUND should be benign for --force, got: %v", err)
		}
	})

	t.Run("no failures -> nil", func(t *testing.T) {
		ca := &fakeCA{deleteFn: func(*codeartifact.DeletePackageVersionsInput) (*codeartifact.DeletePackageVersionsOutput, error) {
			return &codeartifact.DeletePackageVersionsOutput{}, nil
		}}
		if err := NewPublisher(newTestClient(ca)).DeleteVersion(context.Background(), coords()); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
}
