package cli

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/codeartifact"
	catypes "github.com/aws/aws-sdk-go-v2/service/codeartifact/types"

	"github.com/jmurray2011/cob/internal/cob"
)

// provenanceCA returns a fakeCA whose GetPackageVersionAsset for
// cob-provenance.json returns the given provenance document (JSON-encoded).
// Other asset names return ResourceNotFound, matching real behavior.
func provenanceCA(t *testing.T, prov *cob.Provenance) *fakeCA {
	t.Helper()
	body, err := json.Marshal(prov)
	if err != nil {
		t.Fatalf("marshal provenance: %v", err)
	}
	return &fakeCA{
		getAssetFn: func(in *codeartifact.GetPackageVersionAssetInput) (*codeartifact.GetPackageVersionAssetOutput, error) {
			if in.Asset == nil || *in.Asset != cob.ProvenanceFile {
				return nil, &catypes.ResourceNotFoundException{}
			}
			return &codeartifact.GetPackageVersionAssetOutput{Asset: io.NopCloser(strings.NewReader(string(body)))}, nil
		},
	}
}

// sampleProvenance is a minimal-but-realistic two-event chain: publish in
// dev, promote into staging. Used to verify the renderer hits both event
// kinds and the asset/origin path.
func sampleProvenance() *cob.Provenance {
	versioned := true
	return &cob.Provenance{
		Schema:  2,
		Package: "tools/app",
		Chain: []cob.ProvenanceEvent{
			{
				Event: "publish", Repository: "acme/dev", Version: "2.1.0",
				Time: "2026-05-01T12:00:00Z", CobVersion: "v0.0.2", Region: "us-east-2",
				ManifestSHA256: "abcd1234abcd1234abcd1234abcd1234abcd1234abcd1234abcd1234abcd1234",
				Actor:          cob.Actor{ARN: "arn:aws:iam::123:user/alice"},
			},
			{
				Event: "promote", From: "acme/dev", To: "acme/staging",
				Time: "2026-05-02T09:00:00Z", CobVersion: "v0.0.2", Region: "us-east-2",
				Actor: cob.Actor{ARN: "arn:aws:iam::123:user/bob"},
			},
		},
		Assets: []cob.ProvenanceEntry{{
			Key: "payload", Source: "s3://src-bucket/payload.bin",
			Asset:  "payload.bin",
			SHA256: "deadbeef00000000000000000000000000000000000000000000000000000000",
			Size:   42,
			Origin: &cob.Origin{
				Type: "s3", Bucket: "src-bucket", Key: "payload.bin",
				VersionID: "vABC", Versioned: &versioned, Region: "us-east-2",
			},
		}},
	}
}

func TestRunLogTextEmitsChainAndOrigins(t *testing.T) {
	ctx := context.Background()
	cfg, stdout, _ := useFake(t, provenanceCA(t, sampleProvenance()))
	if err := runLog(ctx, cfg, "acme/dev/tools/app@2.1.0"); err != nil {
		t.Fatalf("log: %v", err)
	}
	out := stdout.String()
	// One line per event with the right actors; one asset with its s3 origin.
	for _, want := range []string{
		"chain of evidence:",
		"1. publish",
		"alice",
		"2. promote",
		"bob",
		"acme/dev → acme/staging",
		"where the files came from:",
		"payload.bin",
		"s3://src-bucket/payload.bin v=vABC",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("log output missing %q in:\n%s", want, out)
		}
	}
}

func TestRunLogJSONEmitsProvenance(t *testing.T) {
	ctx := context.Background()
	cfg, stdout, _ := useFake(t, provenanceCA(t, sampleProvenance()))
	cfg.JSON = true
	if err := runLog(ctx, cfg, "acme/dev/tools/app@2.1.0"); err != nil {
		t.Fatalf("log --json: %v", err)
	}
	// Round-trip back through the Provenance struct so we're asserting the
	// canonical JSON shape, not a substring of the rendering.
	var got cob.Provenance
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("log --json emitted invalid JSON: %v\n%s", err, stdout.String())
	}
	if got.Schema != 2 || got.Package != "tools/app" {
		t.Errorf("provenance round-trip wrong: %+v", got)
	}
	if len(got.Chain) != 2 || got.Chain[0].Event != "publish" || got.Chain[1].Event != "promote" {
		t.Errorf("chain wrong: %+v", got.Chain)
	}
	if len(got.Assets) != 1 || got.Assets[0].Origin == nil || got.Assets[0].Origin.Bucket != "src-bucket" {
		t.Errorf("assets/origin wrong: %+v", got.Assets)
	}
}

func TestRunLogMissingProvenance(t *testing.T) {
	// A version that wasn't published with cob has no provenance asset —
	// FetchProvenance returns (nil, nil). log should refuse rather than
	// fabricate a chain.
	ctx := context.Background()
	cfg, _, _ := useFake(t, &fakeCA{}) // default getAsset returns ResourceNotFound
	err := runLog(ctx, cfg, "acme/dev/tools/app@2.1.0")
	wantExit(t, err, cob.ExitError)
}

func TestRunLogRequiresVersion(t *testing.T) {
	ctx := context.Background()
	cfg, _, _ := useFake(t, &fakeCA{})
	err := runLog(ctx, cfg, "acme/dev/tools/app")
	wantExit(t, err, cob.ExitError)
}

func TestRunLogPartialCoords(t *testing.T) {
	ctx := context.Background()
	cfg, _, _ := useFake(t, &fakeCA{})
	err := runLog(ctx, cfg, "acme/dev")
	wantExit(t, err, cob.ExitError)
}
