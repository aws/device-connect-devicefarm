package testsupport

import (
	"encoding/binary"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/websocket"
)

// The session mock is a second implementation of the /adb wire contract, so it
// needs its own tests: a mock that silently stops speaking the contract would make
// the client's tests pass against a fiction.

// firstFrame reads one whole binary message.
func firstFrame(t *testing.T, ws *websocket.Conn) []byte {
	t.Helper()
	var message []byte
	require.NoError(t, websocket.Message.Receive(ws, &message))
	return message
}

// assertControlFrame checks a message is a control frame with the given status and
// returns its payload.
func assertControlFrame(t *testing.T, message []byte, wantStatus byte) string {
	t.Helper()
	require.Greater(t, len(message), len(magicPrefix), "a control frame carries a status byte")
	require.Equal(t, magicPrefix, string(message[:len(magicPrefix)]), "the magic prefix must lead the frame")
	require.Equal(t, wantStatus, message[len(magicPrefix)])
	return string(message[len(magicPrefix)+1:])
}

func TestGivenNewConnectionWhenFirstFrameSentThenServerAnnouncesANewSessionUUID(t *testing.T) {
	mock := NewSessionEchoWSS(t)
	ws := dialTestWSS(t, mock.URL)
	ws.PayloadType = websocket.BinaryFrame

	require.NoError(t, websocket.Message.Send(ws, []byte("000chost:version")))

	uuid := assertControlFrame(t, firstFrame(t, ws), controlStatusNew)
	assert.Len(t, uuid, sessionUUIDLen, "the payload must be a 36-byte session uuid")
	assert.Regexp(t, uuidV4Pattern, uuid, "the mock must mint a canonical UUIDv4, as the real server does")
	assert.Equal(t, 1, mock.SessionsOpened())
}

func TestGivenNewSessionWhenFirstFrameIsAdbBytesThenThoseBytesAreStillForwarded(t *testing.T) {
	// A new session's first frame is raw ADB bytes and must be processed after the
	// ack, not swallowed as a control handshake.
	mock := NewSessionEchoWSS(t)
	ws := dialTestWSS(t, mock.URL)
	ws.PayloadType = websocket.BinaryFrame
	request := []byte("000chost:version")
	require.NoError(t, websocket.Message.Send(ws, request))
	assertControlFrame(t, firstFrame(t, ws), controlStatusNew)

	assert.Equal(t, request, firstFrame(t, ws), "the first frame's ADB bytes must still be echoed")
	require.Len(t, mock.Sessions(), 1)
	assert.Equal(t, request, mock.Sessions()[0].Inbound(), "inbound bytes must be recorded in order")
}

func TestGivenExistingSessionWhenUUIDSentAsFirstFrameThenReattachIsAcked(t *testing.T) {
	mock := NewSessionEchoWSS(t)
	first := dialTestWSS(t, mock.URL)
	first.PayloadType = websocket.BinaryFrame
	require.NoError(t, websocket.Message.Send(first, []byte("000chost:version")))
	uuid := assertControlFrame(t, firstFrame(t, first), controlStatusNew)

	// A bare UUID as the first frame is the reconnect signal; no client-to-server
	// magic prefix is involved.
	second := dialTestWSS(t, mock.URL)
	second.PayloadType = websocket.BinaryFrame
	require.NoError(t, websocket.Message.Send(second, []byte(uuid)))

	assert.Equal(t, uuid, assertControlFrame(t, firstFrame(t, second), controlStatusReattach))
	assert.Equal(t, 1, mock.SessionsOpened(), "a reattach must not mint a second session")
	assert.Equal(t, 1, mock.Reattaches())
}

func TestGivenUnknownUUIDWhenSentAsFirstFrameThenConnectionIsRefusedWithError(t *testing.T) {
	mock := NewSessionEchoWSS(t)
	ws := dialTestWSS(t, mock.URL)
	ws.PayloadType = websocket.BinaryFrame

	require.NoError(t, websocket.Message.Send(ws, []byte("99999999-8888-4777-8666-555555555555")))

	reason := assertControlFrame(t, firstFrame(t, ws), controlStatusError)
	assert.NotEmpty(t, reason, "a refusal must carry a human-readable reason")
	assert.Zero(t, mock.SessionsOpened(), "a refused reconnect must not mint a session")
}

func TestGivenRejectReattachConfiguredWhenReconnectingThenAlwaysRefused(t *testing.T) {
	// This is the knob that exercises the client's fresh-session fallback.
	mock := NewSessionMockWSS(t, SessionMockConfig{RejectReattach: true})
	first := dialTestWSS(t, mock.URL)
	first.PayloadType = websocket.BinaryFrame
	require.NoError(t, websocket.Message.Send(first, []byte("000chost:version")))
	uuid := assertControlFrame(t, firstFrame(t, first), controlStatusNew)

	second := dialTestWSS(t, mock.URL)
	second.PayloadType = websocket.BinaryFrame
	require.NoError(t, websocket.Message.Send(second, []byte(uuid)))

	assertControlFrame(t, firstFrame(t, second), controlStatusError)
	assert.Zero(t, mock.Reattaches())
}

func TestGivenSupersededWebsocketWhenItKeepsWritingThenItsBytesAreSilentlyDropped(t *testing.T) {
	// The server behavior that forces the client to track its read and write targets
	// separately: once superseded, a websocket's inbound bytes vanish with no error.
	mock := NewSessionMockWSS(t, SessionMockConfig{CleanCutDelay: time.Minute})
	first := dialTestWSS(t, mock.URL)
	first.PayloadType = websocket.BinaryFrame
	require.NoError(t, websocket.Message.Send(first, []byte("000chost:version")))
	uuid := assertControlFrame(t, firstFrame(t, first), controlStatusNew)
	require.Equal(t, "000chost:version", string(firstFrame(t, first)))

	second := dialTestWSS(t, mock.URL)
	second.PayloadType = websocket.BinaryFrame
	require.NoError(t, websocket.Message.Send(second, []byte(uuid)))
	require.Equal(t, uuid, assertControlFrame(t, firstFrame(t, second), controlStatusReattach))

	// The predecessor is still open (a long clean-cut delay) but no longer current.
	require.NoError(t, websocket.Message.Send(first, []byte("0006lost:")))
	// A frame on the live websocket, sent after, is answered; that ordering proves the
	// server processed the later frame while the earlier one was dropped.
	require.NoError(t, websocket.Message.Send(second, []byte("000ehost:get-state")))
	assert.Equal(t, "000ehost:get-state", string(firstFrame(t, second)))

	assert.NotContains(t, string(mock.Sessions()[0].Inbound()), "lost:",
		"a superseded websocket's bytes must be silently dropped")
}

func TestGivenSequenceServerWhenReattachedThenStreamResumesAtTheNextIndex(t *testing.T) {
	// The property that makes gap/dupe/reorder a byte-exact assertion: the sequence
	// must continue across a reattach rather than restarting.
	const chunkSize = 16
	mock := NewSequenceWSS(t, SequenceConfig{ChunkSize: chunkSize})

	first := dialTestWSS(t, mock.URL)
	first.PayloadType = websocket.BinaryFrame
	require.NoError(t, websocket.Message.Send(first, []byte("000chost:version")))
	uuid := assertControlFrame(t, firstFrame(t, first), controlStatusNew)
	received := firstFrame(t, first)

	second := dialTestWSS(t, mock.URL)
	second.PayloadType = websocket.BinaryFrame
	require.NoError(t, websocket.Message.Send(second, []byte(uuid)))
	require.Equal(t, uuid, assertControlFrame(t, firstFrame(t, second), controlStatusReattach))
	require.NoError(t, websocket.Message.Send(second, []byte("000chost:version")))
	received = append(received, firstFrame(t, second)...)

	assert.Equal(t, ExpectedSequence(2*chunkSize), received,
		"the sequence must resume at the exact next index across a reattach")
}

func TestGivenGraceWindowWhenItExpiresWithNoReconnectThenSessionIsGone(t *testing.T) {
	mock := NewSessionMockWSS(t, SessionMockConfig{Grace: 50 * time.Millisecond})
	first := dialTestWSS(t, mock.URL)
	first.PayloadType = websocket.BinaryFrame
	require.NoError(t, websocket.Message.Send(first, []byte("000chost:version")))
	uuid := assertControlFrame(t, firstFrame(t, first), controlStatusNew)

	// Drop the only websocket and let grace lapse.
	require.NoError(t, first.Close())
	time.Sleep(200 * time.Millisecond)

	late := dialTestWSS(t, mock.URL)
	late.PayloadType = websocket.BinaryFrame
	require.NoError(t, websocket.Message.Send(late, []byte(uuid)))

	assertControlFrame(t, firstFrame(t, late), controlStatusError)
	assert.Empty(t, mock.Sessions(), "an expired session must be dropped from the registry")
}

func TestGivenReconnectWithinGraceWhenReattachedThenSessionSurvives(t *testing.T) {
	mock := NewSessionMockWSS(t, SessionMockConfig{Grace: 5 * time.Second})
	first := dialTestWSS(t, mock.URL)
	first.PayloadType = websocket.BinaryFrame
	require.NoError(t, websocket.Message.Send(first, []byte("000chost:version")))
	uuid := assertControlFrame(t, firstFrame(t, first), controlStatusNew)

	require.NoError(t, first.Close())

	second := dialTestWSS(t, mock.URL)
	second.PayloadType = websocket.BinaryFrame
	require.NoError(t, websocket.Message.Send(second, []byte(uuid)))

	assert.Equal(t, uuid, assertControlFrame(t, firstFrame(t, second), controlStatusReattach),
		"a reconnect within grace must resume the session")
}

func TestGivenDialsOnTwoRoutesWhenServedThenRoutesDialedListsThemInOrder(t *testing.T) {
	// Each first frame is answered only after serve recorded the route, so the
	// order is fixed before the next dial starts.
	mock := NewSessionEchoWSS(t)
	for _, route := range []string{"adb", "adbreverse"} {
		ws := dialTestWSS(t, mock.URL+"?path="+route)
		ws.PayloadType = websocket.BinaryFrame
		require.NoError(t, websocket.Message.Send(ws, []byte("000chost:version")))
		assertControlFrame(t, firstFrame(t, ws), controlStatusNew)
	}

	assert.Equal(t, []string{"adb", "adbreverse"}, mock.RoutesDialed())
}

// readCloseCode reads frames until the peer's close and returns its status code.
func readCloseCode(t *testing.T, ws *websocket.Conn) int {
	t.Helper()
	for {
		frame, err := ws.NewFrameReader()
		require.NoError(t, err)
		payload, err := io.ReadAll(frame)
		require.NoError(t, err)
		if frame.PayloadType() == websocket.CloseFrame {
			require.GreaterOrEqual(t, len(payload), 2, "the close must carry a status code")
			return int(binary.BigEndian.Uint16(payload))
		}
	}
}

func TestGivenAServerCloseWhenTheSessionIsReattachedThenOnlyAnEndedSessionIsRefused(t *testing.T) {
	tests := []struct {
		name          string
		closeAttached func(mock *SessionMockWSS, session *MockSession)
		wantCloseCode int
		wantAckStatus byte
	}{
		{
			name:          "the server ended the session",
			closeAttached: func(mock *SessionMockWSS, session *MockSession) { mock.EndSession(session, SessionEndedCloseCode) },
			wantCloseCode: SessionEndedCloseCode,
			wantAckStatus: controlStatusError,
		},
		{
			name:          "the relay lost its leg to the server",
			closeAttached: func(_ *SessionMockWSS, session *MockSession) { session.DropAttached(InternalErrorCloseCode) },
			wantCloseCode: InternalErrorCloseCode,
			wantAckStatus: controlStatusReattach,
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			mock := NewSessionEchoWSS(t)
			ws := dialTestWSS(t, mock.URL)
			ws.PayloadType = websocket.BinaryFrame
			require.NoError(t, websocket.Message.Send(ws, []byte("000chost:version")))
			uuid := assertControlFrame(t, firstFrame(t, ws), controlStatusNew)
			firstFrame(t, ws) // the echo
			require.Len(t, mock.Sessions(), 1)

			testCase.closeAttached(mock, mock.Sessions()[0])

			assert.Equal(t, testCase.wantCloseCode, readCloseCode(t, ws))
			reattach := dialTestWSS(t, mock.URL)
			reattach.PayloadType = websocket.BinaryFrame
			require.NoError(t, websocket.Message.Send(reattach, []byte(uuid)))
			assertControlFrame(t, firstFrame(t, reattach), testCase.wantAckStatus)
		})
	}
}
