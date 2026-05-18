package cob

import (
	"errors"
	"net/http"
	"testing"

	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

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
