// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package bridge

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/aws/device-connect-devicefarm/internal/adbwire"
	"github.com/aws/device-connect-devicefarm/internal/forward"
)

// DeviceInfo identifies the remote device the proxy is currently bridging to.
type DeviceInfo struct {
	ARN      string // session ARN that produced the device
	Endpoint string // resolved websocket endpoint to dial (path-less; dials name a Route)
	Label    string // human-readable label shown to the user
	Serial   string // adb serial, fetched by the control add before the bind
	// AttachedAt is when Set bound the device; Set stamps it.
	AttachedAt time.Time
	// SessionStartedAt and SessionExpiresAt come from the session resolution at the add. Zero
	// when the service reported no start time. A refresh leaves them alone.
	SessionStartedAt time.Time
	SessionExpiresAt time.Time
}

// LastDetach is why and when the daemon last detached a device on its own: a session the
// refresh could not renew or a refused connection found gone. Cleared by the next Set. Clear
// records none.
type LastDetach struct {
	Reason string
	At     time.Time
}

// ActiveDevice is the mutex-guarded single-device state shared by the accept loop,
// the pipes, the control handler, the endpoint refresh loop and the attachment's own
// goroutines. Set reports whether the
// bind was accepted, which enforces the one-device restriction. Clearing the device
// also cancels every in-flight connection bound to it: a proxied connection carries
// no deadline, so nothing else would sever it.
//
// The zero value is not usable; construct with NewActiveDevice.
type ActiveDevice struct {
	mu         sync.Mutex
	present    bool
	info       DeviceInfo
	lastDetach LastDetach

	// lastServerVersion is the copy of its adb server's host:version reply that the last
	// attachment kept, taken at the detach; nil until an attachment has kept one. See
	// writeVersionWithNoDevice.
	lastServerVersion []byte

	// changed is closed and replaced under mu by every Set and every clear; a holder from
	// snapshot wakes on the next change. Holders wait outside mu.
	changed chan struct{}

	// refusalHandler is asked about the session after a refused fresh adb connection; see
	// WithRefusalHandler. Set once at construction. Nil: no lookup.
	refusalHandler RefusalHandler

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
	// skipHostPrefetch turns the attach's host reply prefetch off. Tests only.
	skipHostPrefetch bool
}

// attachmentFeatures is one attachment's live features. ctx ends with the attachment, so
// the detach that closes the listeners also severs the table's streams, the notifier, the
// warm socket and the host reply fetches.
type attachmentFeatures struct {
	table    *forward.ForwardRulesTable
	notifier *notifierSupervisor
	warm     *warmSocketKeeper // one parked adb websocket for the next dial; see warmSocketKeeper
	budget   *streamBudget     // the attachment's device-stream slots; see streamBudget
	refusals refusalThrottle   // for local connections the budget turns away
	// reverseRules mirrors the server's reverse rules; see reverseRules
	reverseRules *reverseRules
	// reverseEnabled off: the seam refuses the reverse family; see refuseReverseFamily
	reverseEnabled bool
	hostReplies    *hostReplyCache // the server's host replies the seam answers from; see answerHostQuery
	sessionLookups *refusalGate    // the daemon's session lookups after refused connections; see refusalGate
	ctx            context.Context
	cancel         context.CancelFunc
}

// refusalGate returns the attachment's gate, or nil without features. consult is nil-safe.
func (features *attachmentFeatures) refusalGate() *refusalGate {
	if features == nil {
		return nil
	}
	return features.sessionLookups
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

// warmSockets returns the attachment's keeper, or nil without features. adbSocket is
// nil-safe.
func (features *attachmentFeatures) warmSockets() *warmSocketKeeper {
	if features == nil {
		return nil
	}
	return features.warm
}

// ActiveDeviceOption configures an ActiveDevice at construction.
type ActiveDeviceOption func(*ActiveDevice)

// WithAttachmentFeatures turns on the features beside the adb stream: a forward table per
// device whose accepted connections become device streams, one warm adb websocket parked
// for the next dial, a host reply cache prefetched at attach, and, with reverse on, a reverse
// notifier held for the whole attachment. All dial with dialer. A detach closes the table's
// listeners, severs its streams, cancels the fetches and closes the notifier and the warm
// socket. A nil dialer leaves them off rather than dead.
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

// WithRefusalHandler makes a refused fresh session or a failed dial ask handler about the
// session first: RUNNING renews the endpoint and the connection dials once more, dead detaches
// with the reason the client reads. Lookups run one at a time per attachment.
func WithRefusalHandler(handler RefusalHandler) ActiveDeviceOption {
	return func(device *ActiveDevice) { device.refusalHandler = handler }
}

// NewActiveDevice builds an empty ActiveDevice.
func NewActiveDevice(opts ...ActiveDeviceOption) *ActiveDevice {
	device := &ActiveDevice{conns: make(map[uint64]context.CancelFunc), changed: make(chan struct{})}
	for _, opt := range opts {
		opt(device)
	}
	return device
}

// Set binds info as the active device with AttachedAt stamped now, returning false without
// changing anything if a device is already bound.
func (a *ActiveDevice) Set(info DeviceInfo) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.present {
		return false
	}
	info.AttachedAt = time.Now()
	a.present = true
	a.info = info
	a.lastDetach = LastDetach{}
	a.buildFeaturesLocked()
	a.signalChangeLocked()
	return true
}

// writeVersionWithNoDevice answers host:version while no device is attached: the copy of its adb
// server's reply that the last attachment kept, so an adb client meets the version it met with
// the device. Until an attachment has kept a copy there is none to repeat (in practice: the
// daemon has never held a device), and the answer is adbwire.AdbServerVersion, the version the
// device hosts are expected to run.
func (device *ActiveDevice) writeVersionWithNoDevice(writer io.Writer) error {
	device.mu.Lock()
	kept := device.lastServerVersion
	device.mu.Unlock()
	if kept == nil {
		return adbwire.WriteServerVersion(writer)
	}
	_, err := writer.Write(kept)
	return err
}

// signalChangeLocked wakes every holder of the current change signal and arms the next one.
// Caller holds mu.
func (device *ActiveDevice) signalChangeLocked() {
	close(device.changed)
	device.changed = make(chan struct{})
}

// snapshot reads the features (nil with no device or no dialer), the presence and the signal the
// next Set or clear closes, in one lock hold, so no change slips between the read and the wait.
// Callers wait on the signal outside the lock.
func (device *ActiveDevice) snapshot() (features *attachmentFeatures, present bool, changed <-chan struct{}) {
	device.mu.Lock()
	defer device.mu.Unlock()
	return device.features, device.present, device.changed
}

// buildFeaturesLocked gives the attachment being bound its own features, context, stream budget
// and reverse rule table. The stream opener is per attachment, so it can hold the ARN its
// streams belong to.
// Caller holds device.mu and has filled device.info, so this must not bind, dial, wait or call
// any ActiveDevice method: the opener and forward.NewForwardRulesTable only store their
// arguments, the table binds nothing until a rule is installed, and the notifier, the warm
// socket keeper and the host reply fetches dial on their own goroutines, parked on device.mu
// inside EndpointFor until Set returns.
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
		reverseEnabled: config.params.ReverseEnabled,
		sessionLookups: &refusalGate{handler: device.refusalHandler, device: device}}
	fetcher := &hostReplyFetcher{device: device, dialer: config.dialer, arn: device.info.ARN}
	features.hostReplies = newHostReplyCache(fetcher.fetch)
	features.warm = newWarmSocketKeeper(device, config, device.info.ARN)
	opener := &deviceStreamOpener{device: device, cfg: config, arn: device.info.ARN, ctx: ctx, budget: budget,
		features: features}
	features.table = forward.NewForwardRulesTable(opener, config.params.MaxForwardRules, config.logger)
	features.notifier = newNotifierSupervisor(device, config, device.info.ARN, features.table, features.reverseRules, budget)
	go features.notifier.run(features.ctx)
	go features.warm.run(features.ctx)
	if !config.skipHostPrefetch {
		features.hostReplies.prefetch(features.ctx, config.logger)
	}
	device.features = features
}

// ClearReverseRulesAtServer asks the server to drop the attachment's reverse rules, so adbd's
// listeners and the tunnel legs end now, not at their own timeouts. Call it before Clear: the
// server refuses a reverse request with no notifier registered. Best effort, bounded by
// reverseClearTimeout; on failure the server keeps the rules. The dial is the daemon's own, like
// the add's serial fetch and the host reply fetches: no budget slot, no rule counted.
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
	if features != nil {
		// The version outlives the attachment; every other copy ends with it.
		if kept := features.hostReplies.kept(hostVersionRequest); kept != nil {
			a.lastServerVersion = kept
		}
	}
	features.close()
	for _, cancel := range a.conns {
		cancel()
	}
	clear(a.conns)
	a.signalChangeLocked()
	return info, true
}

// UpdateEndpoint refreshes the bound device's endpoint and label in place, so later
// dials (including a live connection's next websocket swap) pick up a renewed
// endpoint. Updates only when the bound ARN matches arn, so a racing remove or
// re-add is untouched; reports whether it did.
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

// ClearMatching is Clear guarded by ARN: the refresh loop and the refusal lookups use it to
// detach a device whose session they can no longer resolve, without dropping a different
// device attached meanwhile, and record reason as the LastDetach. A same-ARN
// remove-then-re-add in that window (ABA) is not protected; accepted for a single-user
// tool (see refreshEndpointOnce).
func (a *ActiveDevice) ClearMatching(arn, reason string) (DeviceInfo, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.present || a.info.ARN != arn {
		return DeviceInfo{}, false
	}
	a.lastDetach = LastDetach{Reason: reason, At: time.Now()}
	return a.clearLocked()
}

// LastDetach returns why and when the daemon last detached a device on its own, and false
// while a device is attached or none was detached that way since the last Set.
func (device *ActiveDevice) LastDetach() (LastDetach, bool) {
	device.mu.Lock()
	defer device.mu.Unlock()
	return device.lastDetach, device.lastDetach.Reason != ""
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
