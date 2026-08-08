// This file adapts the official listener client and redacts queue URLs from
// errors without changing their error chain.
package scaleset

import (
	"context"
	"fmt"
	"regexp"

	scalesetapi "github.com/actions/scaleset"
	listenerapi "github.com/actions/scaleset/listener"
)

// Message queue URLs contain the session token in their query string.
var messageURLPattern = regexp.MustCompile(`https?://\S+`)

// redactedError hides sensitive text while preserving error classification.
type redactedError struct {
	msg   string
	cause error
}

func (e *redactedError) Error() string { return e.msg }

func (e *redactedError) Unwrap() error { return e.cause }

// redactSessionError removes queue URLs from official errors.
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

// ListenerClient adapts the official MessageSessionClient to listener.Client.
type ListenerClient struct {
	official *scalesetapi.MessageSessionClient
}

var _ listenerapi.Client = (*ListenerClient)(nil)

// NewListenerClient starts one message session for a Scale Set.
func (c *Client) NewListenerClient(ctx context.Context, scaleSetID int, owner string) (*ListenerClient, error) {
	official, err := c.official.MessageSessionClient(ctx, scaleSetID, owner)
	if err != nil {
		return nil, fmt.Errorf("create message session for scale set %d: %w", scaleSetID, redactSessionError(err))
	}
	if official == nil {
		return nil, protocolErrorf("create message session", "official session client is nil for scale set %d", scaleSetID)
	}
	return &ListenerClient{official: official}, nil
}

// GetMessage delegates message polling to the official client.
func (l *ListenerClient) GetMessage(ctx context.Context, lastMessageID, maxCapacity int) (*scalesetapi.RunnerScaleSetMessage, error) {
	msg, err := l.official.GetMessage(ctx, lastMessageID, maxCapacity)
	if err != nil {
		return nil, redactSessionError(err)
	}
	return msg, nil
}

// DeleteMessage acknowledges a message through the official client.
func (l *ListenerClient) DeleteMessage(ctx context.Context, messageID int) error {
	return redactSessionError(l.official.DeleteMessage(ctx, messageID))
}

// AcquireJobs delegates job acquisition to the official client.
func (l *ListenerClient) AcquireJobs(ctx context.Context, requestIDs []int64) ([]int64, error) {
	acquired, err := l.official.AcquireJobs(ctx, requestIDs)
	if err != nil {
		return nil, redactSessionError(err)
	}
	return acquired, nil
}

// Session returns the official session, including its private access token.
func (l *ListenerClient) Session() scalesetapi.RunnerScaleSetSession {
	return l.official.Session()
}

// Close deletes the message session.
func (l *ListenerClient) Close(ctx context.Context) error {
	return redactSessionError(l.official.Close(ctx))
}
