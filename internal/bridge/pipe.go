package bridge

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

const (
	// outboundChunkSize bounds each websocket message emitted from the local TCP
	// stream. Must stay well under maxInboundMessageBytes and its remote limits.
	// Message boundaries carry no meaning: both ends terminate at a real adb server
	// that re-frames from the byte stream.
	outboundChunkSize = 32 << 10 // 32 KiB

	// maxInboundMessageBytes caps a single inbound websocket message. Matches adb's
	// MAX_PAYLOAD and sits under the server's message-size limit. Per-message, not
	// per-transfer: a multi-MB adb push arrives as many small messages.
	maxInboundMessageBytes = 1 << 20 // 1 MiB
)

// pipeConn runs a full-duplex byte pipe between a local adb TCP connection and ws. It
// returns when either side closes or ctx is cancelled, with both connections closed.
//
// aux is an optional goroutine (the refresher) released by the same done channel as the
// context watcher, so it cannot outlive pipeConn. A nil aux runs nothing.
//
// Returns nil for any clean shutdown, non-nil only for a genuine fault such as an
// oversized inbound message (websocket.ErrFrameTooLarge).
func pipeConn(ctx context.Context, tcp net.Conn, ws WSConn, aux func(done <-chan struct{})) error {
	var closeOnce sync.Once
	closeBoth := func() {
		ws.Close()
		tcp.Close()
	}

	// x/net has no context-aware Read/Write, so a watcher tears both halves down
	// when the context ends, unblocking the pumps. Its defer-closed release channel
	// guarantees it exits with pipeConn.
	watcherDone := make(chan struct{})
	defer close(watcherDone)
	go func() {
		select {
		case <-ctx.Done():
			closeOnce.Do(closeBoth)
		case <-watcherDone:
		}
	}()

	if aux != nil {
		go aux(watcherDone)
	}

	var wg sync.WaitGroup
	wg.Add(2)

	var outboundErr, inboundErr error
	go func() {
		defer wg.Done()
		defer closeOnce.Do(closeBoth)
		if err := pumpOutbound(tcp, ws); err != nil {
			outboundErr = fmt.Errorf("outbound (tcp->ws): %w", err)
		}
	}()
	go func() {
		defer wg.Done()
		defer closeOnce.Do(closeBoth)
		if err := pumpInbound(ws, tcp); err != nil {
			inboundErr = fmt.Errorf("inbound (ws->tcp): %w", err)
		}
	}()
	wg.Wait()

	// A context end (SIGTERM or a device remove) is a normal end-of-life; the pump
	// errors in that case are just teardown noise.
	if ctx.Err() != nil {
		return nil
	}
	// Otherwise surface the first genuine fault. The %w wrap above preserves
	// errors.Is, so the classification still works.
	for _, err := range []error{inboundErr, outboundErr} {
		if !isBenignPipeEnd(err) {
			return err
		}
	}
	return nil
}

// writeWithin writes data to conn under a deadline; empty data writes nothing. Callers use
// it before the pipe starts. A peer that stopped reading is not a cancel, so no watcher
// ends the write; only the deadline keeps it from parking them for good.
func writeWithin(conn net.Conn, data []byte, within time.Duration) error {
	if len(data) == 0 {
		return nil
	}
	_ = conn.SetWriteDeadline(time.Now().Add(within))
	_, err := conn.Write(data)
	_ = conn.SetWriteDeadline(time.Time{})
	return err
}

// pumpOutbound copies the local TCP stream to the websocket, one binary message
// per read, each at most outboundChunkSize.
func pumpOutbound(tcp net.Conn, ws WSConn) error {
	buf := make([]byte, outboundChunkSize)
	for {
		n, readErr := tcp.Read(buf)
		if n > 0 {
			if sendErr := ws.Send(buf[:n]); sendErr != nil {
				return sendErr
			}
		}
		if readErr != nil {
			return readErr
		}
	}
}

// pumpInbound copies inbound websocket messages to the local TCP stream. x/net stashes
// (does not drain) an oversized frame, so ErrFrameTooLarge is fatal: stop the pump.
//
// It is adbConn's single reader.
func pumpInbound(ws WSConn, tcp net.Conn) error {
	for {
		message, err := ws.Receive()
		if err != nil {
			return err
		}
		if _, err := tcp.Write(message); err != nil {
			return err
		}
	}
}

// isBenignCloseError reports whether err represents an expected, clean end of
// the pipe rather than a fault worth surfacing. x/net surfaces a peer close as
// io.EOF (it discards the numeric close code), and tearing down one half makes
// the other half's in-flight Read/Write fail with net.ErrClosed; both are
// normal. websocket.ErrFrameTooLarge is deliberately NOT benign.
func isBenignCloseError(err error) bool {
	if err == nil {
		return true
	}
	return errors.Is(err, io.EOF) ||
		errors.Is(err, net.ErrClosed) ||
		errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded)
}

// isBenignPipeEnd reports whether err is a clean end of the whole pipe: an expected
// close, or a peer reset (errPeerReset, one errno per OS), which an adb client that
// aborts can send. The reset stays out of isBenignCloseError: awaitSuccessor uses that
// as the expected-cut test of a refresh, and a reset there may have cut bytes short.
func isBenignPipeEnd(err error) bool {
	return isBenignCloseError(err) || errors.Is(err, errPeerReset)
}
