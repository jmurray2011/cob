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

func sampleProv() *Provenance {
	tru := true
	return &Provenance{
		Package: "ns/pkg",
		Assets: []ProvenanceEntry{
			{Key: "app", Source: "s3://b/app-1.bin", Asset: "app-1.bin", SHA256: "aaaa", Size: 10,
				Origin: &Origin{Type: "s3", Bucket: "b", Key: "app-1.bin", ETag: "e1", VersionID: "v1", Versioned: &tru}},
			{Key: "cfg", Source: "./c.yaml", Asset: "c.yaml", SHA256: "bbbb", Size: 2,
				Origin: &Origin{Type: "file", Path: "/x/c.yaml", Mtime: "2026-01-01T00:00:00Z"}},
		},
		Chain: []ProvenanceEvent{{
			Event: "publish", Repository: "dom/dev", Version: "1.2.3",
			Time: "2026-05-18T00:00:00Z", CobVersion: "v9", Region: "us-east-2",
			ManifestSHA256: "deadbeef",
			Actor:          Actor{Account: "111122223333", ARN: "arn:aws:sts::111122223333:assumed-role/r/u", UserID: "AROA:u"},
		}},
	}
}

func TestProvenanceV2RoundTrip(t *testing.T) {
	p := sampleProv()
	raw := p.Marshal()
	if p.Schema != ProvenanceSchema {
		t.Fatalf("Marshal must stamp schema, got %d", p.Schema)
	}

	var got Provenance
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Schema != 2 || got.Package != "ns/pkg" || len(got.Assets) != 2 || len(got.Chain) != 1 {
		t.Fatalf("round trip lost data: %+v", got)
	}
	if got.Chain[0].Event != "publish" || got.Chain[0].Actor.Account != "111122223333" {
		t.Fatalf("chain/actor not preserved: %+v", got.Chain[0])
	}
	if got.SHAByAsset()["app-1.bin"] != "aaaa" {
		t.Fatalf("SHAByAsset wrong: %v", got.SHAByAsset())
	}
	o := got.OriginByAsset()
	if o["app-1.bin"] == nil || o["app-1.bin"].VersionID != "v1" || o["c.yaml"].Type != "file" {
		t.Fatalf("OriginByAsset wrong: %+v", o)
	}
}

func TestFetchProvenanceV2(t *testing.T) {
	ctx := context.Background()
	doc := sampleProv().Marshal()

	t.Run("present", func(t *testing.T) {
		ca := &fakeCA{getAssetFn: func(in *codeartifact.GetPackageVersionAssetInput) (*codeartifact.GetPackageVersionAssetOutput, error) {
			if *in.Asset != ProvenanceFile {
				t.Fatalf("fetched %s, want %s", *in.Asset, ProvenanceFile)
			}
			return &codeartifact.GetPackageVersionAssetOutput{Asset: io.NopCloser(bytes.NewReader(doc))}, nil
		}}
		p, err := FetchProvenance(ctx, ca, coords())
		if err != nil || p == nil || len(p.Chain) != 1 || p.SHAByAsset()["app-1.bin"] != "aaaa" {
			t.Fatalf("got p=%+v err=%v", p, err)
		}
	})

	t.Run("absent -> (nil,nil)", func(t *testing.T) {
		ca := &fakeCA{getAssetFn: func(*codeartifact.GetPackageVersionAssetInput) (*codeartifact.GetPackageVersionAssetOutput, error) {
			return nil, &catypes.ResourceNotFoundException{}
		}}
		p, err := FetchProvenance(ctx, ca, coords())
		if err != nil || p != nil {
			t.Fatalf("absent must be (nil,nil), got p=%+v err=%v", p, err)
		}
	})
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
	if o, _ := s.Origin(context.Background()); o != nil {
		t.Errorf("provenance asset must have nil origin, got %+v", o)
	}
	r, _ := s.Open(context.Background())
	if got, _ := io.ReadAll(r); !bytes.Equal(got, data) {
		t.Fatalf("Open = %q", got)
	}
}

// TestCASourceOriginRecursive: a ca:// source embeds the upstream's own
// provenance (recursion terminates: the upstream doc is already complete);
// an upstream without provenance is recorded honestly, not as an error.
func TestCASourceOriginRecursive(t *testing.T) {
	ctx := context.Background()
	upstream := (&Provenance{Package: "tools/lib", Chain: []ProvenanceEvent{{Event: "publish"}}}).Marshal()

	t.Run("embedded", func(t *testing.T) {
		ca := &fakeCA{getAssetFn: func(*codeartifact.GetPackageVersionAssetInput) (*codeartifact.GetPackageVersionAssetOutput, error) {
			return &codeartifact.GetPackageVersionAssetOutput{Asset: io.NopCloser(bytes.NewReader(upstream))}, nil
		}}
		src, err := NewCASource(ca, "ca://acme/dev/tools/lib@2.0.0/lib.deb")
		if err != nil {
			t.Fatal(err)
		}
		o, err := src.Origin(ctx)
		if err != nil || o == nil || o.Type != "ca" {
			t.Fatalf("origin=%+v err=%v", o, err)
		}
		if o.UpstreamStatus != UpstreamEmbedded || o.UpstreamProvenance == nil || o.UpstreamProvenance.Package != "tools/lib" {
			t.Fatalf("upstream not embedded: status=%q prov=%+v", o.UpstreamStatus, o.UpstreamProvenance)
		}
		if o.Domain != "acme" || o.CARepository != "dev" || o.CAVersion != "2.0.0" || o.CAAsset != "lib.deb" {
			t.Fatalf("ca coords wrong: %+v", o)
		}
	})

	t.Run("no-cob-provenance", func(t *testing.T) {
		ca := &fakeCA{getAssetFn: func(*codeartifact.GetPackageVersionAssetInput) (*codeartifact.GetPackageVersionAssetOutput, error) {
			return nil, &catypes.ResourceNotFoundException{}
		}}
		src, _ := NewCASource(ca, "ca://acme/dev/tools/lib@2.0.0/lib.deb")
		o, err := src.Origin(ctx)
		if err != nil || o.UpstreamStatus != UpstreamNoProv || o.UpstreamProvenance != nil {
			t.Fatalf("want no-cob-provenance, got status=%q prov=%+v err=%v", o.UpstreamStatus, o.UpstreamProvenance, err)
		}
	})
}
