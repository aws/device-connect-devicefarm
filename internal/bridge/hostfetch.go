package bridge

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Framed host requests. The reply cache fetches each; FetchDeviceSerial sends hostDevicesRequest
// too. See localHostQueries.
const (
	hostVersionRequest     = "000chost:version"
	hostDevicesRequest     = "000chost:devices"
	hostDevicesLongRequest = "000ehost:devices-l"
	hostFeaturesRequest    = "000dhost:features"
)

// defaultHostReplyTimeout bounds the wait for a host reply, so a wedged server cannot hold a
// fetch. Source: pick.
const defaultHostReplyTimeout = 5 * time.Second

// maxHostReplyBytes bounds what a fetch accumulates across messages, so a length header the
// server never satisfies ends the read. A devices reply is one short line per device; one
// message is already capped by maxInboundMessageBytes. Source: pick.
const maxHostReplyBytes = 64 * 1024

// replyLengthDigits is the width of the hex length prefix adb puts on a request and on a reply
// payload. Source: adb_io.cpp SendProtocolString.
const replyLengthDigits = 4

// hostReplyHeaderLen is a host reply's fixed head: the status word, then the payload length.
const hostReplyHeaderLen = statusWordLen + replyLengthDigits

// hostReplyError is a fetch whose reply was refused, malformed or cut short, or did not come
// within hostReplyTimeout. The reply cache relays such a request; see hostReplyFetcher.
type hostReplyError struct{ cause error }

func (replyErr *hostReplyError) Error() string { return replyErr.cause.Error() }

func (replyErr *hostReplyError) Unwrap() error { return replyErr.cause }

// fetchHostReply dials endpoint, sends one framed host request and returns the whole OKAY reply.
// A reply that is not one gives a *hostReplyError naming the problem. ctx bounds the dial; ctx
// and hostReplyTimeout bound the exchange.
//
// The exchange runs on its own goroutine: x/net's Send and Close wait behind a stalled write
// for sendDeadline, which ctx cannot cut, and the add runs on the inline control loop. The
// reply gets one timer over the whole read, not one per Receive as receiveWithin does: a server
// trickling a byte at a time would hold the fetch forever. A late caller closes the websocket
// on a goroutine of its own, which unblocks the reader; the exchange then ends by itself. One
// Close runs, whichever side gets there first.
func (dialer *WSDialer) fetchHostReply(ctx context.Context, endpoint, request string) ([]byte, error) {
	service := request[replyLengthDigits:]
	// Errors name the reply as adb's commands do: "devices", "version".
	replyName := strings.TrimPrefix(service, "host:")
	ws, err := dialer.Dial(ctx, endpoint, RouteAdb)
	if err != nil {
		return nil, err
	}
	var closeOnce sync.Once
	closeWS := func() { closeOnce.Do(func() { ws.Close() }) }
	type exchangeResult struct {
		reply []byte
		err   error
	}
	results := make(chan exchangeResult, 1)
	go func() {
		defer closeWS()
		if sendErr := ws.Send([]byte(request)); sendErr != nil {
			results <- exchangeResult{err: fmt.Errorf("send %s: %w", service, sendErr)}
			return
		}
		reply, readErr := readHostReply(ws, replyName)
		if readErr != nil {
			readErr = &hostReplyError{cause: readErr}
		}
		results <- exchangeResult{reply: reply, err: readErr}
	}()
	timer := time.NewTimer(dialer.hostReplyTimeout)
	defer timer.Stop()
	select {
	case result := <-results:
		return result.reply, result.err
	case <-timer.C:
		go closeWS()
		return nil, &hostReplyError{cause: fmt.Errorf("await the %s reply: %w", replyName, context.DeadlineExceeded)}
	case <-ctx.Done():
		go closeWS()
		return nil, ctx.Err()
	}
}

// readHostReply consumes the fetch connection until the host reply is whole or the connection
// ends. The first message is the server's session control frame and is skipped when present;
// the adb reply behind it may arrive split across messages. A first message that is a control
// frame this client cannot read, such as another wire version's, fails the fetch with the
// decoder's reason.
func readHostReply(ws WSConn, replyName string) ([]byte, error) {
	var reply []byte
	firstMessage := true
	for len(reply) <= maxHostReplyBytes {
		message, err := ws.Receive()
		if err != nil {
			return nil, fmt.Errorf("read the %s reply: %w", replyName, err)
		}
		if firstMessage {
			firstMessage = false
			_, decodeErr := unmarshalControlFrame(message)
			if decodeErr == nil {
				continue
			}
			// Law 1 returns the bare sentinel for adb bytes with no magic: the reply itself.
			if decodeErr != errNotControlFrame {
				return nil, fmt.Errorf("read the session control frame: %w", decodeErr)
			}
		}
		reply = append(reply, message...)
		if replyLength, done, parseErr := hostReplyLength(reply, replyName); done {
			if parseErr != nil {
				return nil, parseErr
			}
			return reply[:replyLength], nil
		}
	}
	return nil, fmt.Errorf("the %s reply runs past its byte cap", replyName)
}

// hostReplyLength measures an accumulated host reply: OKAY, a 4-hex length, the payload
// (adb_io.cpp SendOkay, SendProtocolString). done false: incomplete. done true: length is the
// whole reply's byte count, or err names why the reply is no OKAY reply.
func hostReplyLength(reply []byte, replyName string) (length int, done bool, err error) {
	if len(reply) < statusWordLen {
		return 0, false, nil
	}
	if status := string(reply[:statusWordLen]); status != statusOkay {
		return 0, true, fmt.Errorf("the remote adb server answered %s instead of OKAY", status)
	}
	if len(reply) < hostReplyHeaderLen {
		return 0, false, nil
	}
	lengthField := reply[statusWordLen:hostReplyHeaderLen]
	payloadLen, parseErr := strconv.ParseUint(string(lengthField), 16, 16)
	if parseErr != nil {
		return 0, true, fmt.Errorf("malformed %s reply: length %q", replyName, lengthField)
	}
	replyLen := hostReplyHeaderLen + int(payloadLen)
	if len(reply) < replyLen {
		return 0, false, nil
	}
	return replyLen, true, nil
}
