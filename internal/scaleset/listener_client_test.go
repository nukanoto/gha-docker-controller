package scaleset

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	scalesetapi "github.com/actions/scaleset"
)

// TestListenerClient_RedactsSessionErrors protects URL redaction and errors.Is.
func TestListenerClient_RedactsSessionErrors(t *testing.T) {
	secret := "sessionToken=eyJhbGciOiJIUzI1NiJ9.secret"
	officialWithURL := func(cause error) error {
		return fmt.Errorf(
			"request GET https://pipelines.actions.githubusercontent.com/abc/_apis/runtime/runnerscalesets/1/messages?%s failed: failed to send request: %w",
			secret, cause)
	}
	t.Run("URL redaction preserves the chain", func(t *testing.T) {
		top := fmt.Errorf("failed to get message: %w", redactSessionError(officialWithURL(context.Canceled)))
		msg := top.Error()
		if strings.Contains(msg, secret) || strings.Contains(msg, "https://") {
			t.Fatalf("top-level error still contains the URL or session token")
		}
		if !strings.Contains(msg, "<redacted>") {
			t.Fatalf("URL was not replaced with <redacted>")
		}
		if !errors.Is(top, context.Canceled) {
			t.Fatalf("context.Canceled was lost from the error chain")
		}
	})
	t.Run("token expiry sentinel is preserved", func(t *testing.T) {
		top := fmt.Errorf("failed to delete message: %w",
			redactSessionError(officialWithURL(scalesetapi.MessageQueueTokenExpiredError)))
		msg := top.Error()
		if strings.Contains(msg, secret) || strings.Contains(msg, "https://") {
			t.Fatalf("top-level error still contains the URL or session token")
		}
		if !errors.Is(top, scalesetapi.MessageQueueTokenExpiredError) {
			t.Fatalf("MessageQueueTokenExpiredError was lost from the error chain")
		}
	})
	t.Run("error without a URL is unchanged", func(t *testing.T) {
		cause := context.Canceled
		wrapped := fmt.Errorf("failed to send request: %w", cause)
		if got := redactSessionError(wrapped); got != wrapped {
			t.Fatalf("error without a URL was modified")
		}
		if got := redactSessionError(nil); got != nil {
			t.Fatalf("nil error did not remain nil")
		}
	})
}
