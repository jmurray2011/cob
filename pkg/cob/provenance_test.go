package cob

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/codeartifact"
	catypes "github.com/aws/aws-sdk-go-v2/service/codeartifact/types"
)

func TestProvenanceMarshalRoundTrip(t *testing.T) {
	p := &Provenance{
		Package:    "ns/pkg",
		Repository: "dom/dev",
		Version:    "1.2.3",
		CobVersion: "v9",
		Assets: []ProvenanceEntry{
			{Key: "app", Source: "s3://b/app-1.2.3.bin", Asset: "app-1.2.3.bin", SHA256: "aaaa", Size: 10},
			{Key: "cfg", Source: "./c.yaml", Asset: "c.yaml", SHA256: "bbbb", Size: 2},
		},
	}
	raw := p.Marshal()

	if p.Schema != ProvenanceSchema {
		t.Errorf("Marshal should stamp Schema, got %d", p.Schema)
	}
	if p.Published == "" {
		t.Error("Marshal should stamp Published")
	}

	var got Provenance
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Version != "1.2.3" || len(got.Assets) != 2 {
		t.Fatalf("round trip lost data: %+v", got)
	}
	m := got.SHAByAsset()
	if m["app-1.2.3.bin"] != "aaaa" || m["c.yaml"] != "bbbb" {
		t.Fatalf("SHAByAsset wrong: %v", m)
	}
}

func TestBytesSource(t *testing.T) {
	data := []byte("provenance bytes")
	s := NewBytesSource(ProvenanceFile, data)
	if s.Filename() != ProvenanceFile {
		t.Errorf("Filename = %q", s.Filename())
	}
	meta, err := s.Resolve(context.Background())
	if err != nil || meta.Size != int64(len(data)) || meta.SHA256 == "" {
		t.Fatalf("Resolve = %+v, %v", meta, err)
	}
	r, _ := s.Open(context.Background())
	got, _ := io.ReadAll(r)
	if !bytes.Equal(got, data) {
		t.Fatalf("Open content = %q", got)
	}
}

func TestFetchProvenance(t *testing.T) {
	ctx := context.Background()
	doc := (&Provenance{Version: "1.0.0", Assets: []ProvenanceEntry{{Asset: "x", SHA256: "deadbeef"}}}).Marshal()

	t.Run("present", func(t *testing.T) {
		ca := &fakeCA{getAssetFn: func(in *codeartifact.GetPackageVersionAssetInput) (*codeartifact.GetPackageVersionAssetOutput, error) {
			if *in.Asset != ProvenanceFile {
				t.Fatalf("expected fetch of %s, got %s", ProvenanceFile, *in.Asset)
			}
			return &codeartifact.GetPackageVersionAssetOutput{Asset: io.NopCloser(bytes.NewReader(doc))}, nil
		}}
		p, err := FetchProvenance(ctx, ca, coords())
		if err != nil || p == nil || p.SHAByAsset()["x"] != "deadbeef" {
			t.Fatalf("got p=%+v err=%v", p, err)
		}
	})

	t.Run("absent -> (nil,nil) for fallback", func(t *testing.T) {
		ca := &fakeCA{getAssetFn: func(*codeartifact.GetPackageVersionAssetInput) (*codeartifact.GetPackageVersionAssetOutput, error) {
			return nil, &catypes.ResourceNotFoundException{}
		}}
		p, err := FetchProvenance(ctx, ca, coords())
		if err != nil || p != nil {
			t.Fatalf("absent provenance must be (nil,nil), got p=%+v err=%v", p, err)
		}
	})
}
