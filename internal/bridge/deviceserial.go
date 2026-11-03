package bridge

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

// hostDevicesRequest is the ADB smart-socket request for "list devices": a
// 4-hex-digit length prefix (000c = 12 bytes) plus "host:devices".
const hostDevicesRequest = "000chost:devices"

// defaultSerialReplyTimeout bounds the wait for the devices reply, so a wedged server cannot
// hold the fetch. Source: pick.
const defaultSerialReplyTimeout = 5 * time.Second

// maxDevicesReplyBytes bounds what the fetch accumulates across messages, so a length
// header the server never satisfies ends the read. A devices reply is one short line
// per device; one message is already capped by maxInboundMessageBytes. Source: pick.
const maxDevicesReplyBytes = 64 * 1024

// replyLengthDigits is the width of the hex length prefix adb puts on a reply payload. Source:
// adb_io.cpp SendProtocolString.
const replyLengthDigits = 4

var (
	errNoDeviceSerial      = errors.New("the remote adb server lists no device")
	errDevicesReplyTooLong = errors.New("the devices reply runs past its byte cap")
)

// FetchDeviceSerial dials endpoint, sends one host:devices and returns the first device row's
// serial. The control add calls it before Set, so a bound device always has a serial. ctx
// bounds the dial; ctx and serialReplyTimeout bound the exchange.
//
// The exchange runs on its own goroutine: x/net's Send and Close wait behind a stalled write
// for sendDeadline, which ctx cannot cut, and the add runs on the inline control loop. The
// reply gets one timer over the whole read, not one per Receive as receiveWithin does: a server
// trickling a byte at a time would hold the fetch forever. A late caller closes the websocket
// on a goroutine of its own, which unblocks the reader; the exchange then ends by itself. One
// Close runs, whichever side gets there first.
func (dialer *WSDialer) FetchDeviceSerial(ctx context.Context, endpoint string) (string, error) {
	ws, err := dialer.Dial(ctx, endpoint, RouteAdb)
	if err != nil {
		return "", err
	}
	var closeOnce sync.Once
	closeWS := func() { closeOnce.Do(func() { ws.Close() }) }
	type exchangeResult struct {
		serial string
		err    error
	}
	results := make(chan exchangeResult, 1)
	go func() {
		defer closeWS()
		if sendErr := ws.Send([]byte(hostDevicesRequest)); sendErr != nil {
			results <- exchangeResult{err: fmt.Errorf("send host:devices: %w", sendErr)}
			return
		}
		serial, readErr := readDevicesSerial(ws)
		results <- exchangeResult{serial: serial, err: readErr}
	}()
	timer := time.NewTimer(dialer.serialReplyTimeout)
	defer timer.Stop()
	select {
	case result := <-results:
		return result.serial, result.err
	case <-timer.C:
		go closeWS()
		return "", fmt.Errorf("await the devices reply: %w", context.DeadlineExceeded)
	case <-ctx.Done():
		go closeWS()
		return "", ctx.Err()
	}
}

// readDevicesSerial consumes the fetch connection until the devices reply parses or the
// connection ends. The first message is the server's session control frame and is
// skipped when present; the adb reply behind it may arrive split across messages. A first
// message that is a control frame this client cannot read, such as another wire version's,
// fails the fetch with the decoder's reason.
func readDevicesSerial(ws WSConn) (string, error) {
	var reply []byte
	firstMessage := true
	for len(reply) <= maxDevicesReplyBytes {
		message, err := ws.Receive()
		if err != nil {
			return "", fmt.Errorf("read the devices reply: %w", err)
		}
		if firstMessage {
			firstMessage = false
			_, decodeErr := unmarshalControlFrame(message)
			if decodeErr == nil {
				continue
			}
			// Law 1 returns the bare sentinel for adb bytes with no magic: the reply itself.
			if decodeErr != errNotControlFrame {
				return "", fmt.Errorf("read the session control frame: %w", decodeErr)
			}
		}
		reply = append(reply, message...)
		if serial, done, parseErr := parseHostDevicesSerial(reply); done {
			return serial, parseErr
		}
	}
	return "", errDevicesReplyTooLong
}

// parseHostDevicesSerial parses an accumulated host:devices reply: OKAY plus a
// 4-hex-length-prefixed payload of "<serial>\t<state>" lines. done is false while the
// reply is incomplete. A reply that is not an OKAY list ends the parse with an error that
// names the problem; an OKAY list with no device row ends it with errNoDeviceSerial.
func parseHostDevicesSerial(reply []byte) (serial string, done bool, err error) {
	if len(reply) < statusWordLen {
		return "", false, nil
	}
	if status := string(reply[:statusWordLen]); status != statusOkay {
		return "", true, fmt.Errorf("the remote adb server answered %s instead of OKAY", status)
	}
	const headerLen = statusWordLen + replyLengthDigits
	if len(reply) < headerLen {
		return "", false, nil
	}
	payloadLen, parseErr := strconv.ParseUint(string(reply[statusWordLen:headerLen]), 16, 32)
	if parseErr != nil {
		return "", true, fmt.Errorf("malformed devices reply: length %q", reply[statusWordLen:headerLen])
	}
	payloadEnd := uint64(headerLen) + payloadLen
	if uint64(len(reply)) < payloadEnd {
		return "", false, nil
	}
	serial = firstDeviceSerial(string(reply[headerLen:payloadEnd]))
	if serial == "" {
		return "", true, errNoDeviceSerial
	}
	return serial, true, nil
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
