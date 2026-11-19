// Package control is the daemon's control channel: a per-user Unix-domain socket
// over which the foreground CLI mutates a running detached daemon (add or remove
// the connected device, query status, shut it down). Signals cannot carry an ARN
// or return a device label, so a small request/response protocol fills that gap.
//
// One connection carries exactly one request and one response, both single JSON
// objects; the CLI opens a fresh connection per command. The request carries only
// the session ARN: the daemon resolves it to a websocket endpoint itself (and
// refreshes it periodically), so the CLI does no AWS work and the wire protocol
// stays credential-free.
package control

// Op is the control operation a request carries.
type Op string

const (
	// OpAdd connects a device: the daemon resolves the request ARN to a websocket
	// endpoint and binds it as the active device. The tool holds one device at a
	// time (a temporary restriction): re-adding the same session ARN is an
	// idempotent no-op, while a different ARN while one is attached is rejected.
	OpAdd Op = "add"
	// OpRemove disconnects the active device if its ARN matches the request.
	OpRemove Op = "remove"
	// OpStatus reports whether a device is connected, its label, and the daemon's
	// listen port.
	OpStatus Op = "status"
	// OpShutdown stops the daemon after replying.
	OpShutdown Op = "shutdown"
)

// Request is one control command. ARN names the session for OpAdd (the daemon
// resolves it to a websocket endpoint and device label itself) and confirms which
// device to drop for OpRemove.
type Request struct {
	Op  Op     `json:"op"`
	ARN string `json:"arn,omitempty"`
}

// Response is the daemon's reply. OK reports whether the operation succeeded;
// Error carries the reason when it did not. DeviceLabel and Port describe the
// daemon's resulting state so the CLI can print the listening line without a
// second round trip.
type Response struct {
	OK          bool   `json:"ok"`
	Error       string `json:"error,omitempty"`
	DeviceLabel string `json:"deviceLabel,omitempty"`
	Port        int    `json:"port,omitempty"`
}
