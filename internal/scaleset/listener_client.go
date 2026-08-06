// listener_client.go implements a transparent session adapter that is passed
// directly to the official listener (github.com/actions/scaleset/listener).
// The official Listener acknowledges via DeleteMessage right after reading the
// message statistics, before the event handler (ack-before-processing). This
// adapter accepts that official order as-is and has no custom poll, ack-order
// change, retry, message conversion, or tracker. It also accepts the official
// listener's failure model: client method errors are wrapped by listener.Run
// which stops, and classification or retry is left to the official
// MessageSessionClient internals (token refresh, retryablehttp).
// GetMessage/DeleteMessage/AcquireJobs/Session/Close delegate directly to the
// official MessageSessionClient.
// Official client errors can include the message queue URL (which carries the
// session token in its query), so redactSessionError at the wrapper boundary
// replaces the whole URL with <redacted>. redactedError.Unwrap keeps the error
// chain, so classification via errors.Is / errors.As (context.Canceled,
// MessageQueueTokenExpiredError, and so on) is unchanged from before redaction.
package scaleset

import (
	"context"
	"fmt"
	"regexp"

	scalesetapi "github.com/actions/scaleset"
	listenerapi "github.com/actions/scaleset/listener"
)

// messageURLPattern is the regexp that finds the URL in official client
// errors. The official client embeds req.URL.String() into the error body via
// newRequestResponseError. GitHub's messageQueueUrl carries the session token
// in its query, so returning it as-is would expose the credential in logs and
// errors. The URL is not needed for operators to diagnose the cause and is
// always replaced with <redacted>.
var messageURLPattern = regexp.MustCompile(`https?://\S+`)

// redactedError is an error whose Error() string is redacted. Unwrap keeps the
// original error chain, so classification via errors.Is / errors.As
// (context.Canceled, MessageQueueTokenExpiredError, net.Error, and so on) is
// unchanged from before redaction. Callers always log only the top-level
// error's Error(), and there is no path that formats the tail of the chain
// directly.
type redactedError struct {
	msg   string
	cause error
}

// Error returns the redacted error text.
func (e *redactedError) Error() string { return e.msg }

// Unwrap preserves error classification without changing the displayed text.
func (e *redactedError) Unwrap() error { return e.cause }

// redactSessionError removes the URL from an official message session error.
// Errors without a URL are returned unchanged (the chain is kept). nil stays nil.
func redactSessionError(err error) error {
	if err == nil {
		return nil
	}
	msg := messageURLPattern.ReplaceAllString(err.Error(), "<redacted>")
	if msg == err.Error() {
		return err
	}
	return &redactedError{msg: msg, cause: err}
}

// ListenerClient is a transparent adapter that satisfies the official
// listener.Client interface. It holds the official MessageSessionClient and
// delegates each method as-is. Official listener.Run validates the initial
// session (SessionID, Statistics) itself, so this adapter does not duplicate
// validation. No nil-receiver guard: NewListenerClient is the only creation
// point, and the official listener also rejects a nil client in New.
type ListenerClient struct {
	// official is the official MessageSessionClient. Auto-refresh of the
	// session token and internal-mutex concurrency safety are provided by
	// the official client.
	official *scalesetapi.MessageSessionClient
}

// listenerClientInterfaceAssertion guarantees at compile time that
// ListenerClient satisfies the official listener.Client. The assignment is
// never executed.
var _ listenerapi.Client = (*ListenerClient)(nil)

// NewListenerClient starts exactly one message session for the given Scale Set
// and returns the ListenerClient to pass to the official listener. owner is
// the github.owner of the organization/repository. A nil session returned by
// the official client without an error is protocol-fatal.
func (c *Client) NewListenerClient(ctx context.Context, scaleSetID int, owner string) (*ListenerClient, error) {
	official, err := c.official.MessageSessionClient(ctx, scaleSetID, owner)
	if err != nil {
		// The official error can include the session URL, so redact it first.
		return nil, fmt.Errorf("create message session for scale set %d: %w", scaleSetID, redactSessionError(err))
	}
	if official == nil {
		return nil, protocolErrorf("create message session", "official session client is nil for scale set %d", scaleSetID)
	}
	return &ListenerClient{official: official}, nil
}

// GetMessage delegates to the official MessageSessionClient as-is. It returns
// (nil, nil) when there is no message (202 response). maxCapacity receives the
// configured maxRunners. No message conversion or input validation happens;
// the official listener receives the official RunnerScaleSetMessage as-is.
func (l *ListenerClient) GetMessage(ctx context.Context, lastMessageID, maxCapacity int) (*scalesetapi.RunnerScaleSetMessage, error) {
	msg, err := l.official.GetMessage(ctx, lastMessageID, maxCapacity)
	if err != nil {
		// The official error can include the message queue URL (with session
		// token), so redact it first.
		return nil, redactSessionError(err)
	}
	return msg, nil
}

// DeleteMessage delegates to the official MessageSessionClient as-is. The ack
// order and 404 handling are unchanged and left to the official listener's
// error handling.
func (l *ListenerClient) DeleteMessage(ctx context.Context, messageID int) error {
	// The official error can include the message queue URL (with session
	// token), so redact it first.
	return redactSessionError(l.official.DeleteMessage(ctx, messageID))
}

// AcquireJobs delegates to the official MessageSessionClient as-is. The
// official behavior that a returned ID subset of the request is still success
// is unchanged.
func (l *ListenerClient) AcquireJobs(ctx context.Context, requestIDs []int64) ([]int64, error) {
	acquired, err := l.official.AcquireJobs(ctx, requestIDs)
	if err != nil {
		// The official error can include the message queue URL (with session
		// token), so redact it first.
		return nil, redactSessionError(err)
	}
	return acquired, nil
}

// Session delegates to the official MessageSessionClient as-is. It returns the
// official RunnerScaleSetSession including the MessageQueueAccessToken, as the
// official listener.Client interface requires. The token stays out of logs and
// errors thanks to redactSessionError and the rule of logging only the
// top-level error's Error().
func (l *ListenerClient) Session() scalesetapi.RunnerScaleSetSession {
	return l.official.Session()
}

// Close deletes the message session. Call it at shutdown.
func (l *ListenerClient) Close(ctx context.Context) error {
	// The official error can include the message queue URL (with session
	// token), so redact it first.
	return redactSessionError(l.official.Close(ctx))
}
