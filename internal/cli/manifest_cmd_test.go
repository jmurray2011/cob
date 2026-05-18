package cli

import (
	"strings"
	"testing"

	"github.com/jmurray2011/cob/pkg/cob"
)

func mcoords() *cob.PackageCoordinates {
	return &cob.PackageCoordinates{Domain: "berth-test", Repository: "dev", Namespace: "ns", Package: "pkg", Version: "1.0.0"}
}

func TestRenderManifestFromProvenance(t *testing.T) {
	prov := &cob.Provenance{
		Package: "ns/pkg",
		Assets: []cob.ProvenanceEntry{
			{Key: "app", Source: "s3://b/app-1.0.0.bin", Asset: "app-1.0.0.bin"},
			{Key: "", Source: "", Asset: "weird.bin"}, // fallbacks: key→asset, source→ca://
		},
		Chain: []cob.ProvenanceEvent{
			{Event: "publish", Time: "2026-05-18T00:00:00Z", Actor: cob.Actor{ARN: "arn:aws:sts::1:assumed-role/r/bob"}},
		},
	}
	y, ok := renderManifest(mcoords(), prov, nil)
	if !ok {
		t.Fatal("expected ok")
	}
	for _, want := range []string{
		"# Reconstructed by cob from cob-provenance.json",
		"# ns/pkg@1.0.0 in berth-test/dev",
		"# originally published 2026-05-18T00:00:00Z by arn:aws:sts::1:assumed-role/r/bob",
		"promote.stages not recoverable",
		"domain: berth-test",
		"repository: dev",
		"namespace: ns",
		"package: pkg",
		"sources:",
		"  app: s3://b/app-1.0.0.bin",
		"  weird.bin: ca://berth-test/dev/ns/pkg@1.0.0/weird.bin", // empty key+source fall back
	} {
		if !strings.Contains(y, want) {
			t.Errorf("missing %q in:\n%s", want, y)
		}
	}
}

func TestRenderManifestInferredNonCob(t *testing.T) {
	assets := []cob.AssetSummary{
		{Name: "app"},
		{Name: cob.ProvenanceFile}, // must be excluded
		{Name: "config.yaml"},
	}
	y, ok := renderManifest(mcoords(), nil, assets)
	if !ok {
		t.Fatal("expected ok")
	}
	if !strings.Contains(y, "# Inferred by cob") || !strings.Contains(y, "no cob-provenance.json") {
		t.Errorf("missing inferred header:\n%s", y)
	}
	if !strings.Contains(y, "  app: ca://berth-test/dev/ns/pkg@1.0.0/app") ||
		!strings.Contains(y, "  config.yaml: ca://berth-test/dev/ns/pkg@1.0.0/config.yaml") {
		t.Errorf("inferred ca:// sources wrong:\n%s", y)
	}
	if strings.Contains(y, cob.ProvenanceFile+":") {
		t.Errorf("cob-provenance.json must not be emitted as a source:\n%s", y)
	}
}

func TestRenderManifestNoSources(t *testing.T) {
	if _, ok := renderManifest(mcoords(), nil, []cob.AssetSummary{{Name: cob.ProvenanceFile}}); ok {
		t.Fatal("only the provenance asset → no sources → ok must be false")
	}
	if _, ok := renderManifest(mcoords(), nil, nil); ok {
		t.Fatal("no assets → ok must be false")
	}
}
