package bridge

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// hostDevicesRequest is the ADB smart-socket request for "list devices": a
// 4-hex-digit length prefix (000c = 12 bytes) plus "host:devices". Sent as a
// synthetic keep-alive so the remote access session does not go idle.
const hostDevicesRequest = "000chost:devices"

// defaultProbeReplyTimeout bounds the wait for the devices reply. The probe's real job is
// done once the request is sent, so a missing reply is no failure; the timeout only stops
// a wedged server from stalling the keep-alive loop. Source: pick.
const defaultProbeReplyTimeout = 5 * time.Second

// maxProbeReplyBytes bounds what the probe accumulates across messages, so a length
// header the server never satisfies ends the read. A devices reply is one short line
// per device; one message is already capped by maxInboundMessageBytes. Source: pick.
const maxProbeReplyBytes = 64 * 1024

// replyLengthDigits is the width of the hex length prefix adb puts on a reply payload. Source:
// adb_io.cpp SendProtocolString.
const replyLengthDigits = 4

// SendKeepAlive opens a short-lived websocket to endpoint, sends one host:devices request,
// and reads the reply to learn the device serial. The serial is best-effort: a missing,
// refused or malformed reply yields an empty serial and no error, because the send already
// served the probe's purpose. ctx bounds the dial, probeReplyTimeout the read.
func (dialer *WSDialer) SendKeepAlive(ctx context.Context, endpoint string) (string, error) {
	ws, err := dialer.Dial(ctx, endpoint, RouteAdb)
	if err != nil {
		return "", fmt.Errorf("keep-alive dial: %w", err)
	}
	defer ws.Close()

	if err := ws.Send([]byte(hostDevicesRequest)); err != nil {
		return "", fmt.Errorf("keep-alive send: %w", err)
	}

	// The WSConn seam exposes no read deadline, so the read runs on its own goroutine and
	// the deferred Close unblocks it into the buffered channel. One timer over the whole
	// read, not one per Receive as receiveWithin does: a server trickling a byte at a time
	// would hold the keep-alive loop forever.
	serials := make(chan string, 1)
	go func() { serials <- readProbeSerial(ws) }()

	timer := time.NewTimer(dialer.probeReplyTimeout)
	defer timer.Stop()
	select {
	case serial := <-serials:
		return serial, nil
	case <-timer.C:
		return "", nil
	}
}

// readProbeSerial consumes the probe connection until the devices reply parses or the
// connection ends. The first message is the server's session control frame and is
// skipped when present; the adb reply behind it may arrive split across messages.
func readProbeSerial(ws WSConn) string {
	var reply []byte
	firstMessage := true
	for len(reply) <= maxProbeReplyBytes {
		message, err := ws.Receive()
		if err != nil {
			return ""
		}
		if firstMessage {
			firstMessage = false
			if _, parseErr := parseControlFrame(message); parseErr == nil {
				continue
			}
		}
		reply = append(reply, message...)
		serial, done := parseHostDevicesSerial(reply)
		if done {
			return serial
		}
	}
	return ""
}

// parseHostDevicesSerial parses an accumulated host:devices reply: OKAY plus a
// 4-hex-length-prefixed payload of "<serial>\t<state>" lines. done is false while the
// reply is incomplete; anything but a well-formed OKAY finishes with an empty serial.
func parseHostDevicesSerial(reply []byte) (serial string, done bool) {
	if len(reply) < statusWordLen {
		return "", false
	}
	if string(reply[:statusWordLen]) != statusOkay {
		return "", true
	}
	const headerLen = statusWordLen + replyLengthDigits
	if len(reply) < headerLen {
		return "", false
	}
	payloadLen, err := strconv.ParseUint(string(reply[statusWordLen:headerLen]), 16, 32)
	if err != nil {
		return "", true
	}
	payloadEnd := uint64(headerLen) + payloadLen
	if uint64(len(reply)) < payloadEnd {
		return "", false
	}
	return firstDeviceSerial(string(reply[headerLen:payloadEnd])), true
}

// firstDeviceSerial returns the serial column of the first "<serial>\t<state>" row of a
// devices payload. A line without a state column is not a device row, so it is skipped
// instead of read as a serial. The proxied session serves exactly one device.
func firstDeviceSerial(payload string) string {
	for _, line := range strings.Split(payload, "\n") {
		serial, state, found := strings.Cut(strings.TrimSpace(line), "\t")
		if found && state != "" {
			return serial
		}
	}
	return ""
}
