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
// copy at attach, or on first use if the attach kept none. A held tracker uses no websocket.

// emptyDeviceList is the tracker message for no devices: zero length, no rows (transport.cpp
// device_tracker_send).
const emptyDeviceList = "0000"

// readyDeviceState is the state column of a device adb can use (adb.h kCsDevice, "device").
const readyDeviceState = "device"

// hostQuery says how to answer one host request. request fetches the reply; tracker holds the
// connection open after it.
type hostQuery struct {
	request string
	tracker bool
}

// localHostQueries maps each host request the daemon answers itself, sent alone with the plain
// host: prefix, to its query. As in adb: handle_host_request answers version, devices and
// devices-l, then closes; host_service_to_socket opens a tracker for track-devices and
// track-devices-l, whose first message after OKAY is the same list. The adb version does not
// change within an attachment.
var localHostQueries = map[string]hostQuery{
	"host:version":         {request: hostVersionRequest},
	"host:devices":         {request: hostDevicesRequest},
	"host:devices-l":       {request: hostDevicesLongRequest},
	"host:track-devices":   {request: hostDevicesRequest, tracker: true},
	"host:track-devices-l": {request: hostDevicesLongRequest, tracker: true},
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
// tracker gets no update. A fetch returns whole replies only.
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
// fetching it first if there is none. A tracker is then held; see holdDeviceTracker. A failed
// fetch closes the client, as a failed dial does. answered false: relay the request. Either it
// is not a local query, or the server's reply was not one to keep.
func answerHostQuery(connCtx context.Context, tcp net.Conn, clientOpening opening, features *attachmentFeatures,
	logger *slog.Logger) (answered bool) {
	// A request behind a select is for the device: relay it.
	query, local := localHostQueries[clientOpening.service]
	if features == nil || !local || clientOpening.selected {
		return false
	}
	// Nothing reads the client here, so only a detach or the daemon's shutdown cancels connCtx,
	// and with it this fetch or this wait on another's.
	reply, err := features.hostReplies.lookup(connCtx, query.request)
	if err != nil {
		// Detach or shutdown: a normal end, logged at Debug as the relay does.
		if connCtx.Err() != nil {
			logger.Debug("host reply fetch ended with the connection; closing client", "error", err)
		} else {
			logger.Error("failed to fetch a host reply; closing client", "error", err)
		}
		return true
	}
	if reply == nil {
		return false
	}
	// No pump watches this socket; deadline stops a client that asks, then stops reading.
	_ = tcp.SetWriteDeadline(time.Now().Add(openingRequestTimeout))
	if _, err := tcp.Write(reply); err != nil {
		logger.Debug("could not answer a host request", "error", err)
		return true
	}
	// adb closes a tracker its client writes to, also when the bytes came with the request.
	if query.tracker && len(clientOpening.pending) == replyLengthDigits+len(clientOpening.service) {
		holdDeviceTracker(connCtx, tcp, features.ctx)
	}
	return true
}

// holdDeviceTracker keeps an answered tracker open, as adb does, but sends no update while
// attached. When the attachment ends, the client gets the empty list, then the close. adb closes
// a tracker its client writes to, so any read ends the hold, as does daemon shutdown.
func holdDeviceTracker(connCtx context.Context, tcp net.Conn, attachment context.Context) {
	clientDone := make(chan struct{})
	go func() {
		_, _ = tcp.Read(make([]byte, 1))
		close(clientDone)
	}()
	select {
	case <-attachment.Done():
	case <-connCtx.Done():
	case <-clientDone:
		return
	}
	// A detach also cancels connCtx, so either case may wake first; check the attachment.
	if attachment.Err() != nil {
		_ = tcp.SetWriteDeadline(time.Now().Add(openingRequestTimeout))
		_, _ = io.WriteString(tcp, emptyDeviceList)
	}
}
