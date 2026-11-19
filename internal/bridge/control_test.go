package bridge

import (
	"fmt"
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

// newSessionControlFrame builds a control frame the way the server does: magic
// prefix + status byte + UTF-8 payload, with no length field.
//
// A deliberate independent copy of the layout, byte for byte, kept as the parser's oracle.
// Never replace it with encodeControlFrame: the parser tests would then assert the encoder
// against itself and a wrong layout would pass both.
func newSessionControlFrame(status byte, payload string) []byte {
	frame := make([]byte, 0, len(magicPrefix)+1+len(payload))
	frame = append(frame, magicPrefix...)
	frame = append(frame, status)
	return append(frame, payload...)
}

// reverseOpenPayload renders a REVERSE_OPEN payload as the server does.
func reverseOpenPayload(token string, port int) string { return fmt.Sprintf("%s:%d", token, port) }

func TestGivenTheWireContractWhenComparedToItsLiteralBytesThenItIsUnchanged(t *testing.T) {
	// Every other test builds frames from the same constants the parser uses, so a changed
	// magic prefix or status would leave the suite green and break the wire. Pin the
	// literals: this is the contract shared with the server.
	assert.Equal(t, []byte{0x00, 'D', 'F', 'A', 'D', 'B', 0x00}, []byte(magicPrefix),
		"the magic prefix is a wire constant shared with the server")
	assert.Len(t, magicPrefix, 7, "the prefix is exactly 7 bytes; the status byte follows it")

	assert.Equal(t, byte(0x00), controlStatusNew)
	assert.Equal(t, byte(0x01), controlStatusReattach)
	assert.Equal(t, byte(0x02), controlStatusError)

	assert.Equal(t, 36, sessionUUIDLen, "a reconnect's first frame must be exactly 36 bytes")

	// The fully assembled frame, byte for byte, so the layout (prefix, then status,
	// then payload, with no length field) is pinned and not just its pieces.
	assert.Equal(t,
		append([]byte{0x00, 'D', 'F', 'A', 'D', 'B', 0x00, 0x01}, testSessionUUID...),
		newSessionControlFrame(controlStatusReattach, testSessionUUID))
}

func TestGivenTheReverseFrameContractWhenComparedToItsLiteralBytesThenItIsUnchanged(t *testing.T) {
	// Same rule as above for the ?path=adbreverse statuses: these values are fixed for every
	// copy of the codec, and 0x03 to 0x0F stay reserved.
	assert.Equal(t, byte(0x10), controlStatusNotifierHello)
	assert.Equal(t, byte(0x11), controlStatusNotifierReady)
	assert.Equal(t, byte(0x12), controlStatusReverseOpen)
	assert.Equal(t, byte(0x13), controlStatusPing)
	assert.Equal(t, byte(0x14), controlStatusPong)
	assert.Equal(t, byte(0x15), controlStatusConnectorHello)
}

func TestGivenEachNewStatusWhenEncodedThenMagicThenStatusThenPayloadOnTheWire(t *testing.T) {
	// Hand-written expectations, never newSessionControlFrame: the encoder is production
	// code (the client's first outbound control frames) and this is its byte-level check.
	tests := []struct {
		name    string
		status  byte
		payload string
		want    []byte
	}{
		{name: "notifier hello", status: controlStatusNotifierHello,
			want: []byte{0x00, 'D', 'F', 'A', 'D', 'B', 0x00, 0x10}},
		{name: "notifier ready", status: controlStatusNotifierReady,
			want: []byte{0x00, 'D', 'F', 'A', 'D', 'B', 0x00, 0x11}},
		{name: "reverse open", status: controlStatusReverseOpen, payload: reverseOpenPayload(testSessionUUID, 9090),
			want: append([]byte{0x00, 'D', 'F', 'A', 'D', 'B', 0x00, 0x12}, "11111111-2222-4333-8444-555555555555:9090"...)},
		{name: "ping", status: controlStatusPing,
			want: []byte{0x00, 'D', 'F', 'A', 'D', 'B', 0x00, 0x13}},
		{name: "pong", status: controlStatusPong,
			want: []byte{0x00, 'D', 'F', 'A', 'D', 'B', 0x00, 0x14}},
		{name: "connector hello", status: controlStatusConnectorHello, payload: testSessionUUID,
			want: append([]byte{0x00, 'D', 'F', 'A', 'D', 'B', 0x00, 0x15}, "11111111-2222-4333-8444-555555555555"...)},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			assert.Equal(t, testCase.want, encodeControlFrame(testCase.status, testCase.payload))
		})
	}
}

func TestGivenWellFormedControlFrameWhenParsedThenStatusAndPayloadReturned(t *testing.T) {
	tests := []struct {
		name    string
		status  byte
		payload string
	}{
		{name: "new session", status: controlStatusNew, payload: testSessionUUID},
		{name: "reattach", status: controlStatusReattach, payload: testSessionUUID},
		{name: "error carries a variable-length reason", status: controlStatusError, payload: "unknown or expired session"},
		{name: "error reason may be empty", status: controlStatusError, payload: ""},
		{name: "error reason may be long", status: controlStatusError, payload: "failed to connect to ADB server after several retries"},
		{name: "uppercase uuid is accepted", status: controlStatusNew, payload: "AAAAAAAA-BBBB-4CCC-8DDD-EEEEEEEEEEEE"},
		{name: "notifier hello", status: controlStatusNotifierHello, payload: ""},
		{name: "notifier ready", status: controlStatusNotifierReady, payload: ""},
		{name: "reverse open", status: controlStatusReverseOpen, payload: reverseOpenPayload(testSessionUUID, 9090)},
		{name: "ping", status: controlStatusPing, payload: ""},
		{name: "pong", status: controlStatusPong, payload: ""},
		{name: "connector hello", status: controlStatusConnectorHello, payload: testSessionUUID},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			frame, err := parseControlFrame(newSessionControlFrame(tc.status, tc.payload))

			require.NoError(t, err)
			assert.Equal(t, tc.status, frame.status)
			assert.Equal(t, tc.payload, frame.payload)
		})
	}
}

func TestGivenAReverseOpenFrameWhenParsedThenTheFrameCarriesItsTokenAndPort(t *testing.T) {
	// The notifier reads the token and port off the frame, not the payload.
	frame, err := parseControlFrame(
		newSessionControlFrame(controlStatusReverseOpen, reverseOpenPayload(testSessionUUID, 9090)))

	require.NoError(t, err)
	assert.Equal(t, testSessionUUID, frame.reverseToken)
	assert.Equal(t, 9090, frame.customerPort)
}

func TestGivenRawAdbBytesWhenParsedAsControlFrameThenReportsNotAControlFrame(t *testing.T) {
	// A peer that answers with raw ADB bytes instead of a control frame must be rejected,
	// not forwarded, or the adb client's framing desynchronizes.
	rawAdbFirstFrames := [][]byte{
		[]byte("000chost:version"),
		[]byte("OKAY0004002c"),
		[]byte("FAIL001cdevice 'x' not found"),
	}

	for _, raw := range rawAdbFirstFrames {
		t.Run(string(raw), func(t *testing.T) {
			_, err := parseControlFrame(raw)

			assert.ErrorIs(t, err, errNotControlFrame)
		})
	}
}

func TestGivenMalformedControlFrameWhenParsedThenFailsClosed(t *testing.T) {
	tests := []struct {
		name    string
		message []byte
		// notControlFrame distinguishes "the peer does not speak the session model"
		// from "the frame is a corrupt control frame", which are acted on differently.
		notControlFrame bool
	}{
		{name: "empty", message: nil, notControlFrame: true},
		{name: "shorter than the magic", message: []byte("\x00DFA"), notControlFrame: true},
		{name: "magic with no status byte", message: []byte(magicPrefix), notControlFrame: true},
		{name: "wrong magic", message: append([]byte("\x00DFXYZ\x00"), controlStatusNew), notControlFrame: true},
		{name: "unknown status", message: newSessionControlFrame(0x7f, testSessionUUID)},
		{name: "new with a 35-byte payload", message: newSessionControlFrame(controlStatusNew, testSessionUUID[:35])},
		{name: "new with an over-long payload", message: newSessionControlFrame(controlStatusNew, testSessionUUID+"0")},
		{name: "new with an empty payload", message: newSessionControlFrame(controlStatusNew, "")},
		{name: "new with a non-v4 uuid", message: newSessionControlFrame(controlStatusNew, "11111111-2222-3333-8444-555555555555")},
		{name: "new with a bad variant nibble", message: newSessionControlFrame(controlStatusNew, "11111111-2222-4333-c444-555555555555")},
		{name: "new with a non-uuid of the right length", message: newSessionControlFrame(controlStatusNew, "not-a-uuid-but-exactly-36-chars-long")},
		{name: "reattach with a bad payload", message: newSessionControlFrame(controlStatusReattach, "nope")},
		{name: "connector hello with a bad token", message: newSessionControlFrame(controlStatusConnectorHello, "nope")},
		{name: "connector hello with an empty token", message: newSessionControlFrame(controlStatusConnectorHello, "")},
		{name: "reverse open with port 0", message: newSessionControlFrame(controlStatusReverseOpen, reverseOpenPayload(testSessionUUID, 0))},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseControlFrame(tc.message)

			require.Error(t, err, "a malformed control frame must fail closed")
			if tc.notControlFrame {
				assert.ErrorIs(t, err, errNotControlFrame)
			} else {
				assert.NotErrorIs(t, err, errNotControlFrame,
					"a corrupt control frame is distinct from a peer that does not speak the session model")
			}
		})
	}
}

func TestGivenANonEmptyPayloadOnAnEmptyPayloadStatusWhenParsedThenFailsClosed(t *testing.T) {
	// A hello, ready, ping or pong that carries bytes is not "the same frame with extra";
	// it is a malformed frame, so a peer cannot smuggle data on a keepalive.
	for _, status := range []byte{controlStatusNotifierHello, controlStatusNotifierReady, controlStatusPing, controlStatusPong} {
		t.Run(fmt.Sprintf("0x%02x", status), func(t *testing.T) {
			_, err := parseControlFrame(newSessionControlFrame(status, "x"))

			require.Error(t, err)
			assert.NotErrorIs(t, err, errNotControlFrame)
		})
	}
}

func TestGivenAnUnknownStatusByteWhenParsedThenStillFailsClosed(t *testing.T) {
	// The reserved gap below the reverse block, the first byte past it, and the top of the
	// range: adding six statuses must not have opened the switch to anything else.
	for _, status := range []byte{0x03, 0x0f, 0x16, 0xff} {
		t.Run(fmt.Sprintf("0x%02x", status), func(t *testing.T) {
			_, err := parseControlFrame(newSessionControlFrame(status, ""))

			require.Error(t, err)
			assert.NotErrorIs(t, err, errNotControlFrame,
				"a corrupt control frame is distinct from a peer that does not speak the session model")
		})
	}
}

func TestGivenAReverseOpenPayloadWhenParsedThenTokenAndPortSplit(t *testing.T) {
	tests := []struct {
		name      string
		payload   string
		wantToken string
		wantPort  int
	}{
		{name: "lowest port", payload: reverseOpenPayload(testSessionUUID, 1), wantToken: testSessionUUID, wantPort: 1},
		{name: "typical port", payload: reverseOpenPayload(testSessionUUID, 9090), wantToken: testSessionUUID, wantPort: 9090},
		{name: "highest port", payload: reverseOpenPayload(testSessionUUID, 65535), wantToken: testSessionUUID, wantPort: 65535},
		{name: "uppercase token is accepted", payload: "AAAAAAAA-BBBB-4CCC-8DDD-EEEEEEEEEEEE:80",
			wantToken: "AAAAAAAA-BBBB-4CCC-8DDD-EEEEEEEEEEEE", wantPort: 80},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			token, port, err := parseReverseOpen(testCase.payload)

			require.NoError(t, err)
			assert.Equal(t, testCase.wantToken, token)
			assert.Equal(t, testCase.wantPort, port)
		})
	}
}

func TestGivenAReverseOpenPayloadWithoutAPortWhenParsedThenTheErrorCarriesNoPayload(t *testing.T) {
	// A payload with no separator may be a whole, unredeemed token, and this error reaches the log.
	_, err := parseControlFrame(newSessionControlFrame(controlStatusReverseOpen, testSessionUUID))

	require.ErrorContains(t, err, "no port separator")
	assert.NotContains(t, err.Error(), testSessionUUID)
}

func TestGivenAMalformedReverseOpenPayloadWhenParsedThenFailsClosed(t *testing.T) {
	tests := []struct {
		name    string
		payload string
	}{
		{name: "empty", payload: ""},
		{name: "no colon", payload: testSessionUUID + "9090"},
		{name: "empty token", payload: ":9090"},
		{name: "non-v4 token", payload: "11111111-2222-3333-8444-555555555555:9090"},
		{name: "35-byte token", payload: testSessionUUID[:35] + ":9090"},
		{name: "empty port", payload: testSessionUUID + ":"},
		{name: "port 0", payload: testSessionUUID + ":0"},
		{name: "port 65536", payload: testSessionUUID + ":65536"},
		{name: "six-digit port", payload: testSessionUUID + ":100000"},
		{name: "non-numeric port", payload: testSessionUUID + ":abc"},
		{name: "leading zero", payload: testSessionUUID + ":0080"},
		{name: "signed port", payload: testSessionUUID + ":+80"},
		{name: "negative port", payload: testSessionUUID + ":-80"},
		{name: "trailing junk", payload: testSessionUUID + ":80x"},
		{name: "second colon", payload: testSessionUUID + ":80:1"},
		{name: "space before the port", payload: testSessionUUID + ": 80"},
		{name: "trailing space", payload: testSessionUUID + ":80 "},
		{name: "trailing newline", payload: testSessionUUID + ":80\n"},
		{name: "colon inside the token", payload: "11111111:2222-4333-8444-555555555555:9090"},
		{name: "fullwidth digits", payload: testSessionUUID + ":\uff18\uff10"},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			_, _, err := parseReverseOpen(testCase.payload)

			require.Error(t, err, "a malformed reverse open payload must fail closed")
		})
	}
}

func TestGivenServerThatSendsControlFrameWhenReadWithinThenFrameReturned(t *testing.T) {
	wsURL := testsupport.NewMockWSS(t, func(ws *websocket.Conn) {
		ws.PayloadType = websocket.BinaryFrame
		websocket.Message.Send(ws, newSessionControlFrame(controlStatusReattach, testSessionUUID))
		<-make(chan struct{})
	})
	ws := dialMock(t, wsURL)

	frame, err := readControlFrameWithin(ws, 5*time.Second)

	require.NoError(t, err)
	assert.Equal(t, controlStatusReattach, frame.status)
	assert.Equal(t, testSessionUUID, frame.payload)
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
