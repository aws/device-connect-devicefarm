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
// reply texts come from internal/adbwire. The local end is tcp only. The
// remote end is tcp or a device socket kind (localabstract, localreserved,
// localfilesystem, jdwp), sent as spelled: adbd refuses a bad name or pid per
// connection. Every other kind (dev:, vsock:, acceptfd:, a bare service) fails
// at install with adb's unknown-kind text; adb would fail it at connect time,
// but the daemon keeps rules to sockets it can serve. On a rule whose remote is
// not tcp, each accepted connection carries an OpenTurn, so the device gets the
// rule's device services in accept order.
//
// Every name here is the daemon's own; an Adb-prefixed type holds adb's model
// in its values or fields (see the internal/adbwire package doc). Where a name
// copies the adb host server, its comment names the adb source.
package forward
