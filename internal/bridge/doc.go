// Package bridge is the daemon's data plane: the websocket dialer and the durable adb
// session over it, the byte pipe between a local adb client and the device, the
// attachment state, the serial fetch, the host reply cache, the reverse pair (notifier and
// connectors) and the accept seam that reads a client's first requests before the dial.
//
// Every name here is the daemon's own unless its comment marks it adb's model; the two
// markers and the adb versions they cover are defined in the internal/adbwire package doc,
// with the Adb prefix rule for adbwire and forward. AdbProxyServer is the daemon's own.
package bridge
