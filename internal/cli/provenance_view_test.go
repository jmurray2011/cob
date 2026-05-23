package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/jmurray2011/cob/internal/cob"
	"github.com/jmurray2011/cob/internal/output"
)

func TestRenderChainAndOrigins(t *testing.T) {
	upstream := &cob.Provenance{
		Package: "tools/lib",
		Assets: []cob.ProvenanceEntry{
			{Asset: "lib.bin", SHA256: "abcd1234deadbeef", Origin: &cob.Origin{Type: "s3", Bucket: "b", Key: "lib.bin", ETag: "E1"}},
		},
		Chain: []cob.ProvenanceEvent{{Event: "publish", Actor: cob.Actor{ARN: "arn:aws:sts::1:assumed-role/r/alice"}}},
	}
	p := &cob.Provenance{
		Package: "ns/app",
		Assets: []cob.ProvenanceEntry{
			{Asset: "app.bin", SHA256: "1111222233334444", Origin: &cob.Origin{Type: "s3", Bucket: "bk", Key: "app.bin", VersionID: "VID9"}},
			{Asset: "lib.bin", SHA256: "abcd1234deadbeef", Origin: &cob.Origin{
				Type: "ca", Domain: "d", CARepository: "dev", Namespace: "tools", Package: "lib",
				CAVersion: "1.0.0", CAAsset: "lib.bin", UpstreamStatus: cob.UpstreamEmbedded, UpstreamProvenance: upstream,
			}},
			{Asset: "c.yaml", SHA256: "ff0011", Origin: &cob.Origin{Type: "file", Path: "/x/c.yaml", Mtime: "2026-01-01T00:00:00Z"}},
		},
		Chain: []cob.ProvenanceEvent{
			{Event: "publish", Repository: "d/dev", Version: "2.0.0", Time: "2026-05-18T00:00:00Z",
				CobVersion: "v9", Region: "us-east-2", ManifestSHA256: "deadbeefcafe",
				Actor: cob.Actor{ARN: "arn:aws:sts::1:assumed-role/r/bob"}},
			{Event: "promote", From: "d/dev", To: "d/staging", Time: "2026-05-18T01:00:00Z",
				Actor: cob.Actor{UserID: "AROA:carol"}},
		},
	}

	var buf bytes.Buffer
	w := output.NewWithWriters(&buf, &buf, false)
	renderChain(w, p, nil)
	renderOrigins(w, p, "")
	s := buf.String()

	for _, want := range []string{
		"chain of evidence:",
		"1. publish  d/dev@2.0.0  2026-05-18T00:00:00Z  by arn:aws:sts::1:assumed-role/r/bob",
		"manifest deadbeef  cob v9  us-east-2",
		"2. promote  d/dev → d/staging  2026-05-18T01:00:00Z  by AROA:carol",
		"where the files came from:",
		"app.bin  (11112222)  s3://bk/app.bin v=VID9",
		"ca://d/dev/tools/lib@1.0.0/lib.bin [embedded]",
		"↳ upstream tools/lib, published by arn:aws:sts::1:assumed-role/r/alice",
		"lib.bin  (abcd1234)  s3://b/lib.bin etag=E1", // recursed upstream origin
		"file /x/c.yaml (2026-01-01T00:00:00Z)",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("rendered output missing %q\n---\n%s", want, s)
		}
	}
}

func TestActorStrFallback(t *testing.T) {
	cases := map[cob.Actor]string{
		{ARN: "a"}:       "a",
		{UserID: "u"}:    "u",
		{Account: "123"}: "account 123",
		{}:               "unknown",
	}
	for in, want := range cases {
		if got := actorStr(in); got != want {
			t.Errorf("actorStr(%+v) = %q, want %q", in, got, want)
		}
	}
}
