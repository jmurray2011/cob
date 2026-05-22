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
	y, err := renderManifest(mcoords(), prov, nil)
	if err != nil {
		t.Fatal(err)
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
	y, err := renderManifest(mcoords(), nil, assets)
	if err != nil {
		t.Fatal(err)
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
	if _, err := renderManifest(mcoords(), nil, []cob.AssetSummary{{Name: cob.ProvenanceFile}}); err == nil {
		t.Fatal("only the provenance asset → no sources → must error")
	}
	if _, err := renderManifest(mcoords(), nil, nil); err == nil {
		t.Fatal("no assets → must error")
	}
}

func TestRenderManifestRejectsControlChars(t *testing.T) {
	// A key/URI from an upstream-controlled provenance must not be able to
	// inject extra YAML lines.
	prov := &cob.Provenance{Assets: []cob.ProvenanceEntry{
		{Key: "ok", Source: "s3://b/ok", Asset: "ok"},
		{Key: "evil\n  injected: x", Source: "s3://b/e", Asset: "e"},
	}}
	if _, err := renderManifest(mcoords(), prov, nil); err == nil {
		t.Fatal("a key with a newline must be rejected")
	}
}
