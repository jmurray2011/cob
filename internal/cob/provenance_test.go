package cob

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/codeartifact"
	catypes "github.com/aws/aws-sdk-go-v2/service/codeartifact/types"
)

func TestNowStampSeam(t *testing.T) {
	orig := now
	defer func() { now = orig }()
	now = func() time.Time { return time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC) }
	if got := NowStamp(); got != "2026-03-04T05:06:07Z" {
		t.Errorf("NowStamp() = %q, want the pinned time (clock seam must be honoured)", got)
	}
}

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
	raw, err := p.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if p.Schema != 0 {
		t.Errorf("Marshal must not mutate the receiver, Schema became %d", p.Schema)
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
	doc, err := sampleProv().Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

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
	upstream, err := (&Provenance{Package: "tools/lib", Chain: []ProvenanceEvent{{Event: "publish"}}}).Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

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

// makeChainedProv builds a Provenance with depth levels of UpstreamProvenance
// nested under a single ca-origin asset, for exercising the depth cap.
func makeChainedProv(depth int) *Provenance {
	root := &Provenance{Package: "root", Assets: []ProvenanceEntry{{
		Key: "a", Asset: "a.bin", Origin: &Origin{Type: "ca", UpstreamStatus: UpstreamEmbedded},
	}}}
	cur := root.Assets[0].Origin
	for i := 0; i < depth; i++ {
		next := &Provenance{Package: fmt.Sprintf("up-%d", i), Assets: []ProvenanceEntry{{
			Key: "a", Asset: "a.bin", Origin: &Origin{Type: "ca", UpstreamStatus: UpstreamEmbedded},
		}}}
		cur.UpstreamProvenance = next
		cur = next.Assets[0].Origin
	}
	return root
}

// chainDepth counts how many UpstreamProvenance levels are reachable from p's
// first asset's origin — mirrors makeChainedProv so tests can verify pruning.
func chainDepth(p *Provenance) int {
	d := 0
	if p == nil || len(p.Assets) == 0 || p.Assets[0].Origin == nil {
		return 0
	}
	o := p.Assets[0].Origin
	for o.UpstreamProvenance != nil && len(o.UpstreamProvenance.Assets) > 0 {
		d++
		next := o.UpstreamProvenance.Assets[0].Origin
		if next == nil {
			break
		}
		o = next
	}
	return d
}

func TestPruneUpstreamProvenanceCapsDepth(t *testing.T) {
	// remaining=0 must drop the immediate UpstreamProvenance and flip status.
	p := makeChainedProv(3)
	PruneUpstreamProvenance(p, 0)
	if chainDepth(p) != 0 {
		t.Errorf("remaining=0 should drop all upstreams; depth=%d", chainDepth(p))
	}
	if p.Assets[0].Origin.UpstreamStatus != UpstreamTruncated {
		t.Errorf("cut origin must record UpstreamTruncated, got %q", p.Assets[0].Origin.UpstreamStatus)
	}

	// remaining=2 keeps the first two levels and truncates at the third.
	p = makeChainedProv(5)
	PruneUpstreamProvenance(p, 2)
	if got := chainDepth(p); got != 2 {
		t.Errorf("remaining=2 should keep 2 levels; depth=%d", got)
	}
	// The last-kept origin should now report Truncated where its upstream was.
	cur := p.Assets[0].Origin
	for i := 0; i < 2; i++ {
		cur = cur.UpstreamProvenance.Assets[0].Origin
	}
	if cur.UpstreamStatus != UpstreamTruncated || cur.UpstreamProvenance != nil {
		t.Errorf("truncation marker missing at depth 2: status=%q upstream=%v", cur.UpstreamStatus, cur.UpstreamProvenance)
	}
}

func TestMarshalRejectsOversize(t *testing.T) {
	// Stuff a giant string into a free-form field so the encoded document
	// blows past maxProvenanceBytes — exercises the write-side cap that
	// keeps a downstream cob from publishing a doc its readers would refuse.
	big := strings.Repeat("x", maxProvenanceBytes+1)
	p := &Provenance{Package: big}
	if _, err := p.Marshal(); err == nil {
		t.Fatal("Marshal should refuse a document past maxProvenanceBytes")
	} else if !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("error should mention the limit, got %v", err)
	}
}

func TestCASourceOriginAppliesDepthCap(t *testing.T) {
	// Stage an upstream whose own embedded tree already extends maxUpstreamDepth
	// levels. CASource.Origin must prune it to fit under the new root so the
	// total depth never exceeds maxUpstreamDepth.
	deep := makeChainedProv(maxUpstreamDepth + 4)
	bytes, err := deep.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	ca := &fakeCA{getAssetFn: func(*codeartifact.GetPackageVersionAssetInput) (*codeartifact.GetPackageVersionAssetOutput, error) {
		return &codeartifact.GetPackageVersionAssetOutput{Asset: io.NopCloser(strings.NewReader(string(bytes)))}, nil
	}}
	src, _ := NewCASource(ca, "ca://acme/dev/tools/lib@2.0.0/lib.deb")
	o, err := src.Origin(context.Background())
	if err != nil || o == nil || o.UpstreamProvenance == nil {
		t.Fatalf("origin: o=%+v err=%v", o, err)
	}
	// o.UpstreamProvenance is depth 1 from the new root; remaining-1 = maxUpstreamDepth-1.
	if got := chainDepth(o.UpstreamProvenance); got > maxUpstreamDepth-1 {
		t.Errorf("CASource.Origin must cap embedded chain to maxUpstreamDepth-1 (%d); depth=%d", maxUpstreamDepth-1, got)
	}
}
