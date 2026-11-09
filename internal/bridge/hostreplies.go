package bridge

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"
)

// The daemon answers some host requests from copies of the server's replies. It fetches each
// copy at attach, or on first use if the attach kept none. A held tracker uses no websocket: it
// pushes the copy, or the empty list, on every attach and detach.

// emptyDeviceList is the tracker message for no devices: zero length, no rows (transport.cpp
// device_tracker_send).
const emptyDeviceList = "0000"

// readyDeviceState is the state column of a device adb can use (adb.h kCsDevice, "device").
const readyDeviceState = "device"

// errAttachmentEnded marks a tracker fetch a detach ended; nextTrackerList snapshots again.
var errAttachmentEnded = errors.New("the attachment ended during the fetch")

// localHostQueries maps each one-reply host request, sent alone with the plain host: prefix, to
// the framed request whose cached reply answers it. As adb's handle_host_request: version,
// devices and devices-l get one reply, then the connection closes. The adb version does not
// change within an attachment.
var localHostQueries = map[string]string{
	"host:version":   hostVersionRequest,
	"host:devices":   hostDevicesRequest,
	"host:devices-l": hostDevicesLongRequest,
}

// deviceTrackers maps each device tracker, sent alone with the plain host: prefix, to the list
// request its messages come from. As adb's host_service_to_socket: OKAY, the list, then the
// connection is held; see serveDeviceTracker.
var deviceTrackers = map[string]string{
	"host:track-devices":   hostDevicesRequest,
	"host:track-devices-l": hostDevicesLongRequest,
}

// prefetchedHostRequests is every reply localHostQueries answers from; the attach fetches each.
var prefetchedHostRequests = []string{hostVersionRequest, hostDevicesRequest, hostDevicesLongRequest}

// hostReplyCache holds one attachment's copies of the server's host replies, keyed by framed
// request. A copy lives until the detach. The first request for a reply fetches it; requests
// that come during the fetch wait for it.
type hostReplyCache struct {
	fetch   func(ctx context.Context, request string) ([]byte, error)
	mutex   sync.Mutex
	entries map[string]*hostReply
}

// hostReply is one cached reply. fetched closes when its fetch ends. reply and err are set only
// before that; reply stays nil when the fetch got nothing to keep.
type hostReply struct {
	fetched chan struct{}
	reply   []byte
	err     error
}

func newHostReplyCache(fetch func(ctx context.Context, request string) ([]byte, error)) *hostReplyCache {
	return &hostReplyCache{fetch: fetch, entries: make(map[string]*hostReply)}
}

// lookup returns the cached reply to request, and fetches it on ctx first if there is none. A
// nil reply with no error means the fetch got nothing to keep. The error is the fetch's, shared
// with every request that waited on it, or ctx's when it ended first.
func (cache *hostReplyCache) lookup(ctx context.Context, request string) ([]byte, error) {
	// A detach cancels ctx; no copy may answer after it.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cache.mutex.Lock()
	entry, found := cache.entries[request]
	if !found {
		entry = &hostReply{fetched: make(chan struct{})}
		cache.entries[request] = entry
	}
	cache.mutex.Unlock()
	if !found {
		defer close(entry.fetched)
		reply, err := cache.fetch(ctx, request)
		if err == nil && keepable(request, reply) {
			entry.reply = reply
		} else {
			entry.err = err
			cache.mutex.Lock()
			delete(cache.entries, request)
			cache.mutex.Unlock()
		}
		return entry.reply, entry.err
	}
	select {
	case <-entry.fetched:
		return entry.reply, entry.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// keepable reports whether reply can answer later requests: an OKAY reply with a payload, and
// for a device list, every device ready. A device in another state may change soon, and a held
// tracker is pushed only at an attach or a detach. A fetch returns whole replies only.
func keepable(request string, reply []byte) bool {
	if len(reply) <= hostReplyHeaderLen || string(reply[:statusWordLen]) != statusOkay {
		return false
	}
	if request == hostVersionRequest {
		return true
	}
	for _, row := range strings.Split(strings.TrimSpace(string(reply[hostReplyHeaderLen:])), "\n") {
		if columns := strings.Fields(row); len(columns) < 2 || columns[1] != readyDeviceState {
			return false
		}
	}
	return true
}

// prefetch starts one fetch of every reply in prefetchedHostRequests, in parallel: one dial each,
// no retry. A reply not kept is fetched on first use. ctx is the attachment's, so a detach ends
// the fetches.
func (cache *hostReplyCache) prefetch(ctx context.Context, logger *slog.Logger) {
	for _, request := range prefetchedHostRequests {
		go func() {
			if _, err := cache.lookup(ctx, request); err != nil && ctx.Err() == nil {
				logger.Debug("could not prefetch a host reply", "request", request, "error", err)
			}
		}()
	}
}

// hostReplyFetcher fetches one attachment's host replies. It holds the ARN, so a fetch never
// reaches a device attached later.
type hostReplyFetcher struct {
	device *ActiveDevice
	dialer *WSDialer
	arn    string
}

// fetch gets one host reply on its own websocket. Like the warm socket, a fetch is the daemon's
// own and takes no device-stream slot; the cache runs at most one per reply at a time. A refused,
// malformed, missing or late reply gives nil and no error, so the request is relayed.
func (fetcher *hostReplyFetcher) fetch(ctx context.Context, request string) ([]byte, error) {
	// Read per fetch: the daemon renews the endpoint.
	endpoint, attached := fetcher.device.EndpointFor(fetcher.arn)
	if !attached {
		return nil, fmt.Errorf("%w: the device detached before the host request", errAdbConnClosed)
	}
	reply, err := fetcher.dialer.fetchHostReply(ctx, endpoint, request)
	var replyErr *hostReplyError
	if errors.As(err, &replyErr) {
		return nil, nil
	}
	return reply, err
}

// answerHostQuery answers a request in localHostQueries from the attachment's cached reply,
// fetching it first if there is none. One reply, then the close. A failed fetch closes the
// client, as a failed dial does. answered false: relay the request. Either it is not a local
// query, or the server's reply was not one to keep.
func answerHostQuery(connCtx context.Context, tcp net.Conn, clientOpening opening, features *attachmentFeatures,
	logger *slog.Logger) (answered bool) {
	// A request behind a select is for the device: relay it.
	request, local := localHostQueries[clientOpening.service]
	if features == nil || !local || clientOpening.selected {
		return false
	}
	// Nothing reads the client here, so only a detach or the daemon's shutdown cancels connCtx,
	// and with it this fetch or this wait on another's.
	reply, err := features.hostReplies.lookup(connCtx, request)
	if err != nil {
		logFetchFailure(connCtx, err, logger)
		return true
	}
	if reply == nil {
		return false
	}
	// No pump watches this socket; deadline stops a client that asks, then stops reading.
	_ = tcp.SetWriteDeadline(time.Now().Add(openingRequestTimeout))
	if _, err := tcp.Write(reply); err != nil {
		logger.Debug("could not answer a host request", "error", err)
	}
	return true
}

// logFetchFailure logs a host reply fetch that failed. One ended by ctx (a detach or the
// daemon's shutdown) is a normal end, logged at Debug as the relay does.
func logFetchFailure(ctx context.Context, err error, logger *slog.Logger) {
	if ctx.Err() != nil {
		logger.Debug("host reply fetch ended with the connection; closing client", "error", err)
		return
	}
	logger.Error("failed to fetch a host reply; closing client", "error", err)
}

// serveDeviceTracker answers a request in deviceTrackers and holds it, as adb does, with or
// without a device. OKAY, then the current list; every attach and detach pushes it again. The
// list is the device's cached one, fetched when none is kept, or the empty list with no device.
// ctx is the daemon's, not the connection's: a detach cancels the connection's, and the tracker
// outlives the detach.
//
// The hold ends when the client closes or writes (adb closes a tracker its client writes to, also
// when the bytes came with the request), when the daemon stops, when the device's list cannot be
// copied, or when a push is not read within openingRequestTimeout. On served true the caller
// closes tcp, which ends the one-byte reader.
//
// served false: relay the request. Either it is not a lone tracker, or its first list is not one
// to keep, and the server's own tracker then reports the device's state changes. Only the first
// answer can relay: a held tracker whose next list is not kept ends, so the client's next tracker
// is relayed.
func serveDeviceTracker(ctx context.Context, tcp net.Conn, clientOpening opening, device *ActiveDevice,
	logger *slog.Logger) (served bool) {
	listRequest, tracker := deviceTrackers[clientOpening.service]
	if !tracker || clientOpening.selected {
		return false
	}
	holdCtx, endHold := context.WithCancel(ctx)
	defer endHold()
	// The hold's end closes the socket under a blocked write or the read below.
	defer closeOnCancel(holdCtx, tcp)()

	list, changed, err := nextTrackerList(holdCtx, device, listRequest)
	if err != nil {
		logFetchFailure(holdCtx, err, logger)
		return true
	}
	if list == nil {
		return false
	}
	if !pushList(tcp, statusOkay+string(list), logger) {
		return true
	}
	if len(clientOpening.pending) != replyLengthDigits+len(clientOpening.service) {
		return true
	}
	go func() {
		_, _ = tcp.Read(make([]byte, 1))
		endHold()
	}()
	logger.Debug("holding a device tracker", "service", clientOpening.service)

	for {
		select {
		case <-changed:
		case <-holdCtx.Done():
			return true
		}
		if list, changed, err = nextTrackerList(holdCtx, device, listRequest); err != nil {
			logFetchFailure(holdCtx, err, logger)
			return true
		}
		if list == nil || !pushList(tcp, string(list), logger) {
			return true
		}
	}
}

// nextTrackerList is a held tracker's next message and the signal the next attach or detach
// closes, from one snapshot: the empty list with no device, nil with no dialer, else the device's
// cached list (fetchTrackerList). A detach that ends the fetch snapshots again.
func nextTrackerList(holdCtx context.Context, device *ActiveDevice, listRequest string) ([]byte, <-chan struct{}, error) {
	for {
		features, present, changed := device.snapshot()
		if !present {
			return []byte(emptyDeviceList), changed, nil
		}
		if features == nil {
			return nil, changed, nil
		}
		list, err := fetchTrackerList(holdCtx, features, listRequest)
		if errors.Is(err, errAttachmentEnded) && holdCtx.Err() == nil {
			continue
		}
		return list, changed, err
	}
}

// pushList writes one tracker message. No pump watches this socket, so the deadline stops a
// client that stops reading. false: the write failed.
func pushList(tcp net.Conn, message string, logger *slog.Logger) bool {
	_ = tcp.SetWriteDeadline(time.Now().Add(openingRequestTimeout))
	if _, err := io.WriteString(tcp, message); err != nil {
		logger.Debug("could not push a device list to a tracker", "error", err)
		return false
	}
	return true
}

// fetchTrackerList is the attached device's cached list with its status word cut, the protocol
// string adb's device_tracker_send writes, fetched when none is kept. nil and no error: the list
// is not one to keep. The fetch runs on the attachment's context, never the hold's: a lookup is
// shared with every waiter on the same reply, and a client that leaves must not fail theirs. The
// hold only stops waiting; errAttachmentEnded marks a fetch the detach ended.
func fetchTrackerList(holdCtx context.Context, features *attachmentFeatures, listRequest string) ([]byte, error) {
	type lookedUp struct {
		reply []byte
		err   error
	}
	results := make(chan lookedUp, 1)
	go func() {
		reply, err := features.hostReplies.lookup(features.ctx, listRequest)
		results <- lookedUp{reply: reply, err: err}
	}()
	select {
	case result := <-results:
		switch {
		case result.err != nil && features.ctx.Err() != nil:
			return nil, errAttachmentEnded
		case result.err != nil || result.reply == nil:
			return nil, result.err
		}
		return result.reply[statusWordLen:], nil
	case <-holdCtx.Done():
		return nil, holdCtx.Err()
	}
}
