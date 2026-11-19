package bridge

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/aws/device-connect-devicefarm/internal/forward"
)

// DeviceInfo identifies the remote device the proxy is currently bridging to.
type DeviceInfo struct {
	ARN      string // session ARN that produced the device
	Endpoint string // resolved websocket endpoint to dial (path-less; dials name a Route)
	Label    string // human-readable label shown to the user
	Serial   string // adb serial parsed from the keep-alive probe reply; empty until one parses
}

// ActiveDevice is the mutex-guarded single-device state shared by the accept loop,
// the pipes, the control handler, and the keep-alive loop. Set reports whether the
// bind was accepted, which enforces the one-device restriction. Clearing the device
// also cancels every in-flight connection bound to it: a proxied connection carries
// no deadline, so nothing else would sever it.
//
// The zero value is not usable; construct with NewActiveDevice.
type ActiveDevice struct {
	mu           sync.Mutex
	now          func() time.Time
	present      bool
	info         DeviceInfo
	lastActivity time.Time

	// conns holds the cancel func of every connection currently bridging to the
	// bound device, keyed by a monotonic id so each can deregister itself on exit.
	// Clear cancels them all; they are only ever populated while a device is bound.
	conns      map[uint64]context.CancelFunc
	nextConnID uint64

	// featureConfig is how to build an attachment's features, set once at
	// construction; features is the state of the current attachment, rebuilt by
	// every Set and dropped by every Clear. Both nil when no dialer was given.
	featureConfig *featureConfig
	features      *attachmentFeatures
}

// AttachmentParams carries the limits and timings one attachment runs under.
// Primitives only: this package never reads config.ProxyConfig.
type AttachmentParams struct {
	DeviceStreamStatusTimeout time.Duration
	MaxDeviceStreams          int
	MaxForwardRules           int
	MaxReverseRules           int
	NotifierKeepAlive         time.Duration
	NotifierBackoffInitial    time.Duration
	NotifierBackoffMax        time.Duration

	// ReverseEnabled off: no notifier for the attachment, and the accept seam refuses the
	// reverse family. ListenPort is the daemon's own adb port, kept only for the reverse port screen.
	ReverseEnabled bool
	ListenPort     int
}

// featureConfig is what WithAttachmentFeatures stores: the ingredients of an
// attachment's features, not the features themselves. One dialer serves both, the
// forward table for its device streams and the notifier for its own socket.
type featureConfig struct {
	dialer *WSDialer
	policy WSRefreshPolicy
	params AttachmentParams
	logger *slog.Logger
}

// attachmentFeatures is one attachment's live features. ctx ends when the attachment
// ends, so every stream the table starts and the notifier itself are severed by the
// same detach that closes the listeners.
type attachmentFeatures struct {
	table    *forward.ForwardRulesTable
	notifier *notifierSupervisor
	budget   *streamBudget   // the attachment's device-stream slots; see streamBudget
	refusals refusalThrottle // for local connections the budget turns away
	// reverseRules mirrors the server's reverse rules; see reverseRules
	reverseRules *reverseRules
	// reverseEnabled off: the seam refuses the reverse family; see refuseReverseFamily
	reverseEnabled bool
	ctx            context.Context
	cancel         context.CancelFunc
}

// close ends the attachment. The context dies before the listeners, so a connection
// accepted in the last instant meets a dead context; neither order shuts that window,
// so an opener must treat a dead attachment as a refusal. The notifier closes its
// socket on its own goroutine, so a detach never waits on a write. Nil-safe.
func (features *attachmentFeatures) close() {
	if features == nil {
		return
	}
	features.cancel()
	features.table.Close()
}

// ActiveDeviceOption configures an ActiveDevice at construction.
type ActiveDeviceOption func(*ActiveDevice)

// WithClock overrides the activity clock, so a test can drive idle time
// deterministically. A nil clock is ignored.
func WithClock(now func() time.Time) ActiveDeviceOption {
	return func(a *ActiveDevice) {
		if now != nil {
			a.now = now
		}
	}
}

// WithAttachmentFeatures turns on the features an attachment carries beside the adb
// stream: a fresh forward table per device whose accepted connections become device
// streams of their own, and, with reverse on, a reverse notifier held open for the
// whole attachment. Both dial with dialer, and every detach closes the table's listeners,
// severs its streams and closes the notifier. A caller with no dialer to give leaves both
// off rather than dead.
func WithAttachmentFeatures(dialer *WSDialer, policy WSRefreshPolicy,
	params AttachmentParams, logger *slog.Logger) ActiveDeviceOption {
	if dialer == nil {
		return func(*ActiveDevice) {}
	}
	if logger == nil {
		logger = slog.Default()
	}
	return func(device *ActiveDevice) {
		device.featureConfig = &featureConfig{dialer: dialer, policy: policy, params: params, logger: logger}
	}
}

// NewActiveDevice builds an empty ActiveDevice whose activity clock is time.Now,
// overridable with WithClock.
func NewActiveDevice(opts ...ActiveDeviceOption) *ActiveDevice {
	a := &ActiveDevice{now: time.Now, conns: make(map[uint64]context.CancelFunc)}
	for _, opt := range opts {
		opt(a)
	}
	return a
}

// Set binds info as the active device and starts its activity clock, returning
// false without changing anything if a device is already bound.
func (a *ActiveDevice) Set(info DeviceInfo) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.present {
		return false
	}
	a.present = true
	a.info = info
	a.lastActivity = a.now()
	a.buildFeaturesLocked()
	return true
}

// buildFeaturesLocked gives the attachment being bound its own features, context, stream budget
// and reverse rule table. The stream opener is per attachment, so it can hold the ARN its
// streams belong to.
// Caller holds device.mu and has already assigned device.info, so this must not bind, dial, wait,
// or call any ActiveDevice method: the opener and forward.NewForwardRulesTable only store their
// arguments, the table binds nothing until a rule is installed, and the notifier's own
// goroutine does the dialing, parked on device.mu inside EndpointFor until Set returns.
// Overwriting device.features is safe because Set refuses while a device is present and only
// clearLocked drops it.
func (device *ActiveDevice) buildFeaturesLocked() {
	config := device.featureConfig
	if config == nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	budget := &streamBudget{limit: config.params.MaxDeviceStreams}
	features := &attachmentFeatures{ctx: ctx, cancel: cancel, budget: budget,
		reverseRules:   newReverseRules(config.params.MaxReverseRules),
		reverseEnabled: config.params.ReverseEnabled}
	opener := &deviceStreamOpener{device: device, cfg: config, arn: device.info.ARN, ctx: ctx, budget: budget}
	features.table = forward.NewForwardRulesTable(opener, config.params.MaxForwardRules, config.logger)
	features.notifier = newNotifierSupervisor(device, config, device.info.ARN, features.table, features.reverseRules, budget)
	go features.notifier.run(features.ctx)
	device.features = features
}

// ClearReverseRulesAtServer asks the server to drop the attachment's reverse rules, so adbd's
// listeners and the tunnel legs end now, not at their own timeouts. Call it before Clear: the
// server refuses a reverse request with no notifier registered. Best effort, bounded by
// reverseClearTimeout; on failure the server keeps the rules. The dial is the daemon's own, like
// the keep-alive probe: no budget slot, no rule counted.
func (device *ActiveDevice) ClearReverseRulesAtServer(ctx context.Context) {
	device.mu.Lock()
	info, features, present := device.info, device.features, device.present
	device.mu.Unlock()
	if !present || features == nil || !features.notifier.notifierReady() || !features.reverseRules.holdsAny() {
		return
	}
	logger := device.featureConfig.logger.With("device", info.Label)
	if err := runReverseClear(ctx, device.featureConfig.dialer, info.Endpoint); err != nil {
		logger.Debug("could not clear the server's reverse rules before the detach", "error", err)
		return
	}
	logger.Debug("cleared the server's reverse rules before the detach")
}

// Clear unbinds the active device, cancels every in-flight connection bound to
// it, and returns the device that was bound (or a zero value and false if none
// was).
func (a *ActiveDevice) Clear() (DeviceInfo, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.clearLocked()
}

// clearLocked is the shared core of Clear (unconditional) and ClearMatching
// (ARN-guarded). Caller holds a.mu.
func (a *ActiveDevice) clearLocked() (DeviceInfo, bool) {
	if !a.present {
		return DeviceInfo{}, false
	}
	info := a.info
	a.present = false
	a.info = DeviceInfo{}
	features := a.features
	a.features = nil
	features.close()
	for _, cancel := range a.conns {
		cancel()
	}
	clear(a.conns)
	return info, true
}

// UpdateEndpoint refreshes the bound device's endpoint and label in place, so later
// dials (including a live connection's next websocket swap) pick up a renewed
// endpoint. Updates only when the bound ARN matches arn, so a racing remove or
// re-add is untouched; reports whether it did. Leaves the idle clock alone: a
// refresh is not proxied traffic and must not defer the keep-alive probe.
func (a *ActiveDevice) UpdateEndpoint(arn, endpoint, label string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.present || a.info.ARN != arn {
		return false
	}
	a.info.Endpoint = endpoint
	a.info.Label = label
	return true
}

// SetSerial records the bound device's adb serial, parsed from a keep-alive probe
// reply, and reports whether it recorded. ARN-guarded like UpdateEndpoint, so a probe
// raced by a remove or re-add cannot tag the wrong device. The idle clock is untouched.
func (device *ActiveDevice) SetSerial(arn, serial string) bool {
	device.mu.Lock()
	defer device.mu.Unlock()
	if !device.present || device.info.ARN != arn {
		return false
	}
	device.info.Serial = serial
	return true
}

// EndpointFor returns the bound device's current endpoint when one is bound and
// its ARN matches arn. The guard stops a refresh from dialing a different device
// that was attached in the meantime.
func (a *ActiveDevice) EndpointFor(arn string) (string, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.present || a.info.ARN != arn {
		return "", false
	}
	return a.info.Endpoint, true
}

// ClearMatching is Clear guarded by ARN: the refresh loop uses it to detach a device
// whose session it can no longer resolve, without dropping a different device
// attached meanwhile. A same-ARN remove-then-re-add in that window (ABA) is not
// protected; accepted for a single-user tool (see refreshEndpointOnce).
func (a *ActiveDevice) ClearMatching(arn string) (DeviceInfo, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.present || a.info.ARN != arn {
		return DeviceInfo{}, false
	}
	return a.clearLocked()
}

// Current returns the bound device and true, or a zero value and false when none
// is bound.
func (a *ActiveDevice) Current() (DeviceInfo, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.info, a.present
}

// trackConn reads the bound device and its features, and registers cancel as a
// new connection's teardown, all under one lock, so a Clear between a separate
// presence check and registration cannot leave a connection streaming from a
// detached device. features is nil when the attachment has none. ok is false when no
// device is bound. untrack is safe to call after a Clear has already deregistered
// the connection.
func (device *ActiveDevice) trackConn(cancel context.CancelFunc) (info DeviceInfo, features *attachmentFeatures, untrack func(), ok bool) {
	device.mu.Lock()
	defer device.mu.Unlock()
	if !device.present {
		return DeviceInfo{}, nil, nil, false
	}
	id := device.nextConnID
	device.nextConnID++
	device.conns[id] = cancel
	return device.info, device.features, func() {
		device.mu.Lock()
		defer device.mu.Unlock()
		delete(device.conns, id)
	}, true
}

// MarkActivity resets the idle clock to now; a no-op when no device is bound.
func (a *ActiveDevice) MarkActivity() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.present {
		a.lastActivity = a.now()
	}
}

// IdleProbe reports the bound device, how long it has been idle and how many
// connections are bridging to it, for the keep-alive loop. present is false when
// no device is bound.
func (device *ActiveDevice) IdleProbe() (info DeviceInfo, idle time.Duration, tracked int, present bool) {
	device.mu.Lock()
	defer device.mu.Unlock()
	if !device.present {
		return DeviceInfo{}, 0, 0, false
	}
	return device.info, device.now().Sub(device.lastActivity), len(device.conns), true
}
