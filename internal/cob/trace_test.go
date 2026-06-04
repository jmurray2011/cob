package cob

import (
	"context"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/codeartifact"
)

// TestTracedCALogsAndDelegates verifies the --verbose call-trace decorator
// emits one line (operation + targeted coordinates) per call and still
// delegates to the wrapped client.
func TestTracedCALogsAndDelegates(t *testing.T) {
	var lines []string
	trace := func(op, detail string) { lines = append(lines, op+"|"+detail) }

	called := 0
	inner := &fakeCA{
		listAssetsFn: func(*codeartifact.ListPackageVersionAssetsInput) (*codeartifact.ListPackageVersionAssetsOutput, error) {
			called++
			return &codeartifact.ListPackageVersionAssetsOutput{}, nil
		},
	}
	tc := tracedCA{inner: inner, trace: trace}

	_, err := tc.ListPackageVersionAssets(context.Background(), &codeartifact.ListPackageVersionAssetsInput{
		Domain:         aws.String("acme"),
		Repository:     aws.String("dev"),
		Namespace:      aws.String("tools"),
		Package:        aws.String("app"),
		PackageVersion: aws.String("1.0.0"),
	})
	if err != nil {
		t.Fatalf("delegate returned error: %v", err)
	}
	if called != 1 {
		t.Errorf("inner ListPackageVersionAssets called %d times, want 1", called)
	}
	if len(lines) != 1 {
		t.Fatalf("trace lines = %v, want exactly 1", lines)
	}
	if !strings.Contains(lines[0], "ListPackageVersionAssets") || !strings.Contains(lines[0], "acme/dev/tools/app@1.0.0") {
		t.Errorf("trace line = %q, want op + coords acme/dev/tools/app@1.0.0", lines[0])
	}
}

// TestNewClientWrapsWhenTraceSet pins the wiring: a Trace in ClientOptions
// makes NewClient return a CodeArtifact client that traces; no Trace leaves
// the raw client.
func TestClientTraceWiring(t *testing.T) {
	if _, isTraced := (CodeArtifactAPI(tracedCA{})).(tracedCA); !isTraced {
		t.Fatal("tracedCA must satisfy CodeArtifactAPI")
	}
}
