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
	t.Run("URL redact が chain を保つ", func(t *testing.T) {
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
