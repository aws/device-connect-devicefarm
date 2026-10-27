package bridge

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/aws/device-connect-devicefarm/internal/adbwire"
)

// reverseClearTimeout bounds each clear of the server's reverse rules: the attach clear and the
// one a `device remove` waits on. Source: pick.
const reverseClearTimeout = 5 * time.Second

// reverseKillAllRequest is adb's `reverse:killforward-all` service, framed: the forward
// family's killforward-all, which adbd (the daemon on the device) answers. The error is
// dropped: the encoder fails only on an empty or oversized service.
var reverseKillAllRequest, _ = adbwire.EncodeRequest("reverse:killforward-all")

// reverseRules caps one attachment's reverse rules; adb has no such limit. The server owns the
// rules and judges every `reverse:*` request. This table mirrors them, so an install past the
// limit is refused before any dial. It also records each install's customer port, so an
// announced open reaches only a port the customer named (allowsCustomerPort).
//
// A rule is keyed by its device spec: the `tcp:<port>` adbd (the daemon on the device) listens
// on, `adb reverse`'s first argument, as the client spelled it. Installing a known spec again is
// a rebind, not a new rule. A spec holds its slot while the server holds the rule or an install
// of it awaits a verdict, so a removal cannot free a slot under an install that may still succeed.
type reverseRules struct {
	limit    int
	refusals refusalThrottle // for installs the limit turns away

	rulesMutex sync.Mutex
	rules      map[string]*ruleSlot // device spec -> its slot; absent means free
}

// ruleSlot is what one device spec holds against the limit, and the customer ports its installs
// name.
type ruleSlot struct {
	installed    bool  // the server said OKAY, or never answered an install it received; no removal since
	customerPort int   // of the confirmed install, the latest when a rebind moved it; 0 holds none
	pendingPorts []int // one per install of the spec awaiting the server's verdict
}

func newReverseRules(limit int) *reverseRules {
	return &reverseRules{limit: limit, rules: make(map[string]*ruleSlot)}
}

// holdsAny reports whether any spec holds a slot: a rule held as installed, or a pending install.
func (rules *reverseRules) holdsAny() bool {
	rules.rulesMutex.Lock()
	defer rules.rulesMutex.Unlock()
	return len(rules.rules) > 0
}

// allowsCustomerPort reports whether a confirmed or pending install names port. Pending counts:
// the server can announce an open once adbd listens, before the client reads OKAY. Port 0 names
// nothing; a non-tcp customer spec records it.
func (rules *reverseRules) allowsCustomerPort(port int) bool {
	if port <= 0 {
		return false
	}
	rules.rulesMutex.Lock()
	defer rules.rulesMutex.Unlock()
	for _, slot := range rules.rules {
		if slot.customerPort == port || slices.Contains(slot.pendingPorts, port) {
			return true
		}
	}
	return false
}

// killReverseRules sends `reverse:killforward-all` on one fresh selected session and reads the
// server's verdict. ctx closes the socket when it ends, which unblocks any read.
func killReverseRules(ctx context.Context, dialer *WSDialer, endpoint string) error {
	ws, _, leftover, err := dialer.openSelectedSession(ctx, endpoint, reverseClearTimeout)
	if err != nil {
		return err
	}
	defer ws.Close()
	defer closeOnCancel(ctx, ws)()
	if err := ws.Send(reverseKillAllRequest); err != nil {
		return fmt.Errorf("send killforward-all: %w", err)
	}
	if _, err := readStatusWord(ws, reverseClearTimeout, leftover); err != nil {
		return fmt.Errorf("killforward-all: %w", err)
	}
	return nil
}

// runReverseClear is killReverseRules on its own goroutine, waited on for at most
// reverseClearTimeout: x/net's Close waits behind a stalled Send for sendDeadline, which ctx
// cannot cut.
func runReverseClear(ctx context.Context, dialer *WSDialer, endpoint string) error {
	ctx, cancel := context.WithTimeout(ctx, reverseClearTimeout)
	defer cancel()
	cleared := make(chan error, 1)
	go func() { cleared <- killReverseRules(ctx, dialer, endpoint) }()
	select {
	case err := <-cleared:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// reverseRequest is one counted `reverse:*` request, from the accept seam to the server's first
// status word. pending is read and written under rules.rulesMutex only.
type reverseRequest struct {
	rules        *reverseRules
	action       adbwire.AdbForwardAction
	deviceSpec   string
	customerPort int  // an install's `tcp:<port>` customer spec; 0 for a kill or any other spec
	pending      bool // an install awaiting its verdict, so holding its slot
	relayed      bool // reached the server; set before the pipe starts, read after it ends
}

// parseReverseRequest picks out the requests the table counts: install, killforward,
// killforward-all. counted is false for the rest: list-forward changes no rule, a malformed
// request installs none. adb spells reverse as `forward:<device>;<customer>`, so the device spec
// is the parsed Local. customerPort is the Remote's port when it is `tcp:<port>`; any other
// customer spec installs no rule the server would announce, so it is counted and holds no port.
func parseReverseRequest(service string) (action adbwire.AdbForwardAction, deviceSpec string,
	customerPort int, counted bool) {
	command, isReverse := strings.CutPrefix(service, reverseServicePrefix)
	if !isReverse {
		return 0, "", 0, false
	}
	request, isForwardFamily, parseErr := adbwire.ParseForwardService(command)
	if !isForwardFamily || parseErr != nil || request.Action == adbwire.AdbActionListForward {
		return 0, "", 0, false
	}
	return request.Action, request.Local, tcpPortOf(request.Remote), true
}

// tcpPortOf is the port of a `tcp:<port>` spec text, 0 for any other text.
func tcpPortOf(specText string) int {
	spec, err := adbwire.ParseSpec(specText)
	if err != nil {
		return 0
	}
	port, err := spec.TCPPort()
	if err != nil {
		return 0
	}
	return port
}

// begin counts one request before it is relayed. An install takes or joins its spec's slot now,
// not on OKAY, so racing installs cannot pass the limit together, and names its customer port
// now, so an open announced before the verdict is admitted. full means a new spec found no slot,
// and the caller refuses it. A kill takes nothing.
func (rules *reverseRules) begin(action adbwire.AdbForwardAction, deviceSpec string,
	customerPort int) (request *reverseRequest, full bool) {
	request = &reverseRequest{rules: rules, action: action, deviceSpec: deviceSpec, customerPort: customerPort}
	if action != adbwire.AdbActionForward {
		return request, false
	}
	rules.rulesMutex.Lock()
	defer rules.rulesMutex.Unlock()
	slot := rules.rules[deviceSpec]
	if slot == nil {
		if len(rules.rules) >= rules.limit {
			return nil, true
		}
		slot = &ruleSlot{}
		rules.rules[deviceSpec] = slot
	}
	slot.pendingPorts = append(slot.pendingPorts, customerPort)
	request.pending = true
	return request, false
}

// settle applies the server's first status word. An install ends its hold; OKAY marks its rule
// installed and records its customer port, the latest OKAY winning on a rebind. A kill's OKAY
// uninstalls one spec or all. Anything else changes no rule. Verdicts from different connections
// can land out of order; the count and the ports then drift by those rules until
// `killforward-all`.
func (request *reverseRequest) settle(okay bool) {
	rules := request.rules
	rules.rulesMutex.Lock()
	defer rules.rulesMutex.Unlock()
	switch {
	case request.action == adbwire.AdbActionForward:
		if !request.pending {
			return
		}
		request.pending = false
		slot := rules.rules[request.deviceSpec]
		slot.pendingPorts = withoutOne(slot.pendingPorts, request.customerPort)
		if okay {
			slot.installed = true
			slot.customerPort = request.customerPort
		}
		rules.freeIfIdleLocked(request.deviceSpec)
	case !okay:
	case request.action == adbwire.AdbActionKillForward:
		rules.uninstallLocked(request.deviceSpec)
	case request.action == adbwire.AdbActionKillForwardAll:
		for deviceSpec := range rules.rules {
			rules.uninstallLocked(deviceSpec)
		}
	}
}

// markRelayed records that the request left for the server. Nil-safe.
func (request *reverseRequest) markRelayed() {
	if request != nil {
		request.relayed = true
	}
}

// release ends an install's hold when its connection ends with no verdict. An install that never
// reached the server frees its slot. One that did counts as installed until a removal says
// otherwise: the server finishes what it started even when the client leaves. A kill changes
// nothing. Nil-safe and idempotent, so a connection defers it on every exit.
func (request *reverseRequest) release() {
	if request != nil {
		request.settle(request.relayed && request.action == adbwire.AdbActionForward)
	}
}

// uninstallLocked records the server's OKAY to a removal. Caller holds rulesMutex.
func (rules *reverseRules) uninstallLocked(deviceSpec string) {
	if slot := rules.rules[deviceSpec]; slot != nil {
		slot.installed = false
		slot.customerPort = 0
		rules.freeIfIdleLocked(deviceSpec)
	}
}

// freeIfIdleLocked drops a slot with no rule and no install in flight. Caller holds rulesMutex.
func (rules *reverseRules) freeIfIdleLocked(deviceSpec string) {
	if slot := rules.rules[deviceSpec]; slot != nil && !slot.installed && len(slot.pendingPorts) == 0 {
		delete(rules.rules, deviceSpec)
	}
}

// withoutOne drops one occurrence of port from ports.
func withoutOne(ports []int, port int) []int {
	if index := slices.Index(ports, port); index >= 0 {
		return slices.Delete(ports, index, index+1)
	}
	return ports
}

// watching wraps session so the server's first status word settles the request on its way to
// the client. Every byte passes unchanged. Nil-safe: an uncounted connection reads the session
// itself. No extra read, no deadline: an install waits at the server on adbd and on a tunnel
// create, longer than one round trip, and the adb client waits as long as it takes.
func (request *reverseRequest) watching(session WSConn) WSConn {
	if request == nil {
		return session
	}
	return &verdictWatch{WSConn: session, request: request}
}

// verdictWatch is a WSConn whose first status word settles a reverseRequest. Single reader, like
// the pipe's inbound pump that owns it.
type verdictWatch struct {
	WSConn
	request *reverseRequest
	word    []byte // the reply's first bytes, until a whole status word is in hand
}

func (watch *verdictWatch) Receive() ([]byte, error) {
	message, err := watch.WSConn.Receive()
	if err != nil || len(watch.word) == statusWordLen {
		return message, err
	}
	watch.word = append(watch.word, message[:min(len(message), statusWordLen-len(watch.word))]...)
	if len(watch.word) == statusWordLen {
		watch.request.settle(string(watch.word) == statusOkay)
	}
	return message, nil
}
