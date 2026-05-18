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

// Client wraps AWS SDK clients for S3, CodeArtifact, and STS.
type Client struct {
	S3           *s3.Client
	CodeArtifact CodeArtifactAPI
	STS          STSAPI
	Region       string
}

// ClientOptions configures how the AWS client is created.
type ClientOptions struct {
	Profile string
	Region  string
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
