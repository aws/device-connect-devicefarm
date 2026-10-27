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
	// OpStatus reports whether a device is connected, its label, and the daemon's
	// listen port.
	OpStatus Op = "status"
	// OpList reports every connected device, at most one today, each with its
	// session ARN.
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
// Error carries the reason when it did not. DeviceLabel and Port describe the
// daemon's resulting state so the CLI can print the listening line without a
// second round trip. Devices is the OpList reply: one entry per connected device. An
// OpList reply carries no Port. OpStatus also fills ARN (the attached session) and
// ReverseEnabled (--allow-adb-reverse was set).
type Response struct {
	OK             bool             `json:"ok"`
	Error          string           `json:"error,omitempty"`
	DeviceLabel    string           `json:"deviceLabel,omitempty"`
	Port           int              `json:"port,omitempty"`
	Devices        []AttachedDevice `json:"devices,omitempty"`
	ARN            string           `json:"arn,omitempty"`
	ReverseEnabled bool             `json:"reverseEnabled,omitempty"`
}

// AttachedDevice is one connected device in an OpList reply.
type AttachedDevice struct {
	SessionARN string `json:"sessionArn"`
	Label      string `json:"label"`
}
