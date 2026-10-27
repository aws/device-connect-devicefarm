package bridge

import (
	"context"
	"errors"
	"fmt"
	"hash/maphash"
	"sync/atomic"
	"time"

	"github.com/aws/device-connect-devicefarm/internal/forward"
)

// errNotifierUnanswered means the socket is open but the server stopped answering. Only a redial
// fixes that.
var errNotifierUnanswered = errors.New("the server stopped answering notifier pings")

const (
	// notifierBackoffJitter spreads redials so daemons do not all dial again at once.
	// Chosen, not derived.
	notifierBackoffJitter = 0.1

	// notifierBackoffFactor multiplies the redial delay after each short-lived notifier.
	// Chosen, not derived.
	notifierBackoffFactor = 2

	// notifierMissBudget is how many heartbeat pings in a row may go unanswered before the socket
	// is closed as dead. Counts missing pongs: a peer that has stopped reading still accepts writes
	// until its buffer fills. A failed write ends the notifier at once. Chosen, not derived.
	notifierMissBudget = 2
)

// notifierSupervisor keeps one notifier open for one attachment until ctx ends: dial, send
// heartbeat pings, close it once it stops answering, dial again. A notifier is the websocket on
// which the server announces reverse connections the device opened. It skips trackConn: it is not
// a proxied connection, and ctx already ends with the attachment, so a second cancel is redundant.
type notifierSupervisor struct {
	device *ActiveDevice
	cfg    *featureConfig
	arn    string

	// table is this attachment's forward rules, read to refuse an announced port one of them
	// already binds; nil checks none.
	table *forward.ForwardRulesTable
	// rules is this attachment's reverse rules, read to refuse an announced port no install
	// named; nil refuses every port.
	rules *reverseRules

	budget   *streamBudget // the attachment's; see streamBudget
	refusals refusalThrottle

	ready atomic.Bool
	// slotHeld is set by a hello refused with notifierSlotTakenReason and cleared by the next READY:
	// the server's notifier slot is held elsewhere, by another client or by this client's own dropped
	// notifier until the server times it out. The seam refuses the reverse family while set. run's
	// goroutine alone writes it.
	slotHeld atomic.Bool
	// serverRulesCleared is closed once the attach clear (clearServerRulesOnce) settles, whatever its
	// result, or when run exits without one: reverse off, NotifierKeepAlive or NotifierBackoffInitial
	// unset, the attachment ended. The accept seam holds reverse requests until it is closed. run's
	// goroutine alone closes it, through openGate. Close it nowhere else: clearServerRulesOnce reads
	// a closed channel as a clear already done and skips its own.
	serverRulesCleared chan struct{}
}

// newNotifierSupervisor builds one attachment's supervisor, its gate shut. Only run opens it, so
// start run on its own goroutine.
func newNotifierSupervisor(device *ActiveDevice, cfg *featureConfig, arn string, table *forward.ForwardRulesTable,
	rules *reverseRules, budget *streamBudget) *notifierSupervisor {
	return &notifierSupervisor{device: device, cfg: cfg, arn: arn, table: table, rules: rules, budget: budget,
		serverRulesCleared: make(chan struct{})}
}

// openGate releases every held reverse request and every later one. Idempotent on run's
// goroutine, its only caller.
func (supervisor *notifierSupervisor) openGate() {
	select {
	case <-supervisor.serverRulesCleared:
	default:
		close(supervisor.serverRulesCleared)
	}
}

// notifierReady reports whether a notifier is serving this attachment right now, its attach clear
// settled.
func (supervisor *notifierSupervisor) notifierReady() bool {
	return supervisor.ready.Load()
}

// notifierSlotHeld reports whether the server's notifier slot is held elsewhere; see slotHeld.
func (supervisor *notifierSupervisor) notifierSlotHeld() bool {
	return supervisor.slotHeld.Load()
}

// markSlotHeld sets slotHeld; a change logs one Warn, a repeat logs nothing (run logs retries at
// Debug).
func (supervisor *notifierSupervisor) markSlotHeld(held bool) {
	if supervisor.slotHeld.Swap(held) == held {
		return
	}
	if held {
		supervisor.cfg.logger.Warn("another notifier holds reverse on this device, another client's or this client's dropped one until the server times it out; adb reverse is refused here meanwhile",
			"device", supervisor.arn)
		return
	}
	supervisor.cfg.logger.Warn("this client's notifier holds reverse on the device", "device", supervisor.arn)
}

// run keeps one notifier open at a time until ctx ends. If a notifier served longer than one
// heartbeat interval, run dials a new one at once and resets the backoff: the endpoint worked, so
// the drop was a server restart. A notifier that ended sooner, or a dial that failed, waits, and
// the wait grows up to NotifierBackoffMax.
func (supervisor *notifierSupervisor) run(ctx context.Context) {
	// Every exit opens the gate: a supervisor that never clears must not hold reverse requests.
	defer supervisor.openGate()
	// Reverse off: no notifier. The seam refuses the reverse family; a request that slips past
	// meets a server that denies reverse with no notifier registered.
	if !supervisor.cfg.params.ReverseEnabled {
		supervisor.cfg.logger.Info("reverse connections are off: no notifier is opened", "device", supervisor.arn)
		return
	}

	// A non-positive heartbeat interval panics the ticker; a non-positive backoff spins the loop.
	if supervisor.cfg.params.NotifierKeepAlive <= 0 || supervisor.cfg.params.NotifierBackoffInitial <= 0 {
		supervisor.cfg.logger.Error("the reverse notifier stays closed: its heartbeat interval or initial backoff is unset", "device", supervisor.arn)
		return
	}

	delay := supervisor.cfg.params.NotifierBackoffInitial
	for ctx.Err() == nil {
		served, err := supervisor.serveOne(ctx)
		if ctx.Err() != nil {
			return
		}
		supervisor.cfg.logger.Debug("the reverse notifier ended", "device", supervisor.arn, "servedFor", served, "reason", err)
		if served > supervisor.cfg.params.NotifierKeepAlive {
			delay = supervisor.cfg.params.NotifierBackoffInitial
			continue
		}
		if !sleepFor(ctx, nil, jitteredDelay(delay)) {
			return
		}
		delay = nextNotifierDelay(delay, supervisor.cfg.params.NotifierBackoffMax)
	}
}

// serveOne dials one notifier and serves it until it ends, reporting how long it stayed ready and
// why. The read loop gets its own goroutine, so a Receive waiting for a frame cannot stall the
// heartbeat ticker. A stuck ping write still blocks this goroutine for the send deadline, and
// x/net queues Close behind it.
func (supervisor *notifierSupervisor) serveOne(ctx context.Context) (time.Duration, error) {
	// The ack is one round trip to the endpoint just dialed, so it takes the dial timeout.
	notifier, err := dialNotifier(ctx, supervisor.cfg.dialer, supervisor.device, supervisor.arn,
		supervisor.cfg.dialer.dialTimeout, supervisor.cfg.logger)
	if err != nil {
		if errors.Is(err, ErrNotifierSlotTaken) {
			supervisor.markSlotHeld(true)
		}
		return 0, err
	}
	defer notifier.Close()
	// READY: this attachment holds the slot.
	supervisor.markSlotHeld(false)

	// The attachment's context, not the notifier's: a connector must outlive the notifier that
	// announced it, ending only with the attachment.
	announce := func(token string, customerPort int) { supervisor.announce(ctx, token, customerPort) }

	// Capacity one: a pong between two ticks is still seen, and extra pongs cannot pile up.
	pongs := make(chan struct{}, 1)
	ended := make(chan error, 1)
	go func() {
		ended <- notifier.run(announce, func() {
			select {
			case pongs <- struct{}{}:
			default:
			}
		})
	}()

	// Before ready: a detach's clear keys on ready, and the heartbeat starts after this one
	// bounded exchange.
	supervisor.clearServerRulesOnce(ctx)
	supervisor.ready.Store(true)
	defer supervisor.ready.Store(false)
	servedFrom := time.Now()
	err = supervisor.heartbeat(ctx, notifier, pongs, ended)
	return time.Since(servedFrom), err
}

// clearServerRulesOnce sends one `reverse:killforward-all` after the attachment's first
// NOTIFIER_READY: rules an earlier daemon left at the server would show in `adb reverse --list`
// while the empty table refused their opens. After READY because the server takes the request
// only with a notifier registered. Not on a redial: that would kill the customer's rules. Best
// effort: on failure those rules stay, refused until the customer installs them again. Every
// return opens the gate, so held reverse requests follow the verdict.
func (supervisor *notifierSupervisor) clearServerRulesOnce(ctx context.Context) {
	select {
	case <-supervisor.serverRulesCleared: // redial: clear already settled
		return
	default:
	}
	defer supervisor.openGate()
	endpoint, ok := supervisor.device.EndpointFor(supervisor.arn)
	if !ok {
		return
	}
	err := runReverseClear(ctx, supervisor.cfg.dialer, endpoint)
	switch {
	case ctx.Err() != nil: // a detach mid-clear is no fault
	case err != nil:
		supervisor.cfg.logger.Debug("could not clear the server's reverse rules at attach",
			"device", supervisor.arn, "error", err)
	default:
		supervisor.cfg.logger.Debug("cleared the server's reverse rules at attach", "device", supervisor.arn)
	}
}

// heartbeat sends a ping every NotifierKeepAlive and returns errNotifierUnanswered once
// notifierMissBudget pings in a row go unanswered. Pings keep the connection open: the load
// balancer closes a websocket that carries no data for its idle timeout, and a full server pool
// evicts the longest-quiet websocket first. Pings also detect a connection that looks open but no
// longer answers.
func (supervisor *notifierSupervisor) heartbeat(ctx context.Context, notifier *notifierConn,
	pongs <-chan struct{}, ended <-chan error) error {
	ticker := time.NewTicker(supervisor.cfg.params.NotifierKeepAlive)
	defer ticker.Stop()

	unanswered := 0
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-ended:
			return err
		case <-ticker.C:
			select {
			case <-pongs:
				unanswered = 0
			default:
			}
			if unanswered >= notifierMissBudget {
				return errNotifierUnanswered
			}
			if err := notifier.ping(); err != nil {
				return fmt.Errorf("send a notifier heartbeat ping: %w", err)
			}
			unanswered++
		}
	}
}

// announce splices one reverse connection to the customer's port. It runs on its own goroutine per
// announcement (notifierConn.run) and blocks for that connection's life. The stream budget bounds live
// connectors; a refused announcement returns at once. The token is a bearer capability, never logged.
func (supervisor *notifierSupervisor) announce(ctx context.Context, token string, customerPort int) {
	// Ahead of both dials, so a refused port dials nothing.
	if reason := supervisor.refuseCustomerPort(customerPort); reason != "" {
		supervisor.cfg.logger.Warn("a reverse connection was refused", "device", supervisor.arn,
			"customerPort", customerPort, "reason", reason)
		return
	}
	// Budget next, also ahead of both dials. On a drop the server reaps the parked connection.
	if !supervisor.budget.admit() {
		if supervisor.refusals.allow(time.Now()) {
			supervisor.cfg.logger.Warn("refused a reverse connection: every device-stream slot is taken",
				"device", supervisor.arn, "customerPort", customerPort, "maxDeviceStreams", supervisor.budget.limit)
		}
		return
	}
	defer supervisor.budget.release()
	err := openConnector(ctx, supervisor.cfg, supervisor.device, supervisor.arn, token, customerPort)
	// A gone session, a detached device or a severed attachment is how a connector is meant to end,
	// so none of them is a fault. A cancelled attachment surfaces as whatever error the dial or the
	// splice was in the middle of, so the context is what says so, not the error.
	if err != nil && ctx.Err() == nil && !errors.Is(err, errAdbConnClosed) {
		supervisor.cfg.logger.Warn("a reverse connection failed", "device", supervisor.arn,
			"customerPort", customerPort, "error", err)
		return
	}
	supervisor.cfg.logger.Debug("a reverse connection ended", "device", supervisor.arn, "customerPort", customerPort)
}

// refuseCustomerPort reports why customerPort must not be dialed, or "" when it may be. announce is
// openConnector's only caller, so this screens every connector. The daemon's own port speaks the adb
// host protocol (host:kill, host:transport and everything behind them), and a forward rule's port
// lands the device's bytes in a forwarded stream instead of the customer's server. An unknown daemon
// port refuses every port rather than matching none. A port no reverse rule names is refused too:
// the screen distrusts the announce, so the port must come from an install the seam read, pending
// or confirmed; no table at all refuses every port. Every other port a rule names stays reachable.
func (supervisor *notifierSupervisor) refuseCustomerPort(customerPort int) string {
	switch {
	case supervisor.cfg.params.ListenPort <= 0:
		return "the daemon's own adb port is unknown"
	case customerPort == supervisor.cfg.params.ListenPort:
		return "it is the daemon's own adb port"
	case supervisor.table != nil && supervisor.table.BindsPort(customerPort):
		return "a forward rule listens on it"
	case supervisor.rules == nil || !supervisor.rules.allowsCustomerPort(customerPort):
		return "no reverse rule names it"
	}
	return ""
}

// nextNotifierDelay multiplies delay by notifierBackoffFactor, up to limit. A limit below delay
// leaves delay alone: shrinking it would spin the redial loop.
func nextNotifierDelay(delay, limit time.Duration) time.Duration {
	return max(min(notifierBackoffFactor*delay, limit), delay)
}

// jitteredDelay moves delay by a random amount of up to notifierBackoffJitter times delay, either
// way. maphash is random enough: spreading redials needs no secrecy.
func jitteredDelay(delay time.Duration) time.Duration {
	span := time.Duration(float64(delay) * notifierBackoffJitter)
	if span <= 0 {
		return delay
	}
	return delay + time.Duration(maphash.Bytes(maphash.MakeSeed(), nil)%uint64(2*span)) - span
}
