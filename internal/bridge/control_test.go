package bridge

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/websocket"

	"github.com/aws/device-connect-devicefarm/internal/testsupport"
)

// testSessionUUID is a canonical UUIDv4 (version nibble 4, variant nibble 8) used
// wherever a test needs a well-formed session identifier.
const testSessionUUID = "11111111-2222-4333-8444-555555555555"

func TestGivenRawAdbBytesWhenUnmarshalledThenReportsNotAControlFrame(t *testing.T) {
	// A peer that answers with raw ADB bytes instead of a control frame must be rejected,
	// not forwarded, or the adb client's framing desynchronizes.
	rawAdbFirstFrames := [][]byte{
		[]byte("000chost:version"),
		[]byte("OKAY0004002c"),
		[]byte("FAIL001cdevice 'x' not found"),
	}

	for _, raw := range rawAdbFirstFrames {
		t.Run(string(raw), func(t *testing.T) {
			_, err := unmarshalControlFrame(raw)

			assert.ErrorIs(t, err, errNotControlFrame)
		})
	}
}

func TestGivenServerThatSendsControlFrameWhenReadWithinThenFrameReturned(t *testing.T) {
	wsURL := testsupport.NewMockWSS(t, func(ws *websocket.Conn) {
		ws.PayloadType = websocket.BinaryFrame
		websocket.Message.Send(ws, testsupport.FrameReattach(testSessionUUID))
		<-make(chan struct{})
	})
	ws := dialMock(t, wsURL)

	frame, err := readControlFrameWithin(ws, 5*time.Second)

	require.NoError(t, err)
	assert.Equal(t, armReattach, frame.arm)
	assert.Equal(t, testSessionUUID, frame.payload)
}

func TestGivenAPingWhenSentThenTheServerReceivesTheVectorBytes(t *testing.T) {
	received := make(chan []byte, 1)
	wsURL := testsupport.NewMockWSS(t, func(ws *websocket.Conn) {
		var message []byte
		if websocket.Message.Receive(ws, &message) == nil {
			received <- message
		}
		<-make(chan struct{})
	})
	ws := dialMock(t, wsURL)

	require.NoError(t, sendControlFrame(ws, controlFrame{arm: armPing}))

	// Fixture vector ping.
	assert.Equal(t, []byte("\x00DFADB\x01{\"ping\":{}}"), await(t, received, "the ping"))
}

func TestGivenAFrameWithNoArmWhenSentThenItFailsBeforeAnySend(t *testing.T) {
	// A nil destination: a send before the encode fails would panic here.
	assert.Error(t, sendControlFrame(nil, controlFrame{}))
}

func TestGivenSilentServerWhenControlFrameReadWithinThenTimesOutAndClosesConn(t *testing.T) {
	// A server that accepts and never sends must not park the caller forever: the
	// read is bounded, and the timeout closes the connection (which is what unblocks
	// the parked Receive, since x/net has no context-aware Receive).
	wsURL := testsupport.NewMockWSS(t, func(ws *websocket.Conn) {
		<-make(chan struct{})
	})
	ws := dialMock(t, wsURL)

	_, err := readControlFrameWithin(ws, 100*time.Millisecond)

	assert.ErrorIs(t, err, errReceiveTimeout)
	// The connection is closed on the timeout path, so a further read must fail
	// rather than block.
	_, receiveErr := ws.Receive()
	assert.Error(t, receiveErr, "the timed-out connection must be closed")
}

func TestGivenServerThatClosesImmediatelyWhenControlFrameReadThenReceiveErrorSurfaces(t *testing.T) {
	wsURL := testsupport.NewMockWSS(t, func(ws *websocket.Conn) {
		ws.Close()
	})
	ws := dialMock(t, wsURL)

	_, err := readControlFrameWithin(ws, 5*time.Second)

	require.Error(t, err)
	assert.NotErrorIs(t, err, errReceiveTimeout, "a peer close is not a timeout")
}
