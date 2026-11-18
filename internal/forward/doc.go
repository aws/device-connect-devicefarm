// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

// Package forward owns the daemon's adb forward rules: the spec-text-keyed table,
// the live listening sockets and their accept loops.
//
// Rule semantics mirror the adb host server's install_listener: a plain
// rebind re-points the destination in place and keeps the listening socket;
// norebind against an existing rule fails; tcp:0 resolves a port, re-keys the
// rule on it and reports it for the extra protocol string. The accept path
// copies the rule's destination by value before handing a connection off, so
// a later rebind or removal never touches an accepted connection: removal
// closes the door, not the people inside. Rules list in insertion order, as
// adb's format_listeners walks its listener list, and a rebind keeps its
// position. One limit adb lacks: a table holds at most maxRules rules, and a
// new local past it fails with the daemon's own text; a rebind never counts.
//
// Respond answers one parsed forward-family request with the host server's own
// reply bytes, applying the device selector where adb acquires a transport. The
// serial column list-forward needs comes from the caller.
//
// Nothing here dials the network. Accepted connections leave through the
// StreamOpener interface, for internal/bridge to implement; spec parsing and
// reply texts come from internal/adbwire. Only tcp specs are supported on both
// ends; every other kind fails closed at install with adb's unknown-kind text,
// including the unix-socket kinds adb itself serves on Linux (adb defers remote
// validation to connect time, but a rule this daemon can never serve is
// refused up front).
//
// Every name here is the daemon's own; an Adb-prefixed type holds adb's model
// in its values or fields (see the internal/adbwire package doc). Where a name
// copies the adb host server, its comment names the adb source.
package forward
