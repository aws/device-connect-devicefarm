// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package bridge

import (
	"context"
	"fmt"
	"io"
	"testing"
	"time"

	"golang.org/x/net/websocket"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/aws/device-connect-devicefarm/internal/testsupport"
)

// testSessionLength is the session length the status reads run under.
const testSessionLength = 150 * time.Minute

// statusMocks is a server mock endpoint with its status route, for a read under testWait.
func statusMocks(t *testing.T) (*testsupport.SessionMockWSS, *testsupport.StatusMockWSS) {
	t.Helper()
	sessions, _, status := testsupport.NewServerMockWSS(t, testsupport.SessionMockConfig{})
	return sessions, status
}

// fetchTimeLeft runs one status read against endpoint, bounded by testWait.
func fetchTimeLeft(t *testing.T, endpoint string) (time.Duration, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testWait)
	t.Cleanup(cancel)
	return newTestDialer(testWait).FetchSessionTimeLeft(ctx, endpoint, testSessionLength)
}

func TestGivenAStatusReplyWhenTheTimeLeftIsReadThenItIsReturnedAndTheSocketCloses(t *testing.T) {
	_, status := statusMocks(t)
	status.SetTimeLeft(140 * time.Minute)

	timeLeft, err := fetchTimeLeft(t, status.URL)

	require.NoError(t, err)
	assert.Equal(t, 140*time.Minute, timeLeft)
	waitFor(t, func() bool { return status.OpenSockets() == 0 }, "the read must close its socket")
}

func TestGivenAValueThatIsAnObjectWhenTheTimeLeftIsReadThenItIsAccepted(t *testing.T) {
	_, status := statusMocks(t)
	status.AnswerWith([]byte(`{"status": "success", "value": {"status": "running", "timeLeft": 60.0}}`))

	timeLeft, err := fetchTimeLeft(t, status.URL)

	require.NoError(t, err)
	assert.Equal(t, time.Minute, timeLeft)
}

func TestGivenOtherFramesBeforeTheStatusReplyWhenReadThenTheyAreSkipped(t *testing.T) {
	// The route also carries texts for the web console; none of these is a status reply.
	wsURL := testsupport.NewMockWSS(t, func(serverConn *websocket.Conn) {
		for _, frame := range []string{
			"not json",
			`{"status": "success", "message": "no value member"}`,
			`{"value": "not an object"}`,
			`{"value": "{\"status\": \"running\"}"}`,
			`{"status": "success", "value": "{\"status\": \"running\", \"timeLeft\": 300.0}"}`,
		} {
			if websocket.Message.Send(serverConn, frame) != nil {
				return
			}
		}
		io.Copy(io.Discard, serverConn)
	})

	timeLeft, err := fetchTimeLeft(t, wsURL)

	require.NoError(t, err)
	assert.Equal(t, 5*time.Minute, timeLeft)
}

func TestGivenATimeLeftOutsideTheSessionLengthWhenReadThenItIsRefused(t *testing.T) {
	tests := []struct {
		name     string
		timeLeft time.Duration
	}{
		{name: "zero", timeLeft: 0},
		{name: "negative", timeLeft: -time.Minute},
		{name: "above the session length", timeLeft: testSessionLength + time.Minute},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			_, status := statusMocks(t)
			status.AnswerWith([]byte(fmt.Sprintf(
				`{"status": "success", "value": {"timeLeft": %.1f}}`, testCase.timeLeft.Seconds())))

			_, err := fetchTimeLeft(t, status.URL)

			assert.ErrorIs(t, err, errTimeLeftOutOfRange)
		})
	}
}

func TestGivenAReplyThatIsNotAStatusWhenTheFrameLimitIsReachedThenTheReadFails(t *testing.T) {
	// The server pushes more non-status frames than the read allows.
	wsURL := testsupport.NewMockWSS(t, func(serverConn *websocket.Conn) {
		for range maxStatusFrames + 1 {
			if websocket.Message.Send(serverConn, `{"status": "SUCCESS", "message": "not a status reply"}`) != nil {
				return
			}
		}
		io.Copy(io.Discard, serverConn)
	})

	_, err := fetchTimeLeft(t, wsURL)

	assert.ErrorIs(t, err, errNoStatusReply)
}

func TestGivenATimeLeftAtTheSessionLengthWhenReadThenItIsAccepted(t *testing.T) {
	_, status := statusMocks(t)
	status.SetTimeLeft(testSessionLength)

	timeLeft, err := fetchTimeLeft(t, status.URL)

	require.NoError(t, err)
	assert.Equal(t, testSessionLength, timeLeft)
}

func TestGivenAnOversizedReplyWhenReadThenTheReadFails(t *testing.T) {
	_, status := statusMocks(t)
	oversized := fmt.Sprintf(`{"status": "%s", "value": {"timeLeft": 60.0}}`,
		string(make([]byte, maxStatusReplyBytes)))
	status.AnswerWith([]byte(oversized))

	_, err := fetchTimeLeft(t, status.URL)

	assert.ErrorIs(t, err, errStatusReplyTooBig)
}

func TestGivenASilentServerWhenTheContextEndsThenTheReadFailsAndTheSocketCloses(t *testing.T) {
	_, status := statusMocks(t)
	status.StaySilent()
	ctx, cancel := context.WithTimeout(context.Background(), shortTimeout)
	t.Cleanup(cancel)

	started := time.Now()
	_, err := newTestDialer(testWait).FetchSessionTimeLeft(ctx, status.URL, testSessionLength)

	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Less(t, time.Since(started), testWait, "the read returns at its bound; it never waits for the teardown")
	waitFor(t, func() bool { return status.OpenSockets() == 0 }, "the failed read must close its socket")
}

func TestGivenAStatusReadWhenTheRequestIsSentThenItIsTheOnlyFrameTheServerSees(t *testing.T) {
	sessions, status := statusMocks(t)

	_, err := fetchTimeLeft(t, status.URL)

	require.NoError(t, err)
	require.Len(t, status.Requests(), 1, "one request and nothing else")
	assert.Equal(t, statusRequest, string(status.Requests()[0]))
	assert.Empty(t, sessions.RoutesDialed(), "the read stays on the status route")
}

func TestGivenAStatusReadWhenItFailsThenTheErrorCarriesNoQuery(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), shortTimeout)
	t.Cleanup(cancel)

	_, err := NewWSDialer(testWait).FetchSessionTimeLeft(ctx, testsupport.PresignedUnreachableEndpoint, testSessionLength)

	require.Error(t, err)
	assert.NotContains(t, err.Error(), testsupport.SignatureMarker)
	assert.NotContains(t, err.Error(), "path=", "the routed query must not leak either")
}
