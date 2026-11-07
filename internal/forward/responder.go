// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package forward

import (
	"fmt"
	"io"
	"strings"

	"github.com/aws/device-connect-devicefarm/internal/adbwire"
)

// Respond is the daemon's own handle_forward_request: it answers one parsed
// forward-family request with the adb host server's exact reply bytes and
// closes conn, as adb closes a host service after its reply. With the device
// gone, forward: and killforward: get a FAIL carrying adbwire.MsgNoDevices,
// which the caller writes; a closed table's Install returns the same text.
// killforward-all and list-forward still answer here, over the emptied table.
//
// The returned error is a write failure, for logging. Refusing a request is not
// an error: it reaches the client as a FAIL.
func Respond(conn io.WriteCloser, selector adbwire.AdbDeviceSelector, request adbwire.AdbForwardRequest, deviceSerial string, table *ForwardRulesTable) error {
	defer conn.Close()

	// adb acquires the transport before it reads the request pieces (see
	// adbwire.ParseForwardService), so a device miss outranks both a malformed
	// request and a listener failure.
	// killforward-all and list-forward never acquire one, so they answer
	// whatever device the prefix names.
	if request.Action == adbwire.AdbActionForward || request.Action == adbwire.AdbActionKillForward {
		if reason, selectorMissed := SelectorMiss(selector, deviceSerial); selectorMissed {
			return adbwire.WriteFail(conn, reason)
		}
	}

	switch request.Action {
	case adbwire.AdbActionForward:
		result := table.Install(request.Local, request.Remote, request.NoRebind)
		if result.Status != AdbInstallOK {
			return adbwire.WriteFail(conn, result.FailReason)
		}
		return adbwire.WriteForwardSuccess(conn, result.ResolvedPort)
	case adbwire.AdbActionKillForward:
		if !table.Remove(request.Local) {
			return adbwire.WriteFail(conn, adbwire.FailListenerNotFound(request.Local))
		}
		return adbwire.WriteForwardSuccess(conn, 0)
	case adbwire.AdbActionKillForwardAll:
		table.RemoveAll()
		return adbwire.WriteForwardSuccess(conn, 0)
	case adbwire.AdbActionListForward:
		return adbwire.WriteListForward(conn, adbwire.FormatListForward(listRows(table, deviceSerial)))
	default:
		return fmt.Errorf("unhandled forward action %d", request.Action)
	}
}

// isEmulatorSerial reports whether serial names an emulator. The daemon's
// stand-in for the transport-type match in acquire_one_transport: an
// emulator-<console port> serial (transport_local.cpp) is local, anything else
// usb; a network serial counts as usb here where adb counts it local.
func isEmulatorSerial(serial string) bool {
	return strings.HasPrefix(serial, "emulator-")
}

// SelectorMiss reports whether selector names anything but the attached device,
// with adb's FAIL text when it does. The daemon's own stand-in for adb's
// transport lookup (transport.cpp acquire_one_transport), whose miss texts it
// uses. A serial misses unless it is the device's; usb misses an emulator and
// local anything else. The any selector matches the single attached device,
// and so does a transport id: the ids this daemon reports are its own, adb
// reads 0 as unset, and a client that echoes an id back must reach its device.
func SelectorMiss(selector adbwire.AdbDeviceSelector, deviceSerial string) (string, bool) {
	isEmulator := isEmulatorSerial(deviceSerial)
	switch {
	case selector.Kind == adbwire.AdbSelectorSerial && selector.Serial != deviceSerial:
		return adbwire.FailDeviceNotFound(selector.Serial), true
	case selector.Kind == adbwire.AdbSelectorUSB && isEmulator:
		return adbwire.MsgNoUSBDevices, true
	case selector.Kind == adbwire.AdbSelectorLocal && !isEmulator:
		return adbwire.MsgNoEmulators, true
	}
	return "", false
}

// listRows fills in the serial column the table does not record. The selector
// is ignored, as adb lists every device's rules.
func listRows(table *ForwardRulesTable, deviceSerial string) []adbwire.AdbListedForward {
	rows := table.List()
	for i := range rows {
		rows[i].Serial = deviceSerial
	}
	return rows
}
