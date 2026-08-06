package scaleset

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	scalesetapi "github.com/actions/scaleset"
)

// TestListenerClient_RedactsSessionErrors verifies that the URL in an
// official error chain is removed by the adapter-boundary redaction and that
// error chain classification is unchanged from before redaction. The official
// client's GetMessage/DeleteMessage wrap with "failed to get next message:"
// style formats, and the inner newRequestResponseError embeds the message
// queue URL (with the session token in its query). The adapter handles this
// error with redactSessionError, and the official listener wraps it further
// with %w. redactedError.Unwrap keeps the chain, so context.Canceled and
// MessageQueueTokenExpiredError classification are confirmed unchanged.
func TestListenerClient_RedactsSessionErrors(t *testing.T) {
	secret := "sessionToken=eyJhbGciOiJIUzI1NiJ9.secret"
	// Build an error equivalent to the official newRequestResponseError.
	officialWithURL := func(cause error) error {
		return fmt.Errorf(
			"request GET https://pipelines.actions.githubusercontent.com/abc/_apis/runtime/runnerscalesets/1/messages?%s failed: failed to send request: %w",
			secret, cause)
	}
	t.Run("URL redact が chain を保つ", func(t *testing.T) {
		// Adapter boundary: redact the official MessageSessionClient error and
		// wrap with %w as the official listener's Run does.
		top := fmt.Errorf("failed to get message: %w", redactSessionError(officialWithURL(context.Canceled)))
		msg := top.Error()
		if strings.Contains(msg, secret) || strings.Contains(msg, "https://") {
			t.Fatalf("最上位 error に URL / session token が残っています")
		}
		if !strings.Contains(msg, "<redacted>") {
			t.Fatalf("URL が <redacted> へ置換されていません")
		}
		if !errors.Is(top, context.Canceled) {
			t.Fatalf("error chain の context.Canceled が失われました")
		}
	})
	t.Run("token 失効 sentinel が保たれる", func(t *testing.T) {
		// The official listener returns this classification unchanged; the
		// caller must be able to detect token expiry via errors.Is.
		top := fmt.Errorf("failed to delete message: %w",
			redactSessionError(officialWithURL(scalesetapi.MessageQueueTokenExpiredError)))
		msg := top.Error()
		if strings.Contains(msg, secret) || strings.Contains(msg, "https://") {
			t.Fatalf("最上位 error に URL / session token が残っています")
		}
		if !errors.Is(top, scalesetapi.MessageQueueTokenExpiredError) {
			t.Fatalf("error chain の MessageQueueTokenExpiredError が失われました")
		}
	})
	t.Run("URL を含まない error は元のまま", func(t *testing.T) {
		// Do not rewrite the chain pointlessly; nil stays nil.
		cause := context.Canceled
		wrapped := fmt.Errorf("failed to send request: %w", cause)
		if got := redactSessionError(wrapped); got != wrapped {
			t.Fatalf("URL を含まない error が書き換えられました")
		}
		if got := redactSessionError(nil); got != nil {
			t.Fatalf("nil error が nil になりません")
		}
	})
}
