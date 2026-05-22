package cob

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/codeartifact"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

// STSAPI is the subset of the STS client cob uses (caller identity for
// provenance). The concrete SDK client satisfies it.
type STSAPI interface {
	GetCallerIdentity(context.Context, *sts.GetCallerIdentityInput, ...func(*sts.Options)) (*sts.GetCallerIdentityOutput, error)
}

var _ STSAPI = (*sts.Client)(nil)

// S3API is the subset of *s3.Client the S3 asset source uses. Options is
// included so the source can rebuild itself pinned to a bucket's real region
// after a cross-region redirect (see S3Source.correctRegion); the rebuilt
// *s3.Client satisfies this interface in turn. Depending on the interface
// lets the S3 source be unit-tested with an in-memory fake.
type S3API interface {
	HeadObject(context.Context, *s3.HeadObjectInput, ...func(*s3.Options)) (*s3.HeadObjectOutput, error)
	GetObject(context.Context, *s3.GetObjectInput, ...func(*s3.Options)) (*s3.GetObjectOutput, error)
	Options() s3.Options
}

var _ S3API = (*s3.Client)(nil)

// Client wraps AWS SDK clients for S3, CodeArtifact, and STS.
type Client struct {
	S3           S3API
	CodeArtifact CodeArtifactAPI
	STS          STSAPI
	Region       string
}

// ClientOptions configures how the AWS client is created.
type ClientOptions struct {
	Profile string
	Region  string
	// Debug enables AWS SDK request/response logging to stderr.
	Debug bool
}

// NewClient creates a Client using the standard credential chain.
func NewClient(ctx context.Context, opts ClientOptions) (*Client, error) {
	var cfgOpts []func(*config.LoadOptions) error

	if opts.Profile != "" {
		cfgOpts = append(cfgOpts, config.WithSharedConfigProfile(opts.Profile))
	}
	if opts.Region != "" {
		cfgOpts = append(cfgOpts, config.WithRegion(opts.Region))
	}
	if opts.Debug {
		// Response status lines and retry attempts to stderr — enough to
		// diagnose region/credential/throttling failures. Request logging is
		// deliberately excluded: a signed AWS request carries a live
		// X-Amz-Security-Token header (SSO / instance / ECS-role
		// credentials), and --debug output routinely lands in CI logs.
		cfgOpts = append(cfgOpts, config.WithClientLogMode(aws.LogResponse|aws.LogRetries))
	}

	cfg, err := config.LoadDefaultConfig(ctx, cfgOpts...)
	if err != nil {
		return nil, fmt.Errorf("loading AWS config: %w", err)
	}

	return &Client{
		S3:           s3.NewFromConfig(cfg),
		CodeArtifact: codeartifact.NewFromConfig(cfg),
		STS:          sts.NewFromConfig(cfg),
		Region:       cfg.Region,
	}, nil
}

// CallerIdentity returns the AWS principal recorded in provenance. It is
// best-effort: identity is evidence, not correctness, so a failure yields a
// zero Actor rather than blocking a publish/promote.
func (c *Client) CallerIdentity(ctx context.Context) Actor {
	if c.STS == nil {
		return Actor{}
	}
	out, err := c.STS.GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return Actor{}
	}
	return Actor{
		Account: aws.ToString(out.Account),
		ARN:     aws.ToString(out.Arn),
		UserID:  aws.ToString(out.UserId),
	}
}
