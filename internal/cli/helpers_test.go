package cli

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sts"

	"github.com/jmurray2011/cob/internal/cob"
)

// fakeSTS implements cob.STSAPI with a hand-rolled GetCallerIdentity
// response. Used to verify that fillClientMeta records the executing
// principal — and that the cache shape on Client returns one identity
// across multiple calls.
type fakeSTS struct {
	calls    int
	identity *sts.GetCallerIdentityOutput
	err      error
}

func (f *fakeSTS) GetCallerIdentity(_ context.Context, _ *sts.GetCallerIdentityInput, _ ...func(*sts.Options)) (*sts.GetCallerIdentityOutput, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.identity, nil
}

func TestFillClientMetaPopulatesRegionAndActor(t *testing.T) {
	sts := &fakeSTS{identity: &sts.GetCallerIdentityOutput{
		Account: aws.String("111122223333"),
		Arn:     aws.String("arn:aws:iam::111122223333:user/alice"),
		UserId:  aws.String("AIDAEXAMPLE"),
	}}
	client := &cob.Client{Region: "us-west-2", STS: sts}
	var result cob.CommandResult
	fillClientMeta(context.Background(), client, &result)

	if result.Region != "us-west-2" {
		t.Errorf("Region = %q, want us-west-2", result.Region)
	}
	if result.Actor == nil {
		t.Fatal("Actor was not recorded")
	}
	if result.Actor.ARN != "arn:aws:iam::111122223333:user/alice" {
		t.Errorf("Actor.ARN = %q", result.Actor.ARN)
	}
	if result.Actor.Account != "111122223333" {
		t.Errorf("Actor.Account = %q", result.Actor.Account)
	}
}

func TestFillClientMetaCachesIdentityAcrossCalls(t *testing.T) {
	// The CommandResult stamp + provenance write each call CallerIdentity;
	// the Client must cache so multiple stamps in one process don't each
	// fire an STS round-trip.
	stsFake := &fakeSTS{identity: &sts.GetCallerIdentityOutput{
		Account: aws.String("111122223333"),
		Arn:     aws.String("arn:aws:iam::111122223333:role/runner"),
		UserId:  aws.String("AROAEXAMPLE"),
	}}
	client := &cob.Client{Region: "us-east-2", STS: stsFake}
	var a, b cob.CommandResult
	fillClientMeta(context.Background(), client, &a)
	fillClientMeta(context.Background(), client, &b)
	_ = client.CallerIdentity(context.Background()) // a third caller
	if stsFake.calls != 1 {
		t.Errorf("STS GetCallerIdentity called %d times, want 1 (cached)", stsFake.calls)
	}
	if a.Actor == nil || b.Actor == nil || a.Actor.ARN != b.Actor.ARN {
		t.Errorf("identity should be stable across calls: %+v vs %+v", a.Actor, b.Actor)
	}
}

func TestFillClientMetaFailedSTSLeavesActorNil(t *testing.T) {
	// Identity is evidence, not correctness — an STS error must not block,
	// just leave Actor unset so consumers render "unknown" without confusing
	// it for "some real identity that happens to have empty fields".
	client := &cob.Client{Region: "us-east-2", STS: &fakeSTS{err: errBoom{}}}
	var result cob.CommandResult
	fillClientMeta(context.Background(), client, &result)
	if result.Region != "us-east-2" {
		t.Errorf("Region should still be set despite STS failure")
	}
	if result.Actor != nil {
		t.Errorf("Actor should be nil on STS failure; got %+v", result.Actor)
	}
}

func TestFillClientMetaNilClientIsNoOp(t *testing.T) {
	// validate has no client at all (offline). Passing nil must not panic
	// and must leave the result untouched.
	var result cob.CommandResult
	fillClientMeta(context.Background(), nil, &result)
	if result.Region != "" || result.Actor != nil {
		t.Errorf("nil client should be a no-op, got %+v", result)
	}
}

type errBoom struct{}

func (errBoom) Error() string { return "sts boom" }
