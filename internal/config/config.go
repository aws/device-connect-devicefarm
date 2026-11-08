// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"errors"
	"fmt"
	"math"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/aws/device-connect-devicefarm/internal/bridge"
	"github.com/aws/device-connect-devicefarm/internal/failure"
	"github.com/aws/device-connect-devicefarm/internal/wording"
)

// BinaryName names the executable. The wording package defines it; runtime file
// names read it through this alias. The env var prefix below is spelled out.
const BinaryName = wording.BinaryName

// Hidden test-only overrides; _MS values are integer milliseconds. Env vars, not
// flags: they stay out of --help and cross the daemon re-exec with no AsDaemonArgs
// plumbing.
const (
	wsRefreshIntervalEnvVar = "DEVICEFARM_CONNECT_REFRESH_MS"
	freeSecondWaitEnvVar    = "DEVICEFARM_CONNECT_FREE_SECOND_WAIT_MS"
	quietWindowEnvVar       = "DEVICEFARM_CONNECT_QUIESCE_MS"

	deviceStreamStatusEnvVar = "DEVICEFARM_CONNECT_DEVICE_STREAM_STATUS_MS"
	maxDeviceStreamsEnvVar   = "DEVICEFARM_CONNECT_MAX_DEVICE_STREAMS"
	maxForwardRulesEnvVar    = "DEVICEFARM_CONNECT_MAX_FORWARD_RULES"
	maxReverseRulesEnvVar    = "DEVICEFARM_CONNECT_MAX_REVERSE_RULES"
	notifierKeepAliveEnvVar  = "DEVICEFARM_CONNECT_NOTIFIER_KEEPALIVE_MS"
	notifierBackoffEnvVar    = "DEVICEFARM_CONNECT_NOTIFIER_BACKOFF_MS" // initial delay only; the cap has none
	parkPingEnvVar           = "DEVICEFARM_CONNECT_PARK_PING_MS"
)

// Default flag values. 5037 matches adb's own default server port, so a plain
// `adb` with no -P finds the daemon.
//
// The durations are not CLI flags: fixed runtime constants seeded by Default.
const (
	DefaultListenAddr  = "127.0.0.1"
	DefaultListenPort  = 5037
	DefaultStopTimeout = 10 * time.Second
	DefaultDialTimeout = 10 * time.Second

	// Latest a swap starts per proxied connection; the jitter below only pulls it
	// earlier. Interval + hunt + attach budget + the server's drain of the superseded
	// websocket fits websocketLifetimeCap; Validate enforces the sum. The swap is
	// lossless, so the adb client never sees it.
	DefaultWSRefreshInterval = 3*time.Minute + 15*time.Second

	// Bounds the hunt for a quiet moment to swap in. Past it the refresh proceeds
	// regardless: a connection under continuous load must still refresh.
	DefaultFreeSecondWait = 30 * time.Second

	// Idle gap that makes a swap safe to attempt, since the seam briefly fences
	// the outbound direction.
	DefaultQuietWindow = 1 * time.Second

	// How often the quiet window is re-checked during the hunt.
	DefaultQuiescePoll = 100 * time.Millisecond

	// Consecutive failed refreshes tolerated before the connection is torn down.
	DefaultMaxRefreshFailures = 3

	// Pulls each refresh earlier by up to this fraction, never later, so concurrent
	// connections do not swap together and overrun the server's per-host connection cap.
	DefaultRefreshJitter = 0.10

	// DefaultEndpointRefreshInterval is how often the daemon re-resolves the
	// attached session's interactive endpoint. Renews the URL before
	// endpointURLValidity lapses, with EndpointRenewMargin to spare.
	DefaultEndpointRefreshInterval = 4 * time.Minute

	// Bounds each of the two status-word reads in a forwarded connection's
	// prologue: one round trip to the endpoint, like DialTimeout.
	DefaultDeviceStreamStatusTimeout = 10 * time.Second

	// Device streams open at once per attachment: local adb connections, forwarded
	// connections and reverse connectors together. The /adb admission ceiling less
	// a reserve for the reverse notifier, the serial fetch, the host reply fetches,
	// the parked warm websocket and swap overlaps.
	DefaultMaxDeviceStreams = adbAdmissionCeiling - 8

	// Forward rules per attachment. A rule costs a listener, no websocket; its
	// connections spend MaxDeviceStreams, and this many at one connection each
	// fit inside it. Source: pick.
	DefaultMaxForwardRules = 8

	// Reverse rules per attachment. The server holds the rules; each costs a
	// listener at the server and one at adbd, no websocket. Mirrors
	// DefaultMaxForwardRules. Source: pick.
	DefaultMaxReverseRules = 8

	// Client-to-server PING cadence on the reverse notifier websocket. The load
	// balancer drops a websocket idle for loadBalancerIdleTimeout. The websocket
	// relay forwards no ping or pong. The remote access service evicts the
	// longest-idle connection first. Only frames on that connection, either
	// direction, reset its idle clock; an idle notifier sends none. The same
	// cadence finds a dead notifier: a run of unanswered PINGs closes it.
	DefaultNotifierKeepAlive = 5 * time.Second

	// Websocket ping cadence on the parked adb websocket. A pong missed by the
	// next ping closes it and parks a new one. The websocket relay answers the
	// ping itself, so a pong proves the path to the relay only.
	DefaultParkPingInterval = 5 * time.Second

	// Jittered redial backoff for a notifier that dropped while attached.
	DefaultNotifierBackoffInitial = 1 * time.Second
	DefaultNotifierBackoffMax     = 30 * time.Second

	// Reverse is off until --allow-adb-reverse sets it.
	DefaultReverseEnabled = false
)

const (
	// endpointURLValidity is how long a resolved interactive endpoint URL stays
	// dialable. A server-side property, mirrored so Validate can check the cadence.
	endpointURLValidity = 5 * time.Minute

	// EndpointRenewMargin is the slack Validate demands between an endpoint's
	// worst-case age and the URL going stale. It also bounds the daemon's refresh
	// lookup, retries included.
	EndpointRenewMargin = 30 * time.Second

	// websocketLifetimeCap is the longest any websocket may live, from its dial to
	// the server's close of it. A hard rule: intermediaries on the path cut a
	// connection at about this age. Validate fits a whole swap cycle inside it.
	websocketLifetimeCap = 5 * time.Minute

	// supersededDrainAllowance is how long the server keeps a superseded websocket
	// open after the takeover while its inbound drains under load. A server-side
	// property, mirrored so Validate can count it against websocketLifetimeCap.
	supersededDrainAllowance = 30 * time.Second

	// loadBalancerIdleTimeout is how long the load balancer in front of the
	// websocket relay lets a websocket sit with no frame in either direction. A
	// server-side property, mirrored so Validate can keep the notifier keepalive
	// under it.
	loadBalancerIdleTimeout = 390 * time.Second

	// adbAdmissionCeiling is the most /adb websockets one remote access service
	// process admits: its connection pool less the slots it reserves for its
	// other routes. Other routes past their reserve lower it, the notifier's own
	// route included. A ceiling, not a guarantee; mirrored to derive the stream
	// cap and to bound MaxDeviceStreams in Validate.
	adbAdmissionCeiling = 32
)

// ProxyConfig is the resolved runtime configuration for the bridge daemon:
// populated from CLI flags, passed by value into the data plane, rebuilt for the
// detached child via AsDaemonArgs. It holds no AWS material; the session ARN is
// a per-command argument.
type ProxyConfig struct {
	ListenAddr string
	ListenPort int

	// Daemon logfile the parent redirects the child's stdout/stderr into. A
	// parent-side concern, so not forwarded by AsDaemonArgs.
	LogPath string

	Debug bool

	// Fixed runtime constants (see Default), not CLI flags. Seeded identically in
	// parent and child, so they need not cross the process boundary.
	StopTimeout time.Duration
	DialTimeout time.Duration

	// Websocket-refresh cadence: swap every WSRefreshInterval, hunting up to
	// FreeSecondWait for a QuietWindow-long idle gap, re-checking every
	// QuiescePoll. The data-plane swap, not the AWS re-resolve below. See
	// bridge.WSRefreshPolicy.
	WSRefreshInterval time.Duration
	FreeSecondWait    time.Duration
	QuietWindow       time.Duration
	QuiescePoll       time.Duration

	// How often the daemon re-resolves the session ARN against AWS to renew its
	// interactive endpoint. The control-plane re-resolve, not the websocket swap
	// above; a swap dials whatever endpoint the latest re-resolve stored.
	EndpointRefreshInterval time.Duration

	// Forward and reverse knobs: the bound on each prologue status-word read and
	// the per-attachment caps on open device streams, forward rules and reverse
	// rules. Fixed runtime constants.
	DeviceStreamStatusTimeout time.Duration
	MaxDeviceStreams          int
	MaxForwardRules           int
	MaxReverseRules           int

	// Reverse notifier knobs: PING cadence and the redial backoff range, fixed
	// runtime constants. ReverseEnabled is the exception: --allow-adb-reverse sets it.
	NotifierKeepAlive      time.Duration
	NotifierBackoffInitial time.Duration
	NotifierBackoffMax     time.Duration
	ReverseEnabled         bool

	// Ping cadence on the parked adb websocket, a fixed runtime constant.
	ParkPingInterval time.Duration
}

// Default returns a ProxyConfig with the default values. The CLI binds cobra
// flags on top of these.
func Default() ProxyConfig {
	return ProxyConfig{
		ListenAddr:              DefaultListenAddr,
		ListenPort:              DefaultListenPort,
		StopTimeout:             DefaultStopTimeout,
		DialTimeout:             DefaultDialTimeout,
		WSRefreshInterval:       DefaultWSRefreshInterval,
		FreeSecondWait:          DefaultFreeSecondWait,
		QuietWindow:             DefaultQuietWindow,
		QuiescePoll:             DefaultQuiescePoll,
		EndpointRefreshInterval: DefaultEndpointRefreshInterval,

		DeviceStreamStatusTimeout: DefaultDeviceStreamStatusTimeout,
		MaxDeviceStreams:          DefaultMaxDeviceStreams,
		MaxForwardRules:           DefaultMaxForwardRules,
		MaxReverseRules:           DefaultMaxReverseRules,
		NotifierKeepAlive:         DefaultNotifierKeepAlive,
		NotifierBackoffInitial:    DefaultNotifierBackoffInitial,
		NotifierBackoffMax:        DefaultNotifierBackoffMax,
		ReverseEnabled:            DefaultReverseEnabled,
		ParkPingInterval:          DefaultParkPingInterval,
	}
}

// Validate gates the config before the daemon binds its listener. It checks the
// flag-settable fields and every duration whose non-positive value would silently
// break a loop: EndpointRefreshInterval feeds time.NewTicker (panics at zero),
// WSRefreshInterval at zero switches refreshing off, FreeSecondWait or QuietWindow
// at zero make every swap lossy. QuiescePoll needs no guard: normalized defaults
// it. DialTimeout at zero fails every dial at once. It bounds a websocket's life:
// WSRefreshInterval, FreeSecondWait, the attach budget and the server's drain must fit
// websocketLifetimeCap. It also gates the
// forward and reverse knobs: a non-positive timeout, keepalive or backoff wedges a
// read or spins a loop; a stream cap below one refuses every forwarded connection
// and connector; a stream cap at or past adbAdmissionCeiling plans for slots the
// remote access service does not have; a rule cap below one refuses every rule.
func (c ProxyConfig) Validate() error {
	if c.ListenAddr == "" {
		return errors.New(wording.ConfigListenAddrEmpty)
	}
	// A hostname has no colon or bracket. One in a value netip rejects means
	// host:port or a broken IP literal.
	if _, err := netip.ParseAddr(c.ListenAddr); err != nil && strings.ContainsAny(c.ListenAddr, ":[]") {
		return failure.WithFix(
			fmt.Errorf(wording.ConfigListenAddrMalformedFormat, c.ListenAddr),
			wording.ConfigSetPortWithFlag,
		)
	}
	if c.ListenPort < 1 || c.ListenPort > 65535 {
		return fmt.Errorf(wording.ConfigPortOutOfRangeFormat, c.ListenPort)
	}
	if c.EndpointRefreshInterval <= 0 {
		return fmt.Errorf("endpoint refresh interval must be positive, got %s", c.EndpointRefreshInterval)
	}
	if c.DialTimeout <= 0 {
		return fmt.Errorf("dial timeout must be positive, got %s", c.DialTimeout)
	}
	if c.WSRefreshInterval <= 0 {
		return fmt.Errorf("websocket refresh interval must be positive, got %s", c.WSRefreshInterval)
	}
	if c.FreeSecondWait <= 0 {
		return fmt.Errorf("free-second wait must be positive, got %s", c.FreeSecondWait)
	}
	if c.QuietWindow <= 0 {
		return fmt.Errorf("quiet window must be positive, got %s", c.QuietWindow)
	}
	// Subtract and divide, do not add or multiply: a sum or product can overflow and pass.
	swapBudget := websocketLifetimeCap - supersededDrainAllowance - c.FreeSecondWait
	const roundTrips = 2 * bridge.AttachAttempts
	if c.DialTimeout > swapBudget/roundTrips || c.WSRefreshInterval > swapBudget-roundTrips*c.DialTimeout {
		return fmt.Errorf("websocket refresh interval %s, free-second wait %s, %d attach attempts of two round trips at the dial timeout %s and the server's %s drain of a superseded websocket must fit the websocket lifetime cap %s",
			c.WSRefreshInterval, c.FreeSecondWait, bridge.AttachAttempts, c.DialTimeout, supersededDrainAllowance, websocketLifetimeCap)
	}
	// A swap can dial a URL one EndpointRefreshInterval old, so renewal must stay
	// ahead of expiry with margin. Subtract, do not add: interval+margin can
	// overflow and pass.
	if c.EndpointRefreshInterval > endpointURLValidity-EndpointRenewMargin {
		return fmt.Errorf("endpoint refresh interval %s leaves less than %s of margin before an endpoint URL expires (validity %s)",
			c.EndpointRefreshInterval, EndpointRenewMargin, endpointURLValidity)
	}
	if c.DeviceStreamStatusTimeout <= 0 {
		return fmt.Errorf("device stream status timeout must be positive, got %s", c.DeviceStreamStatusTimeout)
	}
	if c.MaxDeviceStreams < 1 {
		return fmt.Errorf("max device streams must be at least 1, got %d", c.MaxDeviceStreams)
	}
	// Strictly under: every leg holds two slots during a swap, so a cap at the
	// ceiling plans for a slot the service does not have.
	if c.MaxDeviceStreams >= adbAdmissionCeiling {
		return fmt.Errorf("max device streams %d must stay under the remote access service's admission ceiling %d", c.MaxDeviceStreams, adbAdmissionCeiling)
	}
	if c.MaxForwardRules < 1 {
		return fmt.Errorf("max forward rules must be at least 1, got %d", c.MaxForwardRules)
	}
	if c.MaxReverseRules < 1 {
		return fmt.Errorf("max reverse rules must be at least 1, got %d", c.MaxReverseRules)
	}
	if c.NotifierKeepAlive <= 0 {
		return fmt.Errorf("notifier keepalive must be positive, got %s", c.NotifierKeepAlive)
	}
	// A sanity bound, not a margin: an idle notifier sends nothing but PINGs, so a
	// cadence at or past the idle timeout lets the load balancer drop it between two.
	if c.NotifierKeepAlive >= loadBalancerIdleTimeout {
		return fmt.Errorf("notifier keepalive %s must stay under the load balancer idle timeout %s", c.NotifierKeepAlive, loadBalancerIdleTimeout)
	}
	if c.NotifierBackoffInitial <= 0 {
		return fmt.Errorf("notifier initial backoff must be positive, got %s", c.NotifierBackoffInitial)
	}
	if c.NotifierBackoffMax < c.NotifierBackoffInitial {
		return fmt.Errorf("notifier backoff max %s must not be below the initial %s", c.NotifierBackoffMax, c.NotifierBackoffInitial)
	}
	if c.ParkPingInterval <= 0 {
		return fmt.Errorf("park ping interval must be positive, got %s", c.ParkPingInterval)
	}
	return nil
}

// durationFromEnv reads name as positive integer milliseconds, falling back when
// unset, empty, non-positive, unparseable, or too large for a time.Duration.
func durationFromEnv(name string, fallback time.Duration) time.Duration {
	millis := countFromEnv(name, 0)
	// Scaled to nanoseconds below, anything past maxDurationMillis (~9.2e12 ms)
	// overflows time.Duration and wraps to a nonsense interval.
	if millis <= 0 || millis > maxDurationMillis {
		return fallback
	}
	return time.Duration(millis) * time.Millisecond
}

// countFromEnv reads name as a positive integer, falling back when unset, empty,
// non-positive, unparseable or out of range.
func countFromEnv(name string, fallback int) int {
	raw, ok := os.LookupEnv(name)
	if !ok {
		return fallback
	}
	count, err := strconv.Atoi(raw)
	if err != nil || count <= 0 {
		return fallback
	}
	return count
}

// maxDurationMillis is the largest millisecond count that still fits a
// time.Duration once scaled to nanoseconds.
const maxDurationMillis = int(math.MaxInt64 / int64(time.Millisecond))

// WithRefreshOverridesFromEnv returns a copy of c with the websocket-refresh
// cadence taken from the hidden env overrides. Each field falls back
// independently. Only the daemon body calls it.
func (c ProxyConfig) WithRefreshOverridesFromEnv() ProxyConfig {
	c.WSRefreshInterval = durationFromEnv(wsRefreshIntervalEnvVar, c.WSRefreshInterval)
	c.FreeSecondWait = durationFromEnv(freeSecondWaitEnvVar, c.FreeSecondWait)
	c.QuietWindow = durationFromEnv(quietWindowEnvVar, c.QuietWindow)
	return c
}

// WithForwardReverseOverridesFromEnv returns a copy of c with the forward,
// notifier and park ping knobs taken from the hidden env overrides. Each field
// falls back independently; the backoff override moves the initial delay only.
// Only the daemon body calls it.
func (c ProxyConfig) WithForwardReverseOverridesFromEnv() ProxyConfig {
	c.DeviceStreamStatusTimeout = durationFromEnv(deviceStreamStatusEnvVar, c.DeviceStreamStatusTimeout)
	c.MaxDeviceStreams = countFromEnv(maxDeviceStreamsEnvVar, c.MaxDeviceStreams)
	c.MaxForwardRules = countFromEnv(maxForwardRulesEnvVar, c.MaxForwardRules)
	c.MaxReverseRules = countFromEnv(maxReverseRulesEnvVar, c.MaxReverseRules)
	c.NotifierKeepAlive = durationFromEnv(notifierKeepAliveEnvVar, c.NotifierKeepAlive)
	c.NotifierBackoffInitial = durationFromEnv(notifierBackoffEnvVar, c.NotifierBackoffInitial)
	c.ParkPingInterval = durationFromEnv(parkPingEnvVar, c.ParkPingInterval)
	return c
}

// AsDaemonArgs builds the argv for the detached daemon child. Omits the
// durations (the child seeds them itself; test overrides arrive via the
// environment) and --log (the parent redirects the child's stdout/stderr).
func (c ProxyConfig) AsDaemonArgs() []string {
	args := []string{
		"--listen-addr", c.ListenAddr,
		"--port", strconv.Itoa(c.ListenPort),
	}
	if c.Debug {
		args = append(args, "--debug")
	}
	// The child runs the seam and opens the notifier, so it needs the opt-in.
	if c.ReverseEnabled {
		args = append(args, "--allow-adb-reverse")
	}
	return args
}
