package adbwire

import (
	"encoding/binary"
	"fmt"
	"io"
	"strconv"
)

// adb's model throughout: the byte-exact replies and failure texts of the adb
// host server for the forward family (adb.cpp handle_forward_request;
// adb_io.cpp SendOkay, SendFail and SendProtocolString; adb_listeners.cpp
// format_listeners; transport.cpp acquire_one_transport for the device-miss
// texts). Success is OKAY OKAY (list-forward: OKAY plus the table); failure is
// one FAIL plus a length-prefixed reason, never OKAY then FAIL.

const (
	okayReply = "OKAY"
	failReply = "FAIL"

	// MsgCannotRebind is adb's failure text when norebind hits an existing
	// listener.
	MsgCannotRebind = "cannot rebind existing socket"
	// MsgNoDevices is adb's failure text when the any-device selector finds
	// no device.
	MsgNoDevices = "no devices/emulators found"
)

// FailCannotBind wraps a bind failure cause the way adb does.
func FailCannotBind(cause string) string {
	return fmt.Sprintf("cannot bind listener: %s", cause)
}

// FailListenerNotFound is killforward's miss text; local is the raw spec text.
func FailListenerNotFound(local string) string {
	return fmt.Sprintf("listener '%s' not found", local)
}

// FailDeviceNotFound is the miss text for a serial selector.
func FailDeviceNotFound(serial string) string {
	return fmt.Sprintf("device '%s' not found", serial)
}

// WriteOkay writes one OKAY status word.
func WriteOkay(writer io.Writer) error {
	_, err := io.WriteString(writer, okayReply)
	return err
}

// WriteTransportSelect answers a tport: transport select in one write: OKAY
// plus the transport id as 8 raw bytes, with no length prefix (adb.cpp
// handle_host_request: SendOkay, then the uint64 transport id verbatim, in the
// host's native order, which is little-endian on every target this daemon
// builds for). A transport-id:<n> select takes WriteOkay instead: 4 bytes and
// no payload. Get the pair wrong and a real client either reports a failure to
// read the transport id (a short payload), hangs (no payload) or reads a
// protocol fault (a length prefix).
func WriteTransportSelect(writer io.Writer, transportID uint64) error {
	reply := binary.LittleEndian.AppendUint64([]byte(okayReply), transportID)
	_, err := writer.Write(reply)
	return err
}

// WriteFail writes FAIL plus the length-prefixed reason in one write, or
// nothing when the reason is oversized.
func WriteFail(writer io.Writer, reason string) error {
	return writeFramed(writer, failReply, reason)
}

// WriteProtocolString writes a 4-lowercase-hex length prefix plus the payload,
// or nothing when the payload is oversized.
func WriteProtocolString(writer io.Writer, payload string) error {
	return writeFramed(writer, "", payload)
}

// WriteForwardSuccess writes OKAY OKAY for forward, killforward and
// killforward-all in one write, followed in the same write by the resolved
// port as a protocol string when a tcp:0 forward was assigned one (adb sends
// it only when non-zero). The adb client then reads for that optional string
// until end of stream, so the caller closes the connection after this reply.
func WriteForwardSuccess(writer io.Writer, resolvedTCPPort int) error {
	if resolvedTCPPort == 0 {
		_, err := io.WriteString(writer, okayReply+okayReply)
		return err
	}
	return writeFramed(writer, okayReply+okayReply, strconv.Itoa(resolvedTCPPort))
}

// WriteListForward writes list-forward's reply, OKAY plus the rendered table
// as one protocol string, in one write; nothing when the table is oversized.
func WriteListForward(writer io.Writer, table string) error {
	return writeFramed(writer, okayReply, table)
}

// AdbListedForward is one list-forward row. adb's model: the columns of an
// adb_listeners.cpp format_listeners line. The struct is the daemon's own.
type AdbListedForward struct {
	Serial string
	Local  string
	Remote string
}

// FormatListForward renders rules as format_listeners does: one
// "<serial> <local> <remote>\n" line per rule, empty string for no rules. The
// caller fills the serial column; adb prints "(reverse)" there only for a
// listener with no transport, which this daemon never lists.
func FormatListForward(entries []AdbListedForward) string {
	var table string
	for _, entry := range entries {
		table += fmt.Sprintf("%s %s %s\n", entry.Serial, entry.Local, entry.Remote)
	}
	return table
}

// writeFramed writes prefix, then payload behind its byte length as 4 lowercase hex
// digits, in one write. Past maxRequestPayload it fails closed: adb's own cap
// is MAX_PAYLOAD - 4 (about 1 MiB), and between the two adb would emit a
// 5-digit prefix that the client reads as 4 digits, misaligning the payload.
func writeFramed(writer io.Writer, prefix, payload string) error {
	if len(payload) > maxRequestPayload {
		return fmt.Errorf("protocol string of %d bytes exceeds the %d-byte maximum", len(payload), maxRequestPayload)
	}
	_, err := fmt.Fprintf(writer, "%s%04x%s", prefix, len(payload), payload)
	return err
}
