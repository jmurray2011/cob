package cob

import (
	"bytes"
	"context"
	"io"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/codeartifact"
)

// FuzzNewS3Source checks the s3:// URI parser never panics and never returns
// a nil source alongside a nil error.
func FuzzNewS3Source(f *testing.F) {
	for _, s := range []string{
		"", "s3://", "s3://b", "s3://b/k", "s3://b/k/with/slashes",
		"s3:///k", "s3://b/", "notascheme", "s3://b/k?x=1",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, uri string) {
		src, err := NewS3Source(nil, uri)
		if err == nil && src == nil {
			t.Errorf("NewS3Source(%q): nil source with a nil error", uri)
		}
	})
}

// FuzzNewCASource checks the ca:// URI parser never panics and never returns
// a nil source alongside a nil error.
func FuzzNewCASource(f *testing.F) {
	for _, s := range []string{
		"", "ca://", "ca://d/r/n/p@1.0.0/a.jar", "ca://d/r/n/p@1.0.0/path/to/a",
		"ca://d/r/n/p@/a", "ca://d/r/n/p/a", "ca://@/", "ca://d/r/n/p@1.0.0/",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, uri string) {
		src, err := NewCASource(nil, uri)
		if err == nil && src == nil {
			t.Errorf("NewCASource(%q): nil source with a nil error", uri)
		}
	})
}

// FuzzFetchProvenance feeds arbitrary bytes through FetchProvenance's
// JSON decoder, asserting two safety invariants the security model
// rests on:
//
//  1. No panic — Go's stdlib json decoder has no recursion-depth limit,
//     so deeply nested adversarial documents have historically been a
//     stack-blowing concern. The 64 MiB read cap (maxProvenanceBytes)
//     bounds nesting implicitly, but a fuzz that pounds the parser
//     proves we haven't introduced a panic path elsewhere (e.g. a
//     post-unmarshal accessor that derefs a nil sub-field).
//
//  2. Successful parse must yield non-nil — a (nil, nil) return would
//     mean callers see "no provenance" and silently degrade to
//     UpstreamNoProv when arbitrary bytes were in fact accepted.
//     FetchProvenance documents (nil, nil) only for the
//     no-such-asset case, never for a successful unmarshal.
func FuzzFetchProvenance(f *testing.F) {
	good, err := sampleProv().Marshal()
	if err != nil {
		f.Fatalf("marshal sample: %v", err)
	}
	// Seeds: a well-formed v2 doc, empties, a deeply nested truncated
	// structure (stresses the decoder's recursion path), and a few
	// JSON edge cases that have tripped up other parsers historically.
	for _, s := range [][]byte{
		good,
		[]byte(`{}`),
		[]byte(`[]`),
		[]byte(`{"cob_provenance":2,"package":"x","assets":[],"chain":[]}`),
		bytes.Repeat([]byte(`{"assets":[{"origin":{"upstream_provenance":`), 32),
		[]byte(`{"chain":[{"actor":{"arn":" "}}]}`),
		[]byte(`null`),
		[]byte(`"string-not-object"`),
		[]byte(`{"assets":null,"chain":null}`),
	} {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		// LimitReader inside FetchProvenance bounds payload to
		// maxProvenanceBytes+1; bigger inputs are rejected before the
		// decoder runs, which is the contract we want to stress.
		ca := &fakeCA{getAssetFn: func(*codeartifact.GetPackageVersionAssetInput) (*codeartifact.GetPackageVersionAssetOutput, error) {
			return &codeartifact.GetPackageVersionAssetOutput{Asset: io.NopCloser(bytes.NewReader(data))}, nil
		}}
		p, err := FetchProvenance(context.Background(), ca, &PackageCoordinates{
			Domain: "d", Repository: "r", Namespace: "n", Package: "p", Version: "1.0.0",
		})
		if err == nil && p == nil {
			t.Errorf("FetchProvenance returned (nil, nil) on a successful unmarshal — that return is reserved for the not-found case")
		}
		// Exercise the post-unmarshal accessors so a nil-deref in
		// SHAByAsset / OriginByAsset would surface here too.
		if p != nil {
			_ = p.SHAByAsset()
			_ = p.OriginByAsset()
		}
	})
}
