package daemon

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/aws/device-connect-devicefarm/internal/config"
	"github.com/aws/device-connect-devicefarm/internal/session"
)

const verdictARN = "arn:aws:devicefarm:us-west-2:123456789012:session:11111111-2222-3333-4444-555555555555"

// recoveryCommand ends every detach reason that needs a new session.
const recoveryCommand = "start a new session and run 'devicefarm-connect device add --session-arn <new ARN>'"

func TestGivenEachLookupFailureWhenWordedThenTheReasonNamesTheStateAndOnlyADeadSessionDetaches(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantReason string
		wantDetach bool
	}{
		{name: "not found", err: fmt.Errorf("remote access session %q %w", verdictARN, session.ErrSessionNotFound),
			wantReason: "remote access session " + verdictARN + " no longer exists; " + recoveryCommand, wantDetach: true},
		{name: "completed", err: &session.SessionEndedError{SessionARN: verdictARN, Status: "COMPLETED"},
			wantReason: "remote access session " + verdictARN + " has ended (COMPLETED); " + recoveryCommand, wantDetach: true},
		{name: "stopping", err: &session.SessionEndedError{SessionARN: verdictARN, Status: "STOPPING"},
			wantReason: "remote access session " + verdictARN + " is stopping (STOPPING); " + recoveryCommand, wantDetach: true},
		{name: "ended without a status word", err: fmt.Errorf("lookup: %w", session.ErrSessionEnded),
			wantReason: "remote access session " + verdictARN + " has ended; " + recoveryCommand, wantDetach: true},
		{name: "access denied", err: fmt.Errorf("GetRemoteAccessSession: %w: api error", session.ErrAccessDenied),
			wantReason: "the daemon can no longer read remote access session " + verdictARN +
				": access denied; restart the bridge from a shell with working AWS credentials", wantDetach: true},
		{name: "invalid ARN", err: fmt.Errorf("%w: parse", session.ErrInvalidARN),
			wantReason: "the daemon can no longer read remote access session " + verdictARN +
				": invalid session ARN; restart the bridge from a shell with working AWS credentials", wantDetach: true},
		{name: "unsupported device", err: fmt.Errorf("%w", session.ErrUnsupportedDevice),
			wantReason: "the daemon can no longer read remote access session " + verdictARN +
				": unsupported device; restart the bridge from a shell with working AWS credentials", wantDetach: true},
		{name: "no endpoint yet", err: fmt.Errorf("remote access session %q %w (status PREPARING)", verdictARN, session.ErrNoEndpointYet),
			wantReason: "the daemon can no longer read remote access session " + verdictARN +
				": has no interactive endpoint yet; restart the bridge from a shell with working AWS credentials", wantDetach: true},
		{name: "rejected credentials", err: &session.RequestRejectedError{Code: "ExpiredTokenException", Cause: errors.New("api error")},
			wantReason: "the daemon can no longer read remote access session " + verdictARN +
				": request rejected (ExpiredTokenException); restart the bridge from a shell with working AWS credentials",
			wantDetach: true},
		{name: "throttled", err: errors.New("api error ThrottlingException: slow down"),
			wantReason: "the daemon could not look up remote access session " + verdictARN +
				": api error ThrottlingException: slow down; run 'devicefarm-connect device add --session-arn " + verdictARN +
				"' to attach it again"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			reason, detach := detachReason(verdictARN, tc.err)

			assert.Equal(t, tc.wantReason, reason)
			assert.Equal(t, tc.wantDetach, detach)
			assert.NotContains(t, reason, "\n", "one line for adb and the status")
		})
	}
}

func TestGivenTheRefusalHandlerWhenTheSessionResolvesOrFailsThenTheVerdictCarriesTheEndpointOrTheReason(t *testing.T) {
	client := newFakeClient(testEndpointRenewed, "Pixel 10 (OS 17)")
	manager := newTestManagerWithClient(t, config.Default(), client)
	handler := manager.refusalHandler(context.Background())

	running := handler(verdictARN)
	assert.Equal(t, testEndpointRenewed, running.Endpoint, "a RUNNING session renews the endpoint")
	assert.Equal(t, "Pixel 10 (OS 17)", running.Label)
	assert.False(t, running.Detach)

	client.set("", &session.SessionEndedError{SessionARN: verdictARN, Status: "COMPLETED"})
	ended := handler(verdictARN)
	assert.True(t, ended.Detach)
	assert.Contains(t, ended.Reason, "has ended (COMPLETED)")
	assert.Empty(t, ended.Endpoint)

	client.set("", errors.New("connection reset"))
	kept := handler(verdictARN)
	assert.False(t, kept.Detach, "a network failure keeps the device")
	assert.Empty(t, kept.Endpoint, "and renews nothing")
	assert.Equal(t, []string{verdictARN, verdictARN, verdictARN}, client.resolvedARNs())
}

func TestGivenACancelledDaemonWhenTheRefusalHandlerRunsThenTheDeviceIsKept(t *testing.T) {
	client := newFakeClient(testEndpointRenewed, "Pixel 10 (OS 17)")
	client.set("", context.Canceled)
	manager := newTestManagerWithClient(t, config.Default(), client)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	verdict := manager.refusalHandler(ctx)(verdictARN)

	require.False(t, verdict.Detach, "a shutdown is not a dead session")
	assert.Empty(t, verdict.Endpoint)
}
