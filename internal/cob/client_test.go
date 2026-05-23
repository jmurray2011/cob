package cob

import (
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
)

// TestDebugLogModeExcludesRequests pins the --debug log mode. A signed
// AWS request carries an X-Amz-Security-Token header containing live
// session credentials (SSO, instance role, ECS task role); --debug output
// routinely lands in CI logs, so we must never enable request logging.
// A regression that flips aws.LogRequest back on fails the build here
// rather than quietly leaking credentials into a pipeline somewhere.
func TestDebugLogModeExcludesRequests(t *testing.T) {
	if debugClientLogMode&aws.LogRequest != 0 {
		t.Fatal("--debug must NOT enable aws.LogRequest: signed requests carry live X-Amz-Security-Token headers; --debug output lands in CI logs")
	}
	if debugClientLogMode&aws.LogRequestWithBody != 0 {
		t.Fatal("--debug must NOT enable aws.LogRequestWithBody for the same reason")
	}
	// Positive: pin what --debug *should* include, so a future refactor
	// that drops Response or Retries from the mask doesn't quietly
	// regress operator diagnostics either.
	if debugClientLogMode&aws.LogResponse == 0 {
		t.Error("debugClientLogMode should include aws.LogResponse for diagnostics")
	}
	if debugClientLogMode&aws.LogRetries == 0 {
		t.Error("debugClientLogMode should include aws.LogRetries for diagnostics")
	}
}
