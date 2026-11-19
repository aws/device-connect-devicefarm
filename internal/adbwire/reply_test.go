package adbwire

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The wire vectors are hand-written from the adb host server source named in
// doc.go: they are the contract with real adb clients, so the duplication
// against the writers is deliberate.

// oneWriteSink records each write separately, so a test can prove a reply
// reached the wire in a single write.
type oneWriteSink struct {
	writes [][]byte
}

func (sink *oneWriteSink) Write(p []byte) (int, error) {
	sink.writes = append(sink.writes, append([]byte(nil), p...))
	return len(p), nil
}

func TestGivenForwardFamilyOutcomesWhenRepliedThenBytesMatchTheAdbServer(t *testing.T) {
	tests := []struct {
		name  string
		write func(writer io.Writer) error
		wire  string
	}{
		{
			name:  "bare OKAY",
			write: WriteOkay,
			wire:  "OKAY",
		},
		{
			name:  "forward success is a double OKAY",
			write: func(writer io.Writer) error { return WriteForwardSuccess(writer, 0) },
			wire:  "OKAYOKAY",
		},
		{
			name:  "forward tcp:0 success carries the resolved port",
			write: func(writer io.Writer) error { return WriteForwardSuccess(writer, 41235) },
			wire:  "OKAYOKAY000541235",
		},
		{
			name:  "bind failure is a single FAIL",
			write: func(writer io.Writer) error { return WriteFail(writer, FailCannotBind("Address already in use")) },
			wire:  "FAIL002ccannot bind listener: Address already in use",
		},
		{
			name:  "norebind failure",
			write: func(writer io.Writer) error { return WriteFail(writer, MsgCannotRebind) },
			wire:  "FAIL001dcannot rebind existing socket",
		},
		{
			name:  "killforward miss",
			write: func(writer io.Writer) error { return WriteFail(writer, FailListenerNotFound("tcp:1234")) },
			wire:  "FAIL001dlistener 'tcp:1234' not found",
		},
		{
			name:  "serial mismatch",
			write: func(writer io.Writer) error { return WriteFail(writer, FailDeviceNotFound("R3CN30XXXX")) },
			wire:  "FAIL001ddevice 'R3CN30XXXX' not found",
		},
		{
			name:  "any-device selector miss",
			write: func(writer io.Writer) error { return WriteFail(writer, MsgNoDevices) },
			wire:  "FAIL001ano devices/emulators found",
		},
		{
			name:  "reason length counts bytes, not runes",
			write: func(writer io.Writer) error { return WriteFail(writer, "déjà") },
			wire:  "FAIL0006d\xc3\xa9j\xc3\xa0",
		},
		{
			name: "list-forward with one rule",
			write: func(writer io.Writer) error {
				return WriteListForward(writer, FormatListForward([]AdbListedForward{
					{Serial: "R3CN30XXXX", Local: "tcp:1234", Remote: "tcp:9090"},
				}))
			},
			wire: "OKAY001dR3CN30XXXX tcp:1234 tcp:9090\n",
		},
		{
			name: "list-forward with no rules",
			write: func(writer io.Writer) error {
				return WriteListForward(writer, FormatListForward(nil))
			},
			wire: "OKAY0000",
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			var buffer bytes.Buffer

			require.NoError(t, testCase.write(&buffer))

			assert.Equal(t, []byte(testCase.wire), buffer.Bytes())
		})
	}
}

// A resolved port rides in the same write as the two status words, so a client
// cannot read OKAY OKAY, act on it, and then meet a write error where the port
// belongs. Write count is this writer's own property, not adb's, which sends
// the two pieces separately; only this reply pins it.
func TestGivenAForwardSuccessWhenWrittenThenTheWholeReplyIsOneWrite(t *testing.T) {
	tests := []struct {
		name         string
		resolvedPort int
		wire         string
	}{
		{name: "no resolved port", resolvedPort: 0, wire: "OKAYOKAY"},
		{name: "resolved port", resolvedPort: 41235, wire: "OKAYOKAY000541235"},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			var sink oneWriteSink

			require.NoError(t, WriteForwardSuccess(&sink, testCase.resolvedPort))

			require.Len(t, sink.writes, 1)
			assert.Equal(t, []byte(testCase.wire), sink.writes[0])
		})
	}
}

func TestGivenATportSelectWhenAnsweredThenOkayPlusEightLittleEndianBytes(t *testing.T) {
	tests := []struct {
		name        string
		transportID uint64
		payload     []byte
	}{
		{
			name:        "the id the daemon reports",
			transportID: 1,
			payload:     []byte{0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00},
		},
		{
			// adb reads 0 as unset, and it still owes the client 8 bytes.
			name:        "the unset id",
			transportID: 0,
			payload:     []byte{0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00},
		},
		{
			name:        "every byte distinct pins the order",
			transportID: 0x0102030405060708,
			payload:     []byte{0x08, 0x07, 0x06, 0x05, 0x04, 0x03, 0x02, 0x01},
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			var sink oneWriteSink

			require.NoError(t, WriteTransportSelect(&sink, testCase.transportID))

			require.Len(t, sink.writes, 1, "a split reply lets a client read a short transport id")
			assert.Equal(t, append([]byte("OKAY"), testCase.payload...), sink.writes[0])
		})
	}
}

// A legacy transport select takes the shipped bare OKAY: an id after it
// desynchronizes a client that reads the next reply instead.
func TestGivenALegacySelectWhenAnsweredThenBareOkayWithNoPayload(t *testing.T) {
	var sink oneWriteSink

	require.NoError(t, WriteOkay(&sink))

	require.Len(t, sink.writes, 1)
	assert.Equal(t, []byte("OKAY"), sink.writes[0])
}

func TestGivenMultipleRulesWhenListFormattedThenOneLinePerRuleInOrder(t *testing.T) {
	table := FormatListForward([]AdbListedForward{
		{Serial: "R3CN30XXXX", Local: "tcp:1234", Remote: "tcp:9090"},
		{Serial: "R3CN30XXXX", Local: "tcp:9222", Remote: "localabstract:chrome_devtools_remote"},
	})

	assert.Equal(t,
		"R3CN30XXXX tcp:1234 tcp:9090\n"+
			"R3CN30XXXX tcp:9222 localabstract:chrome_devtools_remote\n",
		table)
}

func TestGivenLargestProtocolStringWhenWrittenThenPrefixIsFFFF(t *testing.T) {
	var buffer bytes.Buffer

	require.NoError(t, WriteProtocolString(&buffer, strings.Repeat("x", maxRequestPayload)))

	assert.Equal(t, "ffff", buffer.String()[:4])
	assert.Equal(t, 4+maxRequestPayload, buffer.Len())
}

func TestGivenOversizedProtocolStringWhenWrittenThenFailsClosed(t *testing.T) {
	oversized := strings.Repeat("x", maxRequestPayload+1)
	tests := []struct {
		name  string
		write func(writer io.Writer) error
	}{
		{name: "protocol string", write: func(writer io.Writer) error { return WriteProtocolString(writer, oversized) }},
		{name: "fail reason", write: func(writer io.Writer) error { return WriteFail(writer, oversized) }},
		{name: "list-forward table", write: func(writer io.Writer) error { return WriteListForward(writer, oversized) }},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			var buffer bytes.Buffer

			err := testCase.write(&buffer)

			require.Error(t, err)
			assert.Zero(t, buffer.Len(), "nothing may reach the wire on failure")
		})
	}
}

var errSinkClosed = errors.New("sink closed")

// failingWriter refuses every write.
type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errSinkClosed }

func TestGivenAFailingWriterWhenRepliedThenTheWriteErrorPropagates(t *testing.T) {
	tests := []struct {
		name  string
		write func(writer io.Writer) error
	}{
		{name: "okay", write: WriteOkay},
		{name: "fail", write: func(writer io.Writer) error { return WriteFail(writer, MsgNoDevices) }},
		{name: "protocol string", write: func(writer io.Writer) error { return WriteProtocolString(writer, "41235") }},
		{name: "forward success", write: func(writer io.Writer) error { return WriteForwardSuccess(writer, 41235) }},
		{name: "forward success with no resolved port", write: func(writer io.Writer) error { return WriteForwardSuccess(writer, 0) }},
		{name: "list-forward", write: func(writer io.Writer) error { return WriteListForward(writer, "") }},
		{name: "transport select", write: func(writer io.Writer) error { return WriteTransportSelect(writer, 1) }},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			err := testCase.write(failingWriter{})

			require.ErrorIs(t, err, errSinkClosed)
		})
	}
}
