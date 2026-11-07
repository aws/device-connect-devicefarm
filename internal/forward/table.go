// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package forward

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/aws/device-connect-devicefarm/internal/adbwire"
)

// StreamOpener carries an accepted local connection onto the transport as one
// forwarded stream. Implementations take ownership of conn and must close it.
// Each call runs on its own goroutine, so an implementation may block for the
// stream's whole lifetime; the accept loop is never held up.
type StreamOpener interface {
	OpenStream(conn net.Conn, target adbwire.AdbSpec)
}

// rule is one installed forward. The listener and its accept goroutine live as
// long as the rule; remote is re-pointed in place by a rebind, so the accept
// loop re-reads it under the table mutex at every accept.
type rule struct {
	local    adbwire.AdbSpec
	remote   adbwire.AdbSpec
	listener net.Listener
}

// ForwardRulesTable owns the forward rules and their listening sockets, the
// daemon's own stand-in for the adb host server's listener list
// (adb_listeners.cpp). All methods are safe for concurrent use; the mutex may
// cover a bind but never a dial, and listeners close under it. A connection
// accepted before the close is still handed off, as in adb.
//
// The zero value is not usable; construct with NewForwardRulesTable.
type ForwardRulesTable struct {
	opener   StreamOpener
	maxRules int
	logger   *slog.Logger

	rulesMutex sync.Mutex
	rules      map[string]*rule // keyed by the local spec text, no aliases folded (resolved for tcp:0)
	order      []string         // the same keys in insertion order; written with rules, under rulesMutex
	closed     bool             // see Close
}

// NewForwardRulesTable builds an empty table that holds at most maxRules rules; a
// limit below one refuses every Install. A nil logger defaults to slog.Default.
func NewForwardRulesTable(opener StreamOpener, maxRules int, logger *slog.Logger) *ForwardRulesTable {
	if logger == nil {
		logger = slog.Default()
	}
	return &ForwardRulesTable{opener: opener, maxRules: maxRules, logger: logger, rules: make(map[string]*rule)}
}

// Install binds localText and inserts a rule to remoteText, with adb's
// install_listener semantics: an existing local re-points its destination in
// place (unless noRebind), keeping the socket and leaving already-accepted
// connections on the destination they copied; tcp:0 resolves a port and
// re-keys the rule on it. A new local at the rule limit is refused before
// anything binds; a rebind is not a new rule, and tcp:0 always is.
func (table *ForwardRulesTable) Install(localText, remoteText string, noRebind bool) AdbInstallResult {
	table.rulesMutex.Lock()
	defer table.rulesMutex.Unlock()

	// Closed outranks a bad spec, as a device miss does in adb.
	if table.closed {
		return AdbInstallResult{Status: AdbInstallCannotBind, FailReason: adbwire.MsgNoDevices}
	}

	local, port, failReason := parseTCPSpec(localText)
	if failReason != "" {
		return AdbInstallResult{Status: AdbInstallCannotBind, FailReason: adbwire.FailCannotBind(failReason)}
	}
	remote, _, failReason := parseTCPSpec(remoteText)
	if failReason != "" {
		return AdbInstallResult{Status: AdbInstallUnsupportedRemote, FailReason: failReason}
	}

	if existing, ok := table.rules[local.String()]; ok {
		if noRebind {
			return AdbInstallResult{Status: AdbInstallCannotRebind, FailReason: adbwire.MsgCannotRebind}
		}
		existing.remote = remote
		return AdbInstallResult{Status: AdbInstallOK}
	}

	if len(table.rules) >= table.maxRules {
		return AdbInstallResult{Status: AdbInstallTooManyRules,
			FailReason: fmt.Sprintf("too many forward rules (limit %d)", table.maxRules)}
	}

	// Loopback only, like adb host listeners without -a.
	listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return AdbInstallResult{Status: AdbInstallCannotBind, FailReason: adbwire.FailCannotBind(bindCause(err))}
	}

	var resolvedPort int
	if port == 0 {
		resolvedPort = listener.Addr().(*net.TCPAddr).Port
		local = adbwire.AdbSpec{Kind: "tcp", Value: strconv.Itoa(resolvedPort)}
	}

	key := local.String()
	installed := &rule{local: local, remote: remote, listener: listener}
	table.rules[key] = installed
	table.order = append(table.order, key)
	go table.acceptLoop(installed)
	return AdbInstallResult{Status: AdbInstallOK, ResolvedPort: resolvedPort}
}

// Remove closes the rule's listening socket and drops the rule, reporting
// whether it existed (adb's killforward). The close is under the mutex so the
// port is free the moment the rule is gone: killforward then forward on the
// same port binds. A connection accepted before the close is still served, as
// it holds a copy of the destination.
func (table *ForwardRulesTable) Remove(localText string) bool {
	key := canonicalKey(localText)
	table.rulesMutex.Lock()
	defer table.rulesMutex.Unlock()
	removed, ok := table.rules[key]
	if !ok {
		return false
	}
	removed.listener.Close()
	delete(table.rules, key)
	orderIndex := slices.Index(table.order, key)
	table.order = slices.Delete(table.order, orderIndex, orderIndex+1)
	return true
}

// RemoveAll closes every listener and empties the table (adb's killforward-all).
// The table stays usable. Accepted connections are untouched: they hold copies.
func (table *ForwardRulesTable) RemoveAll() {
	table.rulesMutex.Lock()
	defer table.rulesMutex.Unlock()
	table.removeAllLocked()
}

// Close is RemoveAll for the detach; every later Install fails as adb does with
// no device. Install takes the same mutex, so no listener outlives Close. Idempotent.
func (table *ForwardRulesTable) Close() {
	table.rulesMutex.Lock()
	defer table.rulesMutex.Unlock()
	table.closed = true
	table.removeAllLocked()
}

// removeAllLocked is RemoveAll's body; the caller holds rulesMutex.
func (table *ForwardRulesTable) removeAllLocked() {
	for _, forwardRule := range table.rules {
		forwardRule.listener.Close()
	}
	clear(table.rules)
	table.order = nil
}

// List renders every rule in insertion order, as adb's format_listeners walks
// its listener list; a rebind keeps its position. Serial is left empty: the
// table records no device, the responder renders the attached one.
func (table *ForwardRulesTable) List() []adbwire.AdbListedForward {
	table.rulesMutex.Lock()
	defer table.rulesMutex.Unlock()
	entries := make([]adbwire.AdbListedForward, 0, len(table.order))
	for _, key := range table.order {
		forwardRule := table.rules[key]
		entries = append(entries, adbwire.AdbListedForward{Local: forwardRule.local.String(), Remote: forwardRule.remote.String()})
	}
	return entries
}

// BindsPort reports whether a rule listens on port. Every listener is asked for the
// port it actually bound, not matched by key: a key is its spec text, and "tcp:05000"
// and "tcp:0x1388" both listen on 5000 while neither renders as "tcp:5000".
func (table *ForwardRulesTable) BindsPort(port int) bool {
	table.rulesMutex.Lock()
	defer table.rulesMutex.Unlock()
	for _, forwardRule := range table.rules {
		if boundAddress, ok := forwardRule.listener.Addr().(*net.TCPAddr); ok && boundAddress.Port == port {
			return true
		}
	}
	return false
}

// bindCause renders a listen error as adb does: the errno's strerror text
// ("Address already in use"), which Go spells in lowercase. Anything without
// an errno keeps Go's text.
func bindCause(err error) string {
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return err.Error()
	}
	text := errno.Error()
	return strings.ToUpper(text[:1]) + text[1:]
}

// A non-close accept failure (fd exhaustion is the realistic one) backs off
// and retries. The door stays open and the backlog keeps queuing until accept
// works again; unlike net/http, which gives up on an error that is not
// temporary, the loop retries every non-close error so a rule never stops
// serving on its own.
const (
	// acceptRetryFirstDelay is net/http Server.Serve's first wait (src/net/http/server.go).
	acceptRetryFirstDelay = 5 * time.Millisecond
	// acceptRetryBackoffFactor is net/http Server.Serve's per-failure wait multiplier.
	acceptRetryBackoffFactor = 2
	// acceptRetryMaxDelay is net/http Server.Serve's wait cap.
	acceptRetryMaxDelay = time.Second
)

// acceptLoop serves one rule's listener until it closes. Every accepted
// connection copies the rule's current destination under the table mutex (a
// rebind re-points future accepts, never past ones) and is handed off on its
// own goroutine so a slow open never stalls the door.
func (table *ForwardRulesTable) acceptLoop(forwardRule *rule) {
	var delay time.Duration
	for {
		conn, err := forwardRule.listener.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			delay = min(max(acceptRetryBackoffFactor*delay, acceptRetryFirstDelay), acceptRetryMaxDelay)
			table.logger.Warn("forward listener accept failed, retrying", "local", forwardRule.local.String(), "error", err, "delay", delay)
			time.Sleep(delay)
			continue
		}
		delay = 0
		table.rulesMutex.Lock()
		target := forwardRule.remote
		table.rulesMutex.Unlock()
		go table.opener.OpenStream(conn, target)
	}
}

// parseTCPSpec validates one end of a forward rule: it must parse and be tcp.
// failReason is empty on success and otherwise carries the adbwire text; every
// non-tcp kind gets adb's unknown-kind text, including the unix-socket kinds
// adb itself serves on Linux, because this daemon serves only tcp.
func parseTCPSpec(text string) (spec adbwire.AdbSpec, port int, failReason string) {
	spec, err := adbwire.ParseSpec(text)
	if err != nil {
		return adbwire.AdbSpec{}, 0, err.Error()
	}
	if spec.Kind != "tcp" {
		return adbwire.AdbSpec{}, 0, adbwire.FailUnknownSpec(text)
	}
	port, err = spec.TCPPort()
	if err != nil {
		return adbwire.AdbSpec{}, 0, err.Error()
	}
	return spec, port, ""
}

// canonicalKey renders a raw local spec text the way Install renders the spec
// it keys on, so the two cannot drift if AdbSpec.String ever normalizes. Today
// the two renderings agree byte for byte. Text that does not parse keeps its
// raw form, so it can only miss.
func canonicalKey(text string) string {
	spec, err := adbwire.ParseSpec(text)
	if err != nil {
		return text
	}
	return spec.String()
}
