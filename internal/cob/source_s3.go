package cob

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// S3Source reads an asset from an S3 object. Safe for concurrent use:
// the embedded sync.Mutex guards the three pieces of mutable state
// (client rebuild on a region redirect, the regionFixed flag, the
// captured origin). cob's transfer workers still own one source per
// goroutine in production — the locking is belt-and-braces so a
// refactor that adds a parallel call can't silently race.
type S3Source struct {
	bucket string
	key    string
	uri    string

	// mu guards client, regionFixed, and origin. Held only for the
	// brief mutation; network calls (HeadObject/GetObject) run outside
	// the lock with a local client snapshot, so a slow request never
	// stalls a parallel one.
	mu          sync.Mutex
	client      S3API
	regionFixed bool
	// origin is captured from the GetObject in Open so the recorded
	// etag/version_id correspond to exactly the bytes that were published
	// (no TOCTOU window from a later, separate HeadObject). Origin
	// reuses it when present; otherwise it does its own HeadObject.
	origin *Origin
}

// NewS3Source creates an S3Source from a URI like s3://bucket/key.
func NewS3Source(client S3API, uri string) (*S3Source, error) {
	trimmed := strings.TrimPrefix(uri, "s3://")
	slash := strings.IndexByte(trimmed, '/')
	if slash < 0 {
		return nil, fmt.Errorf("invalid S3 URI %q: missing key", uri)
	}
	bucket, key := trimmed[:slash], trimmed[slash+1:]
	if bucket == "" {
		return nil, fmt.Errorf("invalid S3 URI %q: empty bucket", uri)
	}
	if key == "" {
		return nil, fmt.Errorf("invalid S3 URI %q: empty key", uri)
	}
	return &S3Source{client: client, bucket: bucket, key: key, uri: uri}, nil
}

func (s *S3Source) URI() string { return s.uri }

func (s *S3Source) Filename() string { return path.Base(s.key) }

// currentClient returns the live client snapshot under lock — any caller
// about to issue a network request takes this once, then runs the request
// outside the lock. correctRegion may swap the client in place after a
// cross-region redirect; the snapshot pattern means a slow in-flight
// HeadObject can't observe a half-swapped client.
func (s *S3Source) currentClient() S3API {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.client
}

// correctRegion inspects err for an S3 cross-region redirect. A request sent
// to the wrong regional endpoint comes back as a 301/400 that still carries
// the bucket's real region in the x-amz-bucket-region header. When that
// header is present we rebuild the client pinned to that region — reusing
// the same credentials and HTTP options — and return true so the caller can
// retry once. This happens at most once per source; in the common
// same-region case it is never triggered and adds no overhead.
func (s *S3Source) correctRegion(err error) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.regionFixed {
		return false
	}
	var re *awshttp.ResponseError
	if !errors.As(err, &re) || re.Response == nil {
		return false
	}
	region := re.Response.Header.Get("X-Amz-Bucket-Region")
	if region == "" || region == s.client.Options().Region {
		return false
	}
	opts := s.client.Options()
	opts.Region = region
	s.client = s3.New(opts)
	s.regionFixed = true
	return true
}

func (s *S3Source) Resolve(ctx context.Context) (*AssetMetadata, error) {
	in := &s3.HeadObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.key),
		// Without this, S3 omits ChecksumSHA256 from the response even when
		// the object was uploaded with --checksum-algorithm SHA256, so cob
		// would never see the stored hash (verify/diff fall back to
		// "unverified" and publish always re-hashes).
		ChecksumMode: s3types.ChecksumModeEnabled,
	}
	head, err := s.currentClient().HeadObject(ctx, in)
	if err != nil && s.correctRegion(err) {
		head, err = s.currentClient().HeadObject(ctx, in)
	}
	if err != nil {
		return nil, fmt.Errorf("HeadObject %s: %w", s.uri, err)
	}

	meta := &AssetMetadata{
		Size: aws.ToInt64(head.ContentLength),
	}

	// Check for SHA-256 checksum in S3 metadata.
	if head.ChecksumSHA256 != nil && *head.ChecksumSHA256 != "" {
		raw, err := base64.StdEncoding.DecodeString(*head.ChecksumSHA256)
		if err != nil {
			return nil, fmt.Errorf("decoding S3 SHA-256 for %s: %w", s.uri, err)
		}
		meta.SHA256 = hex.EncodeToString(raw)
	}

	return meta, nil
}

func (s *S3Source) Open(ctx context.Context) (io.ReadCloser, error) {
	in := &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.key),
	}
	out, err := s.currentClient().GetObject(ctx, in)
	if err != nil && s.correctRegion(err) {
		out, err = s.currentClient().GetObject(ctx, in)
	}
	if err != nil {
		return nil, fmt.Errorf("GetObject %s: %w", s.uri, err)
	}
	// Capture origin from the same response that yields the bytes, so the
	// recorded etag/version_id match exactly what gets published.
	o := s.makeOrigin(aws.ToString(out.ETag), aws.ToString(out.VersionId), out.LastModified)
	s.mu.Lock()
	s.origin = o
	s.mu.Unlock()
	return out.Body, nil
}

// makeOrigin builds an s3 Origin from object metadata. Reads s.client
// under lock for the region stamp; callers should not assume the build
// is atomic relative to a concurrent correctRegion (the region recorded
// on Origin is best-effort metadata, not part of the integrity story).
func (s *S3Source) makeOrigin(etag, versionID string, lastMod *time.Time) *Origin {
	versioned := versionID != "" && versionID != "null"
	o := &Origin{
		Type:      "s3",
		Bucket:    s.bucket,
		Key:       s.key,
		ETag:      strings.Trim(etag, `"`),
		Region:    s.currentClient().Options().Region,
		Versioned: &versioned,
	}
	if versioned {
		o.VersionID = versionID
	}
	if lastMod != nil {
		o.LastModified = lastMod.UTC().Format(time.RFC3339)
	}
	return o
}

func (s *S3Source) Origin(ctx context.Context) (*Origin, error) {
	// If Open already ran, reuse the origin captured from that GetObject —
	// it matches the bytes actually downloaded (no TOCTOU window).
	s.mu.Lock()
	cached := s.origin
	s.mu.Unlock()
	if cached != nil {
		return cached, nil
	}
	in := &s3.HeadObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(s.key)}
	head, err := s.currentClient().HeadObject(ctx, in)
	if err != nil && s.correctRegion(err) {
		head, err = s.currentClient().HeadObject(ctx, in)
	}
	if err != nil {
		return nil, fmt.Errorf("HeadObject %s: %w", s.uri, err)
	}
	return s.makeOrigin(aws.ToString(head.ETag), aws.ToString(head.VersionId), head.LastModified), nil
}
