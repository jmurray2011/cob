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

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// S3Source reads an asset from an S3 object.
type S3Source struct {
	client *s3.Client
	bucket string
	key    string
	uri    string

	// regionFixed guards the one-time client rebuild in correctRegion.
	regionFixed bool
}

// NewS3Source creates an S3Source from a URI like s3://bucket/key.
func NewS3Source(client *s3.Client, uri string) (*S3Source, error) {
	trimmed := strings.TrimPrefix(uri, "s3://")
	slash := strings.IndexByte(trimmed, '/')
	if slash < 0 {
		return nil, fmt.Errorf("invalid S3 URI %q: missing key", uri)
	}
	return &S3Source{
		client: client,
		bucket: trimmed[:slash],
		key:    trimmed[slash+1:],
		uri:    uri,
	}, nil
}

func (s *S3Source) URI() string { return s.uri }

func (s *S3Source) Filename() string { return path.Base(s.key) }

// correctRegion inspects err for an S3 cross-region redirect. A request sent
// to the wrong regional endpoint comes back as a 301/400 that still carries
// the bucket's real region in the x-amz-bucket-region header. When that
// header is present we rebuild the client pinned to that region — reusing
// the same credentials and HTTP options — and return true so the caller can
// retry once. This happens at most once per source; in the common
// same-region case it is never triggered and adds no overhead.
func (s *S3Source) correctRegion(err error) bool {
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
	head, err := s.client.HeadObject(ctx, in)
	if err != nil && s.correctRegion(err) {
		head, err = s.client.HeadObject(ctx, in)
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
	out, err := s.client.GetObject(ctx, in)
	if err != nil && s.correctRegion(err) {
		out, err = s.client.GetObject(ctx, in)
	}
	if err != nil {
		return nil, fmt.Errorf("GetObject %s: %w", s.uri, err)
	}
	return out.Body, nil
}
