package bridge

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"time"

	"github.com/aws/device-connect-devicefarm/internal/adbwire"
	"github.com/aws/device-connect-devicefarm/internal/forward"
)

// The accept seam: what the daemon reads from a local adb client before it dials, and
// what it does with a declined request.

// syntheticTransportID is the transport id the daemon reports for its one device. The
// daemon's own value; adb numbers transports from a counter (transport.cpp). Only a
// tport: select carries the id in its reply. Nonzero: adb reads 0 as unset.
// The same on every connection: a client reads it here and may pass it back as `-t <id>`
// on the next one.
const syntheticTransportID = 1

// openingRequestTimeout bounds the wait for the requests that come before the dial. Not
// for a failed adb forward: a real client writes select and service back to back, so it
// never fires for one. It ends a local process that connects and sends a partial request,
// a select with no service, or nothing: there is no request to answer, only wait or close.
// Chosen, not derived.
const openingRequestTimeout = 30 * time.Second

// errSelectRefused means the client named a device the attachment does not hold and
// already has its FAIL, so there is nothing left to serve it.
var errSelectRefused = errors.New("the transport select named another device")

// requestScanner reassembles smart-socket requests from a byte stream and keeps every
// byte: RequestReader drops what it consumes, and a declined request must reach the
// device exactly as the client framed it.
type requestScanner struct {
	reader adbwire.RequestReader
	fed    []byte
}

func (scanner *requestScanner) feed(data []byte) {
	scanner.fed = append(scanner.fed, data...)
	scanner.reader.Feed(data)
}

// pending is every byte read so far that the device must still receive.
func (scanner *requestScanner) pending() []byte {
	return scanner.fed
}

// dropAnswered hides the request the daemon answered itself, which is everything the
// reader has consumed so far.
func (scanner *requestScanner) dropAnswered() {
	scanner.fed = scanner.fed[len(scanner.fed)-scanner.reader.Buffered():]
}

// readRequest reads from reader until one whole request is complete, and returns its service.
// An empty service with no error means the bytes are not a smart-socket request: the
// parser fails closed, so the daemon relays them untouched and reads no further request.
// An error means no request completed, whether the client closed, the read failed, or the
// socket was cut under the reader.
func (scanner *requestScanner) readRequest(reader io.Reader) (service string, err error) {
	readBuffer := make([]byte, outboundChunkSize)
	for {
		payload, scanErr := scanner.reader.Next()
		if scanErr != nil {
			return "", nil
		}
		if payload != nil {
			return string(payload), nil
		}
		readCount, readErr := reader.Read(readBuffer)
		if readCount > 0 {
			scanner.feed(readBuffer[:readCount])
			continue
		}
		// No bytes and no error would spin here forever. No net.Conn does it.
		if readErr == nil {
			readErr = io.ErrNoProgress
		}
		return "", readErr
	}
}

// opening is everything one client said before anything was dialed.
type opening struct {
	// service is the request the client came for: the whole request when it came alone,
	// prefix and all, or the bare service when it followed a select. Empty when its bytes
	// are not a smart-socket request.
	service string
	// selector is what an answered select named. It comes from the select, not from
	// service, which is stored bare behind one. The zero value is the any selector, which
	// the attached device always matches.
	selector adbwire.AdbDeviceSelector
	// pending is every byte the device must receive, an answered select excluded.
	pending []byte
	// selected says a select was answered, so the session still needs its own switch.
	selected bool
}

// requestedService is the service the client came for and the selector it named: the select's
// when one was answered, else the request's own host prefix. ok is false for a lone request with
// no parsable host prefix.
func (clientOpening opening) requestedService() (selector adbwire.AdbDeviceSelector, service string, ok bool) {
	if clientOpening.selected {
		return clientOpening.selector, clientOpening.service, true
	}
	return adbwire.SplitHostPrefix(clientOpening.service)
}

// unwrappedService is the service behind an answered select or a parsable host prefix, else the
// raw request. The reverse-off refusal and the rule screen both read it, so they classify alike.
func (clientOpening opening) unwrappedService() string {
	if _, service, ok := clientOpening.requestedService(); ok {
		return service
	}
	return clientOpening.service
}

// readOpening reads everything the client says before anything is dialed: its service
// request, plus the transport select the daemon answers itself when one comes first.
//
// Nothing is dialed from here, so a client that says nothing mints no session.
func readOpening(ctx context.Context, tcpConn net.Conn, deviceSerial string) (clientOpening opening, err error) {
	// The watcher below is a goroutine, so a cancel that already landed would not stop the
	// first read.
	if err := ctx.Err(); err != nil {
		return opening{}, err
	}
	// A blocked read outlives its context, so a cancel must close the socket under it.
	release := closeOnCancel(ctx, tcpConn)
	defer release()
	// Both directions: the replies from here go out before any pump watches the socket.
	_ = tcpConn.SetDeadline(time.Now().Add(openingRequestTimeout))
	defer func() { _ = tcpConn.SetDeadline(time.Time{}) }()

	var scanner requestScanner
	service, err := scanner.readRequest(tcpConn)
	if err != nil {
		return opening{}, err
	}
	selector, selected, err := answerTransportSelect(tcpConn, service, deviceSerial)
	if err != nil {
		return opening{}, err
	}
	if selected {
		// A client says what it came for only after its select is answered. adb keeps the
		// plain `host:` prefix on the forward family behind it (`host:forward:...`); the
		// select named the device, so the prefix goes, as adb's host server drops it.
		scanner.dropAnswered()
		if service, err = scanner.readRequest(tcpConn); err != nil {
			return opening{}, err
		}
		service = strings.TrimPrefix(service, "host:")
	}
	return opening{service: service, selector: selector, pending: scanner.pending(), selected: selected}, nil
}

// answerTransportSelect answers request itself when it is a transport select, which a
// client sends ahead of the service it came for; only the daemon knows which device the
// attachment holds. claimed means the reply went out, so the service is a second request
// on this connection and selector is what it named. An error ends the connection: the
// reply would not write, or the select named another device and now has its FAIL.
//
// The reply shape follows the select's spelling. A tport: form waits for OKAY plus the
// transport id; transport-id:<n> takes a bare OKAY. Which spelling adb sends, and before
// which services, is at adbwire.SplitHostPrefix. An empty deviceSerial matches every
// selector.
func answerTransportSelect(writer io.Writer, request, deviceSerial string) (selector adbwire.AdbDeviceSelector,
	claimed bool, err error) {
	// adb's client puts every select behind the plain `host:` prefix and names its device
	// in the service (adb_client.cpp switch_socket_transport).
	// Any other prefix carries a selector this seam does not read, so it goes to the
	// device verbatim rather than being answered on a guess.
	prefix, service, ok := adbwire.SplitHostPrefix(request)
	if !ok || prefix.Kind != adbwire.AdbSelectorAny {
		return adbwire.AdbDeviceSelector{}, false, nil
	}
	selector, carriesID, ok := adbwire.ParseTransportSelect(service)
	if !ok {
		return adbwire.AdbDeviceSelector{}, false, nil
	}
	if reason, selectorMissed := forward.SelectorMiss(selector, deviceSerial); selectorMissed {
		if writeErr := adbwire.WriteFail(writer, reason); writeErr != nil {
			return adbwire.AdbDeviceSelector{}, false, fmt.Errorf("refuse the transport select: %w", writeErr)
		}
		return adbwire.AdbDeviceSelector{}, false, fmt.Errorf("%w: %s", errSelectRefused, reason)
	}

	var writeErr error
	if carriesID {
		writeErr = adbwire.WriteTransportSelect(writer, syntheticTransportID)
	} else {
		writeErr = adbwire.WriteOkay(writer)
	}
	if writeErr != nil {
		return adbwire.AdbDeviceSelector{}, false, fmt.Errorf("answer the transport select: %w", writeErr)
	}
	return selector, true, nil
}

// respondForwardLocally answers the forward family from the daemon's own table, so a rule
// and its listening socket live here and a forward request itself never reaches the
// network. answered means the client has its reply and nothing was dialed; everything
// else is declined for the byte path (`reverse:*` too, while reverse is on).
//
// A malformed forward service is answered here too, with adb's own FAIL text: a local
// adb refuses it without reaching a device, and so must this.
func respondForwardLocally(tcpConn net.Conn, clientOpening opening, forwarding *attachmentFeatures,
	deviceSerial string, logger *slog.Logger) (answered bool) {
	// No features (no dialer), nothing to answer from.
	if forwarding == nil {
		return false
	}
	selector, service, ok := clientOpening.requestedService()
	if !ok {
		return false
	}
	request, isForward, parseErr := adbwire.ParseForwardService(service)
	if !isForward {
		return false
	}
	// No pump watches this socket yet, so the deadline is what stops a client that asked
	// and then stopped reading. The connection ends either way once the reply is out.
	_ = tcpConn.SetWriteDeadline(time.Now().Add(openingRequestTimeout))
	// The forwarding is a snapshot from before the client spoke; a detach cannot reach it, so
	// the seam checks. adb with no device refuses what acquires a transport: forward: and
	// killforward:, malformed or not; the table's Close catches an install that gets past.
	// killforward-all and list-forward acquire none and answer from the emptied table.
	acquiresTransport := parseErr != nil || request.Action == adbwire.AdbActionForward ||
		request.Action == adbwire.AdbActionKillForward
	if forwarding.ctx.Err() != nil && acquiresTransport {
		if writeErr := adbwire.WriteFail(tcpConn, adbwire.MsgNoDevices); writeErr != nil {
			logger.Debug("could not refuse a forward request for a detached device", "error", writeErr)
		}
		return true
	}
	if parseErr != nil {
		// adb acquires the transport before it reads a request's pieces (see
		// adbwire.ParseForwardService), so a device miss outranks a malformed request.
		// Only forward: and killforward: can be malformed, and both acquire one.
		reason := parseErr.Error()
		if missReason, selectorMissed := forward.SelectorMiss(selector, deviceSerial); selectorMissed {
			reason = missReason
		}
		if writeErr := adbwire.WriteFail(tcpConn, reason); writeErr != nil {
			logger.Debug("could not refuse a malformed forward request", "error", writeErr)
		}
		return true
	}
	if respondErr := forward.Respond(tcpConn, selector, request, deviceSerial, forwarding.table); respondErr != nil {
		logger.Debug("could not answer a forward request", "error", respondErr)
	}
	return true
}

// msgReverseOff is the FAIL a reverse request gets while reverse is off. adb prints it after
// `error: `, so it names the flag that turns reverse on.
const msgReverseOff = "adb reverse is off for this client; restart it with --allow-adb-reverse"

// reverseServicePrefix opens every request of adb's reverse family.
const reverseServicePrefix = "reverse:"

// refuseReverseFamily answers every `reverse:` request the seam reads with a FAIL while reverse
// is off; real adb has no such setting. It keys on the prefix alone, so a malformed request is
// refused too. One behind a select the seam declined, or alone behind a host prefix it cannot
// parse, rides the byte pipe to the server, which denies reverse with no notifier. It runs after
// the forward family and before the budget and the rule screen, so a refusal spends no slot and
// counts no rule. answered means the client has its FAIL and the connection is done.
func refuseReverseFamily(tcpConn net.Conn, clientOpening opening, forwarding *attachmentFeatures,
	logger *slog.Logger) (answered bool) {
	if forwarding == nil || forwarding.reverseEnabled {
		return false
	}
	if !strings.HasPrefix(clientOpening.unwrappedService(), reverseServicePrefix) {
		return false
	}
	refuseOpening(tcpConn, msgReverseOff, "could not refuse a reverse request", logger)
	return true
}

// msgReverseNotReady is the FAIL a reverse request gets when the attachment's clear of the
// server's reverse rules outlasts the request's hold. adb prints it after `error: `.
const msgReverseNotReady = "the device's reverse rules are not ready yet; run the command again"

// msgReverseSlotHeld is the FAIL a reverse request gets while the server's notifier slot for the
// device is held elsewhere: relayed, it would act on another client's rules, or on rules no live
// notifier here serves. The refusal payload is the same for both holders, so the text names both.
// adb prints it after `error: `.
const msgReverseSlotHeld = "adb reverse on this device is held by another adb client, or by this client's previous connection for a short while; stop the other client or retry later"

// awaitServerRulesCleared holds a `reverse:` request until the attachment's clear of the server's
// reverse rules settles, so the clear cannot wipe an install sent first. It keys on the prefix
// alone, as refuseReverseFamily does, so `list-forward` waits too and lists the cleared rules. It
// runs after the reverse-off refusal and before the budget and the rule screen, so a held request
// spends no slot and counts no rule. Slot held elsewhere: refuse at once, gate open or not, and
// again at the bound. Past bound: local FAIL, no dial; a relay would race a clear still to come. A
// gate opened by the attachment's end, or a connection canceled while held, gets no reply. proceed
// false means the connection is done.
func awaitServerRulesCleared(ctx context.Context, tcpConn net.Conn, clientOpening opening,
	forwarding *attachmentFeatures, bound time.Duration, logger *slog.Logger) (proceed bool) {
	if forwarding == nil || !strings.HasPrefix(clientOpening.unwrappedService(), reverseServicePrefix) {
		return true
	}
	if refuseWhileSlotHeld(tcpConn, clientOpening, forwarding, logger) {
		return false
	}
	boundTimer := time.NewTimer(bound)
	defer boundTimer.Stop()
	select {
	case <-forwarding.notifier.serverRulesCleared:
		// A detach opens the gate before it cancels the connections: that is no verdict.
		return forwarding.ctx.Err() == nil
	case <-boundTimer.C:
		if refuseWhileSlotHeld(tcpConn, clientOpening, forwarding, logger) {
			return false
		}
		logger.Debug("refused a reverse request: the attach clear has not settled", "service", clientOpening.service)
		refuseOpening(tcpConn, msgReverseNotReady, "could not refuse a reverse request", logger)
		return false
	case <-ctx.Done():
		return false
	}
}

// refuseWhileSlotHeld answers a reverse request with msgReverseSlotHeld while the server's notifier
// slot is held elsewhere. A detached attachment gets no reply: its connections are being canceled.
// answered means the client has its FAIL and the connection is done.
func refuseWhileSlotHeld(tcpConn net.Conn, clientOpening opening, forwarding *attachmentFeatures,
	logger *slog.Logger) (answered bool) {
	if forwarding.ctx.Err() != nil || !forwarding.notifier.notifierSlotHeld() {
		return false
	}
	logger.Debug("refused a reverse request: the notifier slot is held elsewhere", "service", clientOpening.service)
	refuseOpening(tcpConn, msgReverseSlotHeld, "could not refuse a reverse request", logger)
	return true
}

// refuseOverBudget turns a local adb client away when every device-stream slot is taken; adb
// has no such limit. A length-prefixed adb request gets a FAIL naming the limit, as the
// status word the client waits for, so adb prints the reason. Bytes that form no request get
// the zero-byte close: no reply shape is known for them. The Warn is throttled; a poller at a
// full budget would flood the log.
func refuseOverBudget(tcpConn net.Conn, clientOpening opening, forwarding *attachmentFeatures, logger *slog.Logger) {
	if forwarding.refusals.allow(time.Now()) {
		logger.Warn("refused a local adb connection: every device-stream slot is taken",
			"maxDeviceStreams", forwarding.budget.limit)
	}
	if clientOpening.service == "" {
		return
	}
	reason := fmt.Sprintf("too many adb connections (limit %d)", forwarding.budget.limit)
	refuseOpening(tcpConn, reason, "could not refuse a local adb connection", logger)
}

// refuseOpening writes FAIL and reason to a client still at its opening. No pump watches the
// socket yet, so a write deadline stops a client that asked, then stopped reading. failMessage
// is the Debug message for a failed write.
func refuseOpening(tcpConn net.Conn, reason, failMessage string, logger *slog.Logger) {
	_ = tcpConn.SetWriteDeadline(time.Now().Add(openingRequestTimeout))
	if writeErr := adbwire.WriteFail(tcpConn, reason); writeErr != nil {
		logger.Debug(failMessage, "error", writeErr)
	}
}

// screenReverseRule counts a `reverse:*` request against the attachment's reverse rules before
// anything is dialed; adb has no such limit. A new device spec (the device-side `tcp:<port>`)
// at the limit gets the FAIL `too many reverse rules (limit <n>)`, the limit as `<n>`, so adb
// prints the reason, and refused ends the connection with no dial. Otherwise request awaits
// the server's verdict; nil when nothing is counted. The Warn is throttled like the budget's.
func screenReverseRule(tcpConn net.Conn, clientOpening opening, forwarding *attachmentFeatures,
	logger *slog.Logger) (request *reverseRequest, refused bool) {
	if forwarding == nil {
		return nil, false
	}
	action, deviceSpec, customerPort, counted := parseReverseRequest(clientOpening.unwrappedService())
	if !counted {
		return nil, false
	}
	rules := forwarding.reverseRules
	if request, full := rules.begin(action, deviceSpec, customerPort); !full {
		return request, false
	}
	if rules.refusals.allow(time.Now()) {
		logger.Warn("refused a reverse rule: the attachment holds its limit", "maxReverseRules", rules.limit)
	}
	reason := fmt.Sprintf("too many reverse rules (limit %d)", rules.limit)
	refuseOpening(tcpConn, reason, "could not refuse a reverse rule", logger)
	return nil, true
}

// relayRequest hands a freshly wrapped session every byte the client has said that the
// device must see, so the pipe starts with nothing stranded.
//
// selected says the daemon already answered the client's select. The session then needs
// its own switch, and it must be the daemon's canonical one: the client's select was
// consumed at the seam, and a second status word reaching the client would leave it one
// reply ahead for the rest of the connection.
func relayRequest(ctx context.Context, tcpConn net.Conn, session *adbConn, pending []byte,
	selected bool) error {
	// The watcher below is a goroutine, so a cancel that landed before it ran would not stop
	// the first send.
	if ctx.Err() != nil {
		return cancelOrFault(ctx, ctx.Err())
	}
	// Nothing pumps either leg yet, so the relay needs the close-on-cancel the pipe would
	// otherwise give it: a `device remove` must both unblock the wait for the device's
	// reply and stop the client's request from reaching a device the daemon has released.
	release := closeOnCancel(ctx, session, tcpConn)
	defer release()
	return cancelOrFault(ctx, sendOpening(tcpConn, session, pending, selected))
}

// cancelOrFault returns nil after a cancel, whatever fault came with it: end-of-life, as
// the pipe reports it. The connection context carries no deadline, so a deadline is a
// fault and stands.
func cancelOrFault(ctx context.Context, fault error) error {
	if errors.Is(ctx.Err(), context.Canceled) {
		return nil
	}
	return fault
}

// sendOpening plays the daemon's own `host:transport-any` switch when the seam answered the
// client's select, then sends pending. With no switch it reads the control frame within one
// round trip: a parked websocket can die unseen.
// One return for cancelOrFault to judge.
func sendOpening(tcpConn net.Conn, session *adbConn, pending []byte, selected bool) error {
	// The device's reply is one round trip, which is what ReattachTimeout budgets.
	within := session.policy.ReattachTimeout
	if selected {
		leftover, selectErr := selectTransport(session, within)
		if selectErr != nil {
			// The client is waiting for the status word of its own request, so adb's
			// refusal is exactly what it can read: it prints the reason instead of
			// reporting a hangup.
			var refusal *statusFailure
			if errors.As(selectErr, &refusal) {
				_ = tcpConn.SetWriteDeadline(time.Now().Add(within))
				_ = adbwire.WriteFail(tcpConn, refusal.reason)
				_ = tcpConn.SetWriteDeadline(time.Time{})
			}
			return fmt.Errorf("switch the session's transport: %w", selectErr)
		}
		// Bytes coalesced with the status word belong to the client, so they go out
		// before the pipe's own first read.
		if writeErr := writeWithin(tcpConn, leftover, within); writeErr != nil {
			return fmt.Errorf("deliver the bytes behind the switch reply: %w", writeErr)
		}
	}

	// pending can span several reads, so it goes out at the pipe's own message cap.
	for len(pending) > 0 {
		end := min(len(pending), outboundChunkSize)
		if err := session.Send(pending[:end]); err != nil {
			return fmt.Errorf("send the client's request: %w", err)
		}
		pending = pending[end:]
	}
	if !selected {
		if err := session.awaitHandshake(within); err != nil {
			return fmt.Errorf("open the session: %w", err)
		}
	}
	return nil
}

// closeOnCancel closes every target when ctx ends: no context can unblock a blocked read
// or write. The returned release stops the watcher, which must not outlive its caller.
func closeOnCancel(ctx context.Context, targets ...io.Closer) (release func()) {
	released := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			for _, target := range targets {
				_ = target.Close()
			}
		case <-released:
		}
	}()
	return func() { close(released) }
}
