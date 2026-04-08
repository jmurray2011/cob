package cob

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// S3Source reads an asset from an S3 object.
type S3Source struct {
	client *s3.Client
	bucket string
	key    string
	uri    string

	// Populated by Resolve.
	meta *AssetMetadata
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

func (s *S3Source) Resolve(ctx context.Context) (*AssetMetadata, error) {
	head, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.key),
	})
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

	s.meta = meta
	return meta, nil
}

func (s *S3Source) Open(ctx context.Context) (io.ReadCloser, error) {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.key),
	})
	if err != nil {
		return nil, fmt.Errorf("GetObject %s: %w", s.uri, err)
	}
	return out.Body, nil
}
