package cli

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
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
	if err := runLog(ctx, cfg, "acme/dev/tools/app@2.1.0", false); err != nil {
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
	if err := runLog(ctx, cfg, "acme/dev/tools/app@2.1.0", false); err != nil {
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
	err := runLog(ctx, cfg, "acme/dev/tools/app@2.1.0", false)
	wantExit(t, err, cob.ExitError)
}

func TestRunLogRequiresVersion(t *testing.T) {
	ctx := context.Background()
	cfg, _, _ := useFake(t, &fakeCA{})
	err := runLog(ctx, cfg, "acme/dev/tools/app", false)
	wantExit(t, err, cob.ExitError)
}

func TestRunLogPartialCoords(t *testing.T) {
	ctx := context.Background()
	cfg, _, _ := useFake(t, &fakeCA{})
	err := runLog(ctx, cfg, "acme/dev", false)
	wantExit(t, err, cob.ExitError)
}

// chainRefFake serves the provenance asset AND a per-repo
// VersionStatus answer keyed off the request's Repository, so
// --check-references can be exercised end-to-end. existsByRepo[repo]
// of type bool says "version exists / deleted"; an error value injects a
// transient probe failure.
func chainRefFake(t *testing.T, prov *cob.Provenance, existsByRepo map[string]any) *fakeCA {
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
		describeFn: func(in *codeartifact.DescribePackageVersionInput) (*codeartifact.DescribePackageVersionOutput, error) {
			repo := aws.ToString(in.Repository)
			answer, ok := existsByRepo[repo]
			if !ok {
				return nil, &catypes.ResourceNotFoundException{}
			}
			switch v := answer.(type) {
			case bool:
				if !v {
					return nil, &catypes.ResourceNotFoundException{}
				}
				return &codeartifact.DescribePackageVersionOutput{
					PackageVersion: &catypes.PackageVersionDescription{Status: catypes.PackageVersionStatusPublished},
				}, nil
			case error:
				return nil, v
			}
			return nil, &catypes.ResourceNotFoundException{}
		},
	}
}

func TestRunLogCheckReferencesAnnotatesDeleted(t *testing.T) {
	// Chain: published in dev, promoted into staging. We log the staging
	// copy. The source repo "dev" has been deleted; "staging" still has
	// the version (it's what we're logging from).
	ctx := context.Background()
	cfg, stdout, stderr := useFake(t, chainRefFake(t, sampleProvenance(), map[string]any{
		"dev":     false,
		"staging": true,
	}))
	if err := runLog(ctx, cfg, "acme/staging/tools/app@2.1.0", true); err != nil {
		t.Fatalf("log --check-references: %v", err)
	}
	out := stdout.String()
	// publish.Repository = acme/dev (deleted) and promote.From = acme/dev
	// (deleted) — both should pick up the annotation.
	if !strings.Contains(out, "acme/dev (deleted)") {
		t.Errorf("expected 'acme/dev (deleted)' annotation:\n%s", out)
	}
	if strings.Contains(out, "acme/staging (deleted)") {
		t.Errorf("acme/staging should not be flagged deleted:\n%s", out)
	}
	if !strings.Contains(stderr.String(), "1 deleted") {
		t.Errorf("expected '1 deleted' summary on stderr:\n%s", stderr.String())
	}
}

func TestRunLogCheckReferencesSilentWhenAllResolve(t *testing.T) {
	// Clean chain → no inline annotations, no warning. Common-case
	// readability matters here.
	ctx := context.Background()
	cfg, stdout, stderr := useFake(t, chainRefFake(t, sampleProvenance(), map[string]any{
		"dev":     true,
		"staging": true,
	}))
	if err := runLog(ctx, cfg, "acme/staging/tools/app@2.1.0", true); err != nil {
		t.Fatalf("log --check-references: %v", err)
	}
	if strings.Contains(stdout.String(), "(deleted)") || strings.Contains(stdout.String(), "(?)") {
		t.Errorf("clean chain should have no inline annotations:\n%s", stdout.String())
	}
	if strings.Contains(stderr.String(), "chain references:") {
		t.Errorf("clean chain should not emit a warning:\n%s", stderr.String())
	}
}

func TestRunLogCheckReferencesAnnotatesProbeFailure(t *testing.T) {
	// A transient probe error (auth, network) renders as (?) — distinct
	// from (deleted) — so operators can't confuse "couldn't check" with
	// "definitely gone".
	ctx := context.Background()
	cfg, stdout, _ := useFake(t, chainRefFake(t, sampleProvenance(), map[string]any{
		"dev":     &catypes.AccessDeniedException{Message: aws.String("denied")},
		"staging": true,
	}))
	if err := runLog(ctx, cfg, "acme/staging/tools/app@2.1.0", true); err != nil {
		t.Fatalf("log --check-references: %v", err)
	}
	if !strings.Contains(stdout.String(), "acme/dev (?)") {
		t.Errorf("expected '(?)' annotation for probe failure:\n%s", stdout.String())
	}
}

func TestRunLogCheckReferencesNoOpInJSONMode(t *testing.T) {
	// JSON consumers can probe themselves; we don't bend the shape with
	// an extra "references" field. The output must round-trip cleanly to
	// a Provenance struct.
	ctx := context.Background()
	cfg, stdout, _ := useFake(t, chainRefFake(t, sampleProvenance(), map[string]any{
		"dev":     false,
		"staging": true,
	}))
	cfg.JSON = true
	if err := runLog(ctx, cfg, "acme/staging/tools/app@2.1.0", true); err != nil {
		t.Fatalf("log --check-references --json: %v", err)
	}
	var got cob.Provenance
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("output is not a clean Provenance: %v\n%s", err, stdout.String())
	}
	if got.Schema != 2 {
		t.Errorf("provenance not round-tripped: %+v", got)
	}
}

func TestProbeChainReferencesDedupes(t *testing.T) {
	// A chain that mentions the same repo three times (publish.Repository,
	// promote.From, promote.To pointing back) should probe each repo
	// exactly once. The set dedupes by "domain/repo" before fan-out.
	calls := map[string]int{}
	var mu sync.Mutex
	ca := &fakeCA{
		describeFn: func(in *codeartifact.DescribePackageVersionInput) (*codeartifact.DescribePackageVersionOutput, error) {
			mu.Lock()
			calls[aws.ToString(in.Repository)]++
			mu.Unlock()
			return &codeartifact.DescribePackageVersionOutput{
				PackageVersion: &catypes.PackageVersionDescription{Status: catypes.PackageVersionStatusPublished},
			}, nil
		},
	}
	cfg, _, _ := useFake(t, ca)
	_ = cfg
	registry := cob.NewRegistry(&cob.Client{CodeArtifact: ca, Region: "us-east-2"})
	coords := &cob.PackageCoordinates{Namespace: "tools", Package: "app", Version: "2.1.0"}
	prov := &cob.Provenance{Chain: []cob.ProvenanceEvent{
		{Event: "publish", Repository: "acme/dev", Version: "2.1.0"},
		{Event: "promote", From: "acme/dev", To: "acme/staging"},
		{Event: "promote", From: "acme/staging", To: "acme/prod"},
	}}
	probeChainReferences(context.Background(), registry, coords, prov)
	for repo, want := range map[string]int{"dev": 1, "staging": 1, "prod": 1} {
		if got := calls[repo]; got != want {
			t.Errorf("repo %q probed %d times, want %d (probe set should dedupe)", repo, got, want)
		}
	}
}
