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
// closes conn, as adb closes a host service after its reply. The caller judges
// the selector first (SelectorMiss), so only the table can refuse here. A
// closed table's Install returns adbwire.MsgNoDevices. killforward-all and
// list-forward still answer here, over the emptied table.
//
// The returned error is a write failure, for logging. Refusing a request is not
// an error: it reaches the client as a FAIL.
func Respond(conn io.WriteCloser, request adbwire.AdbForwardRequest, deviceSerial string, table *ForwardRulesTable) error {
	defer conn.Close()

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
// with adb's FAIL text when it does (MissReason). Stand-in for adb's transport
// lookup (transport.cpp acquire_one_transport). The any selector matches. A
// serial must be the device's. usb misses an emulator; local misses anything
// else. A transport id must be deviceTransportID; id 0 is any, as adb reads 0
// as unset.
func SelectorMiss(selector adbwire.AdbDeviceSelector, deviceSerial string, deviceTransportID uint64) (failReason string, missed bool) {
	isEmulator := isEmulatorSerial(deviceSerial)
	switch selector.Kind {
	case adbwire.AdbSelectorSerial:
		missed = selector.Serial != deviceSerial
	case adbwire.AdbSelectorUSB:
		missed = isEmulator
	case adbwire.AdbSelectorLocal:
		missed = !isEmulator
	case adbwire.AdbSelectorTransportID:
		missed = selector.TransportID != 0 && selector.TransportID != deviceTransportID
	}
	if !missed {
		return "", false
	}
	return MissReason(selector), true
}

// MissReason is adb's FAIL text for a selector that names no attached device,
// or for any selector with no device (transport.cpp acquire_one_transport).
func MissReason(selector adbwire.AdbDeviceSelector) string {
	switch selector.Kind {
	case adbwire.AdbSelectorSerial:
		return adbwire.FailDeviceNotFound(selector.Serial)
	case adbwire.AdbSelectorUSB:
		return adbwire.MsgNoUSBDevices
	case adbwire.AdbSelectorLocal:
		return adbwire.MsgNoEmulators
	case adbwire.AdbSelectorTransportID:
		if selector.TransportID != 0 {
			return adbwire.FailNoTransportID(selector.TransportID)
		}
	}
	return adbwire.MsgNoDevices
}

// listRows fills in the serial column the table does not record. adb lists
// every device's rules, so no selector applies.
func listRows(table *ForwardRulesTable, deviceSerial string) []adbwire.AdbListedForward {
	rows := table.List()
	for i := range rows {
		rows[i].Serial = deviceSerial
	}
	return rows
}
