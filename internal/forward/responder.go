package forward

import (
	"fmt"
	"io"

	"github.com/aws/device-connect-devicefarm/internal/adbwire"
)

// Respond is the daemon's own handle_forward_request: it answers one parsed
// forward-family request with the adb host server's exact reply bytes and
// closes conn, as adb closes a host service after its reply. deviceSerial is
// the attached device's adb serial, empty until the keep-alive probe has parsed
// one. With the device gone, forward: and killforward: get a FAIL carrying
// adbwire.MsgNoDevices, which the caller writes; a closed table's Install returns
// the same text. killforward-all and list-forward still answer here, over the
// emptied table.
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

// SelectorMiss reports whether selector names anything but the attached device,
// with adb's FAIL text when it does. The daemon's own stand-in for adb's
// transport lookup (transport.cpp acquire_one_transport), whose miss texts it
// uses. Only a serial can miss. The any, usb and local selectors match the
// single attached device, and so does a transport id: the ids this daemon
// reports are its own, adb reads 0 as unset, and a client that echoes an id
// back must reach its device. A serial matches too while deviceSerial is empty,
// since one device is attached either way and refusing the customer's own
// serial until the keep-alive probe parses it is worse than serving it.
func SelectorMiss(selector adbwire.AdbDeviceSelector, deviceSerial string) (string, bool) {
	if selector.Kind == adbwire.AdbSelectorSerial && deviceSerial != "" && selector.Serial != deviceSerial {
		return adbwire.FailDeviceNotFound(selector.Serial), true
	}
	return "", false
}

// listRows fills in the serial column the table does not record. The selector
// is ignored, as adb lists every device's rules. A serial the keep-alive probe
// has not parsed yet renders "unknown", so every row keeps three fields.
func listRows(table *ForwardRulesTable, deviceSerial string) []adbwire.AdbListedForward {
	if deviceSerial == "" {
		deviceSerial = "unknown"
	}
	rows := table.List()
	for i := range rows {
		rows[i].Serial = deviceSerial
	}
	return rows
}
