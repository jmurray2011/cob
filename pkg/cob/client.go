package cob

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/codeartifact"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// Client wraps AWS SDK clients for S3 and CodeArtifact.
type Client struct {
	S3           *s3.Client
	CodeArtifact *codeartifact.Client
	cfg          aws.Config
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
		cfg:          cfg,
	}, nil
}
