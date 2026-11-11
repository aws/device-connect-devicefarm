// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package adbwire

import "strings"

// waitForPrefix opens adb's wait service: wait-for-<transport>-<state>[-<state>...]
// (services.cpp wait_service).
const waitForPrefix = "wait-for-"

// waitForTransports maps the spec's transport word to its selector kind. adb takes the
// transport type from this word, not from a host-usb: or host-local: prefix.
var waitForTransports = map[string]AdbSelectorKind{
	"any":   AdbSelectorAny,
	"usb":   AdbSelectorUSB,
	"local": AdbSelectorLocal,
}

// AdbWaitFor is what a wait-for request waits for: the transport type and the states
// services.cpp wait_service reads off its spec. The struct is the daemon's own.
type AdbWaitFor struct {
	// Kind is the transport word's selector: AdbSelectorAny, AdbSelectorUSB or AdbSelectorLocal.
	Kind AdbSelectorKind
	// Online: the spec lists device or any, which an attached device satisfies.
	Online bool
	// Disconnect: the spec lists disconnect, satisfied while no device matches.
	Disconnect bool
}

// ParseWaitFor reads service, a host service with its prefix cut, as a wait-for request.
// isWaitFor is false unless it starts with wait-for-. failReason is adb's FAIL text for a
// malformed spec, checked in adb's order: too few parts, the transport word, each state word.
// The other states (recovery, rescue, sideload, bootloader) parse and never match here.
func ParseWaitFor(service string) (wait AdbWaitFor, isWaitFor bool, failReason string) {
	spec, isWaitFor := strings.CutPrefix(service, waitForPrefix)
	if !isWaitFor {
		return AdbWaitFor{}, false, ""
	}
	parts := strings.Split(spec, "-")
	if len(parts) < 2 {
		return AdbWaitFor{}, true, "short wait-for-: " + spec
	}
	kind, known := waitForTransports[parts[0]]
	if !known {
		return AdbWaitFor{}, true, "bad wait-for- transport: " + spec
	}
	wait.Kind = kind
	for _, state := range parts[1:] {
		switch state {
		case "device", "any":
			wait.Online = true
		case "disconnect":
			wait.Disconnect = true
		case "recovery", "rescue", "sideload", "bootloader":
		default:
			return AdbWaitFor{}, true, "bad wait-for- state: " + spec
		}
	}
	return wait, true, ""
}
