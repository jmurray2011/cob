package cob

import (
	"context"
	"fmt"
	"os"
	"sync"

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
	// TmpDir is where publish/promote spill assets while streaming ("" = the
	// OS default temp dir). Point it at real disk if $TMPDIR is RAM-backed.
	TmpDir string

	// actorOnce + actor cache the STS caller identity so commands that
	// fetch it more than once per run (provenance write + CommandResult
	// audit field) pay for one round-trip, not two.
	actorOnce sync.Once
	actor     Actor
}

// debugClientLogMode is what --debug enables on the AWS SDK: response
// status lines and retry attempts to stderr — enough to diagnose
// region / credential / throttling failures.
//
// Request logging (aws.LogRequest / aws.LogRequestWithBody) is
// deliberately excluded: a signed AWS request carries a live
// X-Amz-Security-Token header (SSO, instance role, ECS task role), and
// --debug output routinely lands in CI logs. Pinned with a unit test
// (TestDebugLogModeExcludesRequests) so a careless refactor that flips
// LogRequest back on fails the build instead of silently leaking
// credentials.
const debugClientLogMode = aws.LogResponse | aws.LogRetries

// ClientOptions configures how the AWS client is created.
type ClientOptions struct {
	Profile string
	Region  string
	// Debug enables AWS SDK request/response logging to stderr.
	Debug bool
	// TmpDir overrides the asset spill directory (see Client.TmpDir).
	TmpDir string
	// Trace, when non-nil, receives one entry per CodeArtifact/S3 call the
	// resulting client makes (operation + targeted coordinates). Wired to
	// --verbose. Distinct from Debug: Debug is the SDK's own response/retry
	// logging; Trace is cob's coarse "which calls did this command issue".
	Trace TraceFunc
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
		cfgOpts = append(cfgOpts, config.WithClientLogMode(debugClientLogMode))
	}

	cfg, err := config.LoadDefaultConfig(ctx, cfgOpts...)
	if err != nil {
		return nil, fmt.Errorf("loading AWS config: %w", err)
	}

	if opts.TmpDir != "" {
		// 0o700 — files inside are os.CreateTemp's default 0o600 so contents
		// are already private, but on a shared host the dir itself leaks
		// operational metadata (how often cob runs, when, against which
		// versions) to any local user who can ls it. Match ~/.aws/.
		if err := os.MkdirAll(opts.TmpDir, 0o700); err != nil {
			return nil, fmt.Errorf("creating temp directory %s: %w", opts.TmpDir, err)
		}
	}

	var s3API S3API = s3.NewFromConfig(cfg)
	var caAPI CodeArtifactAPI = codeartifact.NewFromConfig(cfg)
	if opts.Trace != nil {
		// Wrap the read/transfer clients so --verbose can trace each call.
		// (A cross-region S3 redirect rebuilds its client via rebuildS3Client
		// and drops the wrapper, so calls after a redirect aren't traced —
		// an acceptable gap for a coarse trace.)
		s3API = tracedS3{inner: s3API, trace: opts.Trace}
		caAPI = tracedCA{inner: caAPI, trace: opts.Trace}
	}

	return &Client{
		S3:           s3API,
		CodeArtifact: caAPI,
		STS:          sts.NewFromConfig(cfg),
		Region:       cfg.Region,
		TmpDir:       opts.TmpDir,
	}, nil
}

// CallerIdentity returns the AWS principal recorded in provenance and on
// CommandResult.Actor. It is best-effort: identity is evidence, not
// correctness, so a failure yields a zero Actor rather than blocking a
// publish/promote. Cached for the life of the Client — once per process
// — so repeated calls don't each pay an STS round-trip.
func (c *Client) CallerIdentity(ctx context.Context) Actor {
	c.actorOnce.Do(func() {
		if c.STS == nil {
			return
		}
		out, err := c.STS.GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
		if err != nil {
			return
		}
		c.actor = Actor{
			Account: aws.ToString(out.Account),
			ARN:     aws.ToString(out.Arn),
			UserID:  aws.ToString(out.UserId),
		}
	})
	return c.actor
}
