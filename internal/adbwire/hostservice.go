// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package adbwire

import (
	"fmt"
	"strconv"
	"strings"
)

// AdbSelectorKind is which transport-selection rule a host prefix or transport
// select names. adb's model: the prefixes sockets.cpp smart_socket_enqueue
// dispatches on. Any, USB and Local are adb's TransportType names (adb.h
// kTransportAny, kTransportUsb, kTransportLocal); Serial and TransportID carry
// a target. The Go enum is the daemon's own.
type AdbSelectorKind int

const (
	// AdbSelectorAny is "host:": any single device.
	AdbSelectorAny AdbSelectorKind = iota
	// AdbSelectorUSB is "host-usb:": any single USB device.
	AdbSelectorUSB
	// AdbSelectorLocal is "host-local:": any single TCP/emulator device. The
	// daemon resolves it by the emulator- serial prefix (forward.SelectorMiss).
	AdbSelectorLocal
	// AdbSelectorSerial is "host-serial:<serial>:": the named device.
	AdbSelectorSerial
	// AdbSelectorTransportID is "host-transport-id:<id>:": the numbered transport.
	AdbSelectorTransportID
)

// AdbDeviceSelector is the device a host prefix or transport select addresses.
// adb's model: the transport type, serial and id that sockets.cpp
// smart_socket_enqueue reads off a prefix, and adb.cpp handle_host_request off
// a select, before acquiring a transport. The struct is the daemon's own.
type AdbDeviceSelector struct {
	Kind        AdbSelectorKind
	Serial      string // AdbSelectorSerial only
	TransportID uint64 // AdbSelectorTransportID only; adb reads 0 as any device
}

// unsupportedSerialPrefixes are the serial grammars of adb's parse_host_service
// this parser does not implement: the named-transport prefixes, the fastboot
// protocol prefixes, vsock and a bracketed IPv6 address. No Device Farm client
// is expected to send one; such a request is refused whole rather than
// half-parsed, and the server answers it.
var unsupportedSerialPrefixes = []string{
	"usb:", "product:", "model:", "device:", "localfilesystem:",
	"tcp:", "udp:", "vsock:", "[",
}

// SplitHostPrefix splits a smart-socket request into its device selector and
// the bare service that follows, mirroring the prefix dispatch in adb's
// sockets.cpp (smart_socket_enqueue, parse_host_service). ok is false when the
// request has no host prefix, when the prefix is malformed or when the service
// is empty; the byte-blind path owns those requests.
//
// Serial parsing accepts adb's plain form (scan to the next colon) plus its
// port-merge rule: one following all-digits segment joins any serial, so
// "192.168.0.1:5555" survives. The grammars in unsupportedSerialPrefixes are
// refused. An empty serial (`-s ”`, ANDROID_SERIAL=”) is refused; adb reads
// it as any device.
//
// Request order on one connection, from the adb client source
// (client/commandline.cpp, adb_client.cpp). `adb forward` set and remove send
// a transport select first (host:tport:any, host:tport:serial:<s> or
// host:transport-id:<n>), then the request under the plain host: prefix:
// host:forward:..., host:killforward:... or host:killforward-all. adb's host
// server obeys the select and reads no device off that prefix. `adb forward
// --list` is one host:list-forward request, with no features probe. reverse:*
// (`--list` included) comes bare behind the same select, unclaimed by this
// splitter. The set form also opens a separate connection first, asking
// host:features, host-serial:<s>:features or host-transport-id:<n>:features,
// and a FAIL there kills the command before any forward request. Android
// Studio's adb libraries select with host:transport:<serial> before every device
// service and reverse:* request; a forward from them is one
// host-serial:<s>:forward:... request, no select.
func SplitHostPrefix(request string) (AdbDeviceSelector, string, bool) {
	var selector AdbDeviceSelector
	var service string
	if rest, found := strings.CutPrefix(request, "host-serial:"); found {
		serial, bare, ok := splitSerial(rest)
		if !ok {
			return AdbDeviceSelector{}, "", false
		}
		selector, service = AdbDeviceSelector{Kind: AdbSelectorSerial, Serial: serial}, bare
	} else if rest, found := strings.CutPrefix(request, "host-transport-id:"); found {
		digits, bare, hasService := strings.Cut(rest, ":")
		transportID, err := strconv.ParseUint(digits, 10, 64)
		if !hasService || err != nil {
			return AdbDeviceSelector{}, "", false
		}
		selector, service = AdbDeviceSelector{Kind: AdbSelectorTransportID, TransportID: transportID}, bare
	} else if rest, found := strings.CutPrefix(request, "host-usb:"); found {
		selector, service = AdbDeviceSelector{Kind: AdbSelectorUSB}, rest
	} else if rest, found := strings.CutPrefix(request, "host-local:"); found {
		selector, service = AdbDeviceSelector{Kind: AdbSelectorLocal}, rest
	} else if rest, found := strings.CutPrefix(request, "host:"); found {
		selector, service = AdbDeviceSelector{Kind: AdbSelectorAny}, rest
	} else {
		return AdbDeviceSelector{}, "", false
	}
	if service == "" {
		return AdbDeviceSelector{}, "", false
	}
	return selector, service, true
}

// ParseTransportSelect reads the service half of a host request as a transport
// select, the request that moves a connection off the host services and onto
// one device's own. carriesID reports which reply the client waits for: a
// tport: spelling wants OKAY plus the 8-byte transport id
// (WriteTransportSelect), every other spelling a bare OKAY (WriteOkay). ok is
// false for every other service, and for a select whose target this parser does
// not implement; the byte-blind path owns those, so a refused select must reach
// the device verbatim or a client waiting for an id waits forever.
//
// adb's model: the forms are switch_socket_transport's (adb_client.cpp): the
// tport: spellings for a transport type or a serial, transport-id:<n> for `-t`,
// and the older transport-any|usb|local and transport:<serial>, which adb's
// server answers (adb.cpp handle_host_request) and Android Studio's adb
// libraries send. All forms are frozen, so they are matched whole and no arm
// depends on being tried before another. Any transport id parses; the caller
// judges it against the daemon's own.
func ParseTransportSelect(service string) (selector AdbDeviceSelector, carriesID bool, ok bool) {
	switch service {
	case "tport:any":
		return AdbDeviceSelector{Kind: AdbSelectorAny}, true, true
	case "tport:usb":
		return AdbDeviceSelector{Kind: AdbSelectorUSB}, true, true
	case "tport:local":
		return AdbDeviceSelector{Kind: AdbSelectorLocal}, true, true
	case "transport-any":
		return AdbDeviceSelector{Kind: AdbSelectorAny}, false, true
	case "transport-usb":
		return AdbDeviceSelector{Kind: AdbSelectorUSB}, false, true
	case "transport-local":
		return AdbDeviceSelector{Kind: AdbSelectorLocal}, false, true
	}
	if serial, found := strings.CutPrefix(service, "tport:serial:"); found {
		selector, ok = serialSelector(serial)
		if !ok {
			return AdbDeviceSelector{}, false, false
		}
		return selector, true, true
	}
	if serial, found := strings.CutPrefix(service, "transport:"); found {
		selector, ok = serialSelector(serial)
		return selector, false, ok
	}
	if digits, found := strings.CutPrefix(service, "transport-id:"); found {
		transportID, err := strconv.ParseUint(digits, 10, 64)
		if err != nil {
			return AdbDeviceSelector{}, false, false
		}
		return AdbDeviceSelector{Kind: AdbSelectorTransportID, TransportID: transportID}, false, true
	}
	return AdbDeviceSelector{}, false, false
}

// serialSelector checks a select's serial: non-empty and in a grammar this
// parser implements. The serial runs to the end of a select, so there is no
// port to merge back in.
func serialSelector(serial string) (AdbDeviceSelector, bool) {
	if serial == "" || hasUnsupportedSerialPrefix(serial) {
		return AdbDeviceSelector{}, false
	}
	return AdbDeviceSelector{Kind: AdbSelectorSerial, Serial: serial}, true
}

// hasUnsupportedSerialPrefix reports whether text opens with one of the serial
// grammars this parser refuses whole.
func hasUnsupportedSerialPrefix(text string) bool {
	for _, prefix := range unsupportedSerialPrefixes {
		if strings.HasPrefix(text, prefix) {
			return true
		}
	}
	return false
}

// splitSerial implements parse_host_service's plain path: the serial runs to
// the next colon, and one following all-digits segment is merged in as a
// port. The serial must be non-empty; the caller checks the service.
func splitSerial(rest string) (serial, service string, ok bool) {
	if hasUnsupportedSerialPrefix(rest) {
		return "", "", false
	}
	serial, service, found := strings.Cut(rest, ":")
	if !found || serial == "" {
		return "", "", false
	}
	if port, remainder, found := strings.Cut(service, ":"); found && isPort(port) {
		return fmt.Sprintf("%s:%s", serial, port), remainder, true
	}
	return serial, service, true
}

// isPort mirrors adb's port-merge test: every byte a decimal digit, at least
// one. adb also merges an empty segment; this parser refuses it, since no
// client sends one.
func isPort(segment string) bool {
	return segment != "" && strings.Trim(segment, "0123456789") == ""
}
