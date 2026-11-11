// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

// Package control is the daemon's control channel: a per-user Unix-domain socket
// over which the foreground CLI mutates a running detached daemon (add, list or
// remove the connected device, query status, shut it down). Signals cannot carry an ARN
// or return a device label, so a small request/response protocol fills that gap.
//
// One connection carries exactly one request and one response, both single JSON
// objects; the CLI opens a fresh connection per command. The request carries only
// the session ARN: the daemon resolves it to a websocket endpoint itself (and
// refreshes it periodically), so the CLI does no AWS work and the wire protocol
// stays credential-free.
package control

import "time"

// Op is the control operation a request carries.
type Op string

const (
	// OpAdd connects a device: the daemon resolves the request ARN to a websocket
	// endpoint and binds it as the active device. The tool holds one device at a
	// time (a temporary restriction): re-adding the same session ARN is an
	// idempotent no-op, while a different ARN while one is attached is rejected.
	OpAdd Op = "add"
	// OpRemove disconnects the attached device. With an ARN, only if it matches; with
	// none, whichever device is attached (one at a time today).
	OpRemove Op = "remove"
	// OpStatus reports the listen port, whether reverse is on and the connected devices;
	// with none, why and when the daemon last detached one on its own.
	OpStatus Op = "status"
	// OpList reports the connected devices alone, each as an AttachedDevice; with none,
	// the last detach as OpStatus does.
	OpList Op = "list"
	// OpShutdown stops the daemon after replying.
	OpShutdown Op = "shutdown"
)

// Request is one control command. ARN names the session for OpAdd (the daemon
// resolves it to a websocket endpoint and device label itself) and, when given,
// confirms which device to drop for OpRemove.
type Request struct {
	Op  Op     `json:"op"`
	ARN string `json:"arn,omitempty"`
}

// Response is the daemon's reply. OK reports whether the operation succeeded;
// Error carries the reason when it did not. Fixes carries the remedies the daemon
// knows (failure.Split); the CLI prints each as a fix line. DeviceLabel and Port
// describe the daemon's resulting state so the CLI can print the listening line
// without a second round trip. Devices is one entry per connected device; OpStatus
// and OpList both fill it. An OpList reply carries no Port. OpStatus also fills ARN
// (the attached session) and ReverseEnabled (--allow-adb-reverse was set).
// LastDetachReason and LastDetachAt say why and when the daemon last detached a
// device on its own (a session gone or ended);
// OpStatus and OpList carry them while no device is attached, and a remove by the user
// leaves them empty.
type Response struct {
	OK               bool             `json:"ok"`
	Error            string           `json:"error,omitempty"`
	Fixes            []string         `json:"fixes,omitempty"`
	DeviceLabel      string           `json:"deviceLabel,omitempty"`
	Port             int              `json:"port,omitempty"`
	Devices          []AttachedDevice `json:"devices,omitempty"`
	ARN              string           `json:"arn,omitempty"`
	ReverseEnabled   bool             `json:"reverseEnabled,omitempty"`
	LastDetachReason string           `json:"lastDetachReason,omitempty"`
	LastDetachAt     time.Time        `json:"lastDetachAt,omitzero"`
}

// AttachedDevice is one connected device in an OpStatus or OpList reply: its session ARN,
// label and adb serial, when the daemon attached it, and when its session started and
// expires. The session times are zero when the service reported no start time.
type AttachedDevice struct {
	SessionARN       string    `json:"sessionArn"`
	Label            string    `json:"label"`
	Serial           string    `json:"serial"`
	AttachedAt       time.Time `json:"attachedAt,omitzero"`
	SessionStartedAt time.Time `json:"sessionStartedAt,omitzero"`
	SessionExpiresAt time.Time `json:"sessionExpiresAt,omitzero"`
}
