// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

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
const recoveryCommand = "start a new remote access session, then run 'devicefarm-connect device add --session-arn <new ARN>'"

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
			wantReason: "remote access session " + verdictARN + " is stopping; " + recoveryCommand, wantDetach: true},
		{name: "ended without a status word", err: fmt.Errorf("lookup: %w", session.ErrSessionEnded),
			wantReason: "remote access session " + verdictARN + " has ended; " + recoveryCommand, wantDetach: true},
		{name: "access denied", err: fmt.Errorf("GetRemoteAccessSession: %w: api error", session.ErrAccessDenied),
			wantReason: "the bridge can no longer read remote access session " + verdictARN +
				": access denied; restart the bridge from a shell with working AWS credentials", wantDetach: true},
		{name: "invalid ARN", err: fmt.Errorf("%w: parse", session.ErrInvalidARN),
			wantReason: "the bridge can no longer read remote access session " + verdictARN +
				": invalid session ARN; " + recoveryCommand, wantDetach: true},
		{name: "unsupported device", err: fmt.Errorf("%w", session.ErrUnsupportedDevice),
			wantReason: "the bridge can no longer read remote access session " + verdictARN + ": unsupported device; start a remote " +
				"access session on an Android 9 or later device, then run 'devicefarm-connect device add --session-arn <new ARN>'",
			wantDetach: true},
		{name: "no endpoint", err: &session.NoEndpointError{SessionARN: verdictARN, Status: "RUNNING"},
			wantReason: "the bridge can no longer read remote access session " + verdictARN + ": has no interactive endpoint yet; " +
				"run 'devicefarm-connect doctor --session-arn " + verdictARN + " --port 5037'; if the doctor does not find the " +
				"cause, contact AWS Support", wantDetach: true},
		{name: "expired credentials", err: &session.RequestRejectedError{Code: "ExpiredTokenException", Cause: errors.New("api error")},
			wantReason: "the AWS credentials the bridge started with have expired, so it can no longer read remote access session " +
				verdictARN + "; refresh the credentials, run 'devicefarm-connect bridge stop', start the bridge again from that " +
				"shell with the same options, then run 'devicefarm-connect device add --session-arn " + verdictARN + "'",
			wantDetach: true},
		{name: "rejected credentials", err: &session.RequestRejectedError{Code: "UnrecognizedClientException", Cause: errors.New("api error")},
			wantReason: "the bridge can no longer read remote access session " + verdictARN +
				": request rejected (UnrecognizedClientException); restart the bridge from a shell with working AWS credentials",
			wantDetach: true},
		{name: "throttled", err: errors.New("api error ThrottlingException: slow down"),
			wantReason: "the bridge could not look up remote access session " + verdictARN +
				": api error ThrottlingException: slow down; run 'devicefarm-connect device add --session-arn " + verdictARN +
				"' to attach it again"},
	}
	manager := newTestManagerWithClient(t, config.Default(), newFakeClient(testEndpoint, "Pixel 10 (OS 17)"))
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			reason, detach := manager.detachReason(verdictARN, tc.err)

			assert.Equal(t, tc.wantReason, reason)
			assert.Equal(t, tc.wantDetach, detach)
			assert.NotContains(t, reason, "\n", "one line for adb and the status")
		})
	}
}

func TestGivenTheSessionStoppingCloseWhenItsDetachReasonIsWordedThenItReadsAsASessionFoundStopping(t *testing.T) {
	reason := sessionStoppingReason(verdictARN)

	assert.Equal(t, "remote access session "+verdictARN+" is stopping; "+recoveryCommand, reason)
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
