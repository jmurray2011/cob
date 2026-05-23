package cob

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

// fakeS3 is an in-memory S3API for unit tests. The region-redirect retry path
// is covered separately by TestCorrectRegion, so these hooks never need to
// emit a cross-region ResponseError.
type fakeS3 struct {
	headFn func(*s3.HeadObjectInput) (*s3.HeadObjectOutput, error)
	getFn  func(*s3.GetObjectInput) (*s3.GetObjectOutput, error)
	region string
}

var _ S3API = (*fakeS3)(nil)

func (f *fakeS3) HeadObject(_ context.Context, in *s3.HeadObjectInput, _ ...func(*s3.Options)) (*s3.HeadObjectOutput, error) {
	if f.headFn != nil {
		return f.headFn(in)
	}
	return &s3.HeadObjectOutput{}, nil
}

func (f *fakeS3) GetObject(_ context.Context, in *s3.GetObjectInput, _ ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	if f.getFn != nil {
		return f.getFn(in)
	}
	return &s3.GetObjectOutput{}, nil
}

func (f *fakeS3) Options() s3.Options { return s3.Options{Region: f.region} }

func newFakeS3Source(f *fakeS3) *S3Source {
	return &S3Source{client: f, bucket: "b", key: "k", uri: "s3://b/k"}
}

func respErr(status int, header http.Header) error {
	return &awshttp.ResponseError{
		ResponseError: &smithyhttp.ResponseError{
			Response: &smithyhttp.Response{Response: &http.Response{StatusCode: status, Header: header}},
			Err:      errors.New("redirect"),
		},
		RequestID: "req",
	}
}

func newS3SourceForTest(region string) *S3Source {
	return &S3Source{
		client: s3.New(s3.Options{Region: region}),
		bucket: "b",
		key:    "k",
		uri:    "s3://b/k",
	}
}

func TestCorrectRegion(t *testing.T) {
	t.Run("header present rebuilds client and returns true", func(t *testing.T) {
		s := newS3SourceForTest("us-east-1")
		if !s.correctRegion(respErr(301, http.Header{"X-Amz-Bucket-Region": {"us-west-2"}})) {
			t.Fatal("expected correctRegion to return true")
		}
		if got := s.client.Options().Region; got != "us-west-2" {
			t.Fatalf("client region = %q, want us-west-2", got)
		}
		if !s.regionFixed {
			t.Fatal("regionFixed should be set")
		}
		// One-shot guard: a second redirect must not swap again.
		if s.correctRegion(respErr(301, http.Header{"X-Amz-Bucket-Region": {"eu-west-1"}})) {
			t.Fatal("second correctRegion call must return false")
		}
		if s.client.Options().Region != "us-west-2" {
			t.Fatal("region must not change after the guard trips")
		}
	})

	t.Run("non-ResponseError returns false", func(t *testing.T) {
		s := newS3SourceForTest("us-east-1")
		if s.correctRegion(errors.New("plain")) {
			t.Fatal("plain error must not trigger a region swap")
		}
	})

	t.Run("missing region header returns false", func(t *testing.T) {
		s := newS3SourceForTest("us-east-1")
		if s.correctRegion(respErr(403, http.Header{})) {
			t.Fatal("missing x-amz-bucket-region must not trigger a swap")
		}
	})

	t.Run("same region is a no-op", func(t *testing.T) {
		s := newS3SourceForTest("us-west-2")
		if s.correctRegion(respErr(301, http.Header{"X-Amz-Bucket-Region": {"us-west-2"}})) {
			t.Fatal("same-region header must be a no-op")
		}
	})

	t.Run("nil response returns false", func(t *testing.T) {
		s := newS3SourceForTest("us-east-1")
		e := &awshttp.ResponseError{ResponseError: &smithyhttp.ResponseError{Err: errors.New("x")}}
		if s.correctRegion(e) {
			t.Fatal("nil response must not trigger a swap")
		}
	})
}

func TestS3SourceResolve(t *testing.T) {
	// SHA-256 of "abc"; S3 reports it base64-encoded, cob stores it as hex.
	wantHex := "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
	raw, _ := hex.DecodeString(wantHex)
	b64 := base64.StdEncoding.EncodeToString(raw)

	t.Run("size and checksum decoded", func(t *testing.T) {
		f := &fakeS3{headFn: func(in *s3.HeadObjectInput) (*s3.HeadObjectOutput, error) {
			if aws.ToString(in.Bucket) != "b" || aws.ToString(in.Key) != "k" {
				t.Fatalf("HeadObject got bucket=%q key=%q", aws.ToString(in.Bucket), aws.ToString(in.Key))
			}
			return &s3.HeadObjectOutput{ContentLength: aws.Int64(42), ChecksumSHA256: aws.String(b64)}, nil
		}}
		meta, err := newFakeS3Source(f).Resolve(context.Background())
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if meta.Size != 42 {
			t.Errorf("Size = %d, want 42", meta.Size)
		}
		if meta.SHA256 != wantHex {
			t.Errorf("SHA256 = %q, want %q (base64 must be decoded to hex)", meta.SHA256, wantHex)
		}
	})

	t.Run("no checksum metadata leaves SHA256 empty", func(t *testing.T) {
		f := &fakeS3{headFn: func(*s3.HeadObjectInput) (*s3.HeadObjectOutput, error) {
			return &s3.HeadObjectOutput{ContentLength: aws.Int64(7)}, nil
		}}
		meta, err := newFakeS3Source(f).Resolve(context.Background())
		if err != nil || meta.SHA256 != "" {
			t.Fatalf("Resolve = %+v, %v", meta, err)
		}
	})

	t.Run("HeadObject error surfaces", func(t *testing.T) {
		f := &fakeS3{headFn: func(*s3.HeadObjectInput) (*s3.HeadObjectOutput, error) {
			return nil, errors.New("access denied")
		}}
		if _, err := newFakeS3Source(f).Resolve(context.Background()); err == nil {
			t.Fatal("expected the HeadObject error to surface")
		}
	})
}

func TestS3SourceOpen(t *testing.T) {
	lastMod := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	f := &fakeS3{getFn: func(*s3.GetObjectInput) (*s3.GetObjectOutput, error) {
		return &s3.GetObjectOutput{
			Body:         io.NopCloser(strings.NewReader("payload")),
			ETag:         aws.String(`"etag123"`),
			VersionId:    aws.String("ver-9"),
			LastModified: &lastMod,
		}, nil
	}}
	s := newFakeS3Source(f)

	r, err := s.Open(context.Background())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	got, _ := io.ReadAll(r)
	r.Close()
	if string(got) != "payload" {
		t.Errorf("body = %q, want payload", got)
	}

	// Origin must reuse what Open captured from the same GetObject — a nil
	// headFn would yield an empty origin if Origin fell back to HeadObject.
	o, err := s.Origin(context.Background())
	if err != nil || o == nil {
		t.Fatalf("Origin = %+v, %v", o, err)
	}
	if o.ETag != "etag123" {
		t.Errorf("ETag = %q, want etag123 (quotes trimmed)", o.ETag)
	}
	if o.VersionID != "ver-9" || o.Versioned == nil || !*o.Versioned {
		t.Errorf("version not captured from GetObject: %+v", o)
	}
	if o.LastModified != "2026-05-01T12:00:00Z" {
		t.Errorf("LastModified = %q", o.LastModified)
	}
}

func TestS3SourceOriginViaHead(t *testing.T) {
	f := &fakeS3{headFn: func(*s3.HeadObjectInput) (*s3.HeadObjectOutput, error) {
		return &s3.HeadObjectOutput{ETag: aws.String(`"h-etag"`)}, nil
	}}
	o, err := newFakeS3Source(f).Origin(context.Background())
	if err != nil {
		t.Fatalf("Origin: %v", err)
	}
	if o.Type != "s3" || o.Bucket != "b" || o.Key != "k" || o.ETag != "h-etag" {
		t.Fatalf("Origin = %+v", o)
	}
	if o.Versioned == nil || *o.Versioned {
		t.Errorf("no version id -> Versioned must be false: %+v", o)
	}
}

func TestMakeOrigin(t *testing.T) {
	s := &S3Source{client: &fakeS3{region: "us-west-2"}, bucket: "b", key: "k"}

	t.Run("versioned with quoted etag", func(t *testing.T) {
		lastMod := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
		o := s.makeOrigin(`"abc123"`, "v-1", &lastMod)
		if o.ETag != "abc123" {
			t.Errorf("ETag = %q, want abc123 (quotes trimmed)", o.ETag)
		}
		if o.Versioned == nil || !*o.Versioned || o.VersionID != "v-1" {
			t.Errorf("versioned object not recorded: %+v", o)
		}
		if o.Region != "us-west-2" {
			t.Errorf("Region = %q, want us-west-2", o.Region)
		}
		if o.LastModified != "2026-01-02T03:04:05Z" {
			t.Errorf("LastModified = %q", o.LastModified)
		}
	})

	t.Run("empty and 'null' version ids are unversioned", func(t *testing.T) {
		for _, vid := range []string{"", "null"} {
			o := s.makeOrigin("etag", vid, nil)
			if o.Versioned == nil || *o.Versioned {
				t.Errorf("version id %q must be unversioned", vid)
			}
			if o.VersionID != "" {
				t.Errorf("version id %q: VersionID should stay empty, got %q", vid, o.VersionID)
			}
			if o.LastModified != "" {
				t.Errorf("nil lastMod -> empty LastModified, got %q", o.LastModified)
			}
		}
	})
}

// TestS3SourceConcurrentOpenOriginRaceFree drives Open and Origin from
// parallel goroutines on the same S3Source. Pre-mutex, the comment on
// the struct warned "not safe for concurrent use" and Open's write to
// s.origin would race with Origin's read under -race. The added mutex
// guards mutable state; this test fails the build (data race detected)
// if a future refactor drops the lock.
func TestS3SourceConcurrentOpenOriginRaceFree(t *testing.T) {
	fs := &fakeS3{
		region: "us-east-2",
		headFn: func(*s3.HeadObjectInput) (*s3.HeadObjectOutput, error) {
			return &s3.HeadObjectOutput{ETag: aws.String("\"head-etag\"")}, nil
		},
		getFn: func(*s3.GetObjectInput) (*s3.GetObjectOutput, error) {
			return &s3.GetObjectOutput{
				ETag: aws.String("\"get-etag\""),
				Body: io.NopCloser(strings.NewReader("payload")),
			}, nil
		},
	}
	src, err := NewS3Source(fs, "s3://b/k")
	if err != nil {
		t.Fatal(err)
	}
	const goroutines = 16
	done := make(chan struct{}, goroutines)
	for i := 0; i < goroutines; i++ {
		// Alternate Open and Origin to maximize the chance of the race
		// detector catching an unguarded read/write.
		if i%2 == 0 {
			go func() {
				rc, err := src.Open(context.Background())
				if err == nil {
					_, _ = io.Copy(io.Discard, rc)
					_ = rc.Close()
				}
				done <- struct{}{}
			}()
		} else {
			go func() {
				_, _ = src.Origin(context.Background())
				done <- struct{}{}
			}()
		}
	}
	for i := 0; i < goroutines; i++ {
		<-done
	}
	// Sanity: after the storm, origin is set (Open ran at least once)
	// and the etag is one of the two responses (whichever raced first).
	o, err := src.Origin(context.Background())
	if err != nil {
		t.Fatalf("Origin after storm: %v", err)
	}
	if o == nil || (o.ETag != "get-etag" && o.ETag != "head-etag") {
		t.Errorf("Origin = %+v; expected non-nil with one of the two response etags", o)
	}
}

// TestResolveRetainsChecksumModeOnRegionRedirect pins a fragile lexical
// invariant: in S3Source.Resolve, the *s3.HeadObjectInput (with
// ChecksumMode=Enabled) is built once and used by both the initial call
// and the post-region-redirect retry. A refactor that moves `in :=
// &s3.HeadObjectInput{...}` inside the conditional, or rebuilds the
// input for the retry, would silently drop ChecksumMode on the retry —
// and S3 only returns ChecksumSHA256 when ChecksumMode is set, so cob
// would fall back to "unverified" without anyone noticing.
//
// We override rebuildS3Client (the package-level seam in source_s3.go)
// so the post-redirect retry stays on our fake; otherwise the production
// code path would build a real s3.Client for the second call and try to
// hit real S3.
func TestResolveRetainsChecksumModeOnRegionRedirect(t *testing.T) {
	// Real SHA-256 of "hello" base64'd is what HeadObject would return.
	sum := sha256.Sum256([]byte("hello"))
	wantHex := hex.EncodeToString(sum[:])
	wantB64 := base64.StdEncoding.EncodeToString(sum[:])

	var calls []*s3.HeadObjectInput
	f := &fakeS3{
		region: "us-east-1",
		headFn: func(in *s3.HeadObjectInput) (*s3.HeadObjectOutput, error) {
			// Capture a snapshot — Go's struct values are copied here,
			// so a refactor that mutates `in` between calls can't fool us.
			snapshot := *in
			calls = append(calls, &snapshot)
			if len(calls) == 1 {
				// First call: a cross-region redirect carrying the bucket's real region.
				return nil, respErr(301, http.Header{"X-Amz-Bucket-Region": {"us-west-2"}})
			}
			// Second call (post-redirect): real response with the SHA-256
			// S3 only returns when ChecksumMode is enabled.
			return &s3.HeadObjectOutput{
				ContentLength:  aws.Int64(5),
				ChecksumSHA256: aws.String(wantB64),
			}, nil
		},
	}

	// Keep the post-redirect retry on the same fake instead of letting
	// correctRegion build a real *s3.Client.
	orig := rebuildS3Client
	rebuildS3Client = func(opts s3.Options) S3API {
		f.region = opts.Region // reflect the swap so f.Options() reports it
		return f
	}
	t.Cleanup(func() { rebuildS3Client = orig })

	src := newFakeS3Source(f)
	meta, err := src.Resolve(context.Background())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(calls) != 2 {
		t.Fatalf("expected exactly 2 HeadObject calls (initial + retry), got %d", len(calls))
	}

	// First call must have ChecksumMode — that's the documented contract,
	// and a regression there would also break the no-redirect case.
	if calls[0].ChecksumMode != s3types.ChecksumModeEnabled {
		t.Errorf("first HeadObject: ChecksumMode = %v, want Enabled", calls[0].ChecksumMode)
	}
	// Second call MUST also have ChecksumMode — this is the invariant
	// the test exists to pin. If someone refactors Resolve to build a
	// fresh input for the retry (or moves construction inside the
	// conditional) without copying ChecksumMode, this fires.
	if calls[1].ChecksumMode != s3types.ChecksumModeEnabled {
		t.Errorf("retry HeadObject after region redirect: ChecksumMode = %v, want Enabled — the captured-in-scope input invariant in Resolve is broken; the retry must use the same input value as the initial call", calls[1].ChecksumMode)
	}

	// And the user-visible consequence: AssetMetadata.SHA256 round-trips.
	// If ChecksumMode were dropped on the retry, S3 wouldn't return
	// ChecksumSHA256 and meta.SHA256 would be empty — silent regression
	// to "unverified" for every cross-region bucket.
	if meta.SHA256 != wantHex {
		t.Errorf("meta.SHA256 = %q, want %q (ChecksumSHA256 must round-trip via the retry too)", meta.SHA256, wantHex)
	}
}
