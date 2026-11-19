# Device Connect for AWS Device Farm

`devicefarm-connect` presents itself to local `adb` clients as the adb host. It listens
on a local TCP port and proxies each connection over the websocket of an AWS
Device Farm remote access session to the device's adb server. Because both ends terminate at a
genuine adb server, the proxy is a byte pipe once a connection is running. It speaks adb in two
places only. At the accept seam, where it reads a client's first requests before dialing the
device, it answers the forward family (`adb forward` and its list and remove forms) and the
transport select, and refuses the reverse family while reverse is off. And it writes a
forwarded connection's opening requests itself.

## Install

The release downloads and the install steps are added with the first release.

## Verify this download

The verification steps are added with the first signed release.

## Usage

```sh
# Start the background daemon and attach a Device Farm remote access device. The
# --session-arn is optional: omit it to start the daemon with no device attached
# and connect one later with 'device add'.
devicefarm-connect proxy start --port 5037 --session-arn arn:aws:devicefarm:us-west-2:123456789012:session:11111111-2222-3333-4444-5555555abcde

# Point a local adb client at the proxy.
adb -P 5037 devices

# Forward a local port to the device, list the rules, then drop one.
adb -P 5037 forward tcp:8080 tcp:8080
adb -P 5037 forward --list
adb -P 5037 forward --remove tcp:8080

# Let the device reach a server running on this machine. Off by default: the daemon must have
# been started with 'proxy start --allow-adb-reverse'.
adb -P 5037 reverse tcp:9000 tcp:9000

# Detach the active device (the listener stays up).
devicefarm-connect device remove --session-arn arn:aws:devicefarm:us-west-2:123456789012:session:11111111-2222-3333-4444-5555555abcde

# Attach another one. Remove first: a different ARN is refused while a device is attached.
devicefarm-connect device add --session-arn arn:aws:devicefarm:us-west-2:123456789012:session:66666666-7777-8888-9999-aaaaaaaaaaaa

# Report status, then stop the daemon.
devicefarm-connect proxy status
devicefarm-connect proxy stop

# Print the release version.
devicefarm-connect --version
```

Both ends of a forward rule must be `tcp:<port>`: no socket kind (`localabstract:`,
`localfilesystem:`, `dev:`) is served, and an unsupported remote is refused at install, where a
real adb server defers the complaint to connect time. `tcp:0` works and reports the port it
resolved. With `--allow-adb-reverse`, a reverse rule is answered remotely under the same tcp-only
restriction, and `tcp:0` is refused.
Forward rules belong to the attachment, so `device remove`, a failed endpoint refresh, or a daemon
stop drops them all, as restarting a real adb server does; reverse rules live in the remote session
and outlive the daemon, but a new attachment with reverse on clears them once its notifier is up, so
run `adb reverse` again.

## Credentials and permissions

AWS credentials come from the default credential chain, and the daemon (not the
CLI) makes the AWS calls, so the credentials must be visible in the environment it
inherits. The daemon needs one permission: `devicefarm:GetRemoteAccessSession` on the
session ARN. Device Farm remote access is only offered in `us-west-2` today, so the
client targets that region and a session ARN from any other region is rejected
(AWS endpoint environment overrides are still honored).

## Flags

Flags are scoped to the command they affect (see `devicefarm-connect <command> --help`).
`--debug` (enable debug-level logging; the default is info) is global. `--version` (or `-v`)
is accepted by the bare command only. The daemon's
listener and logfile flags belong to `proxy start`, the only command that stands up
the daemon: `--port` (default `5037`, matching adb's own default server port),
`--listen-addr` (default `127.0.0.1`), and `--log` (the daemon logfile, defaulting
to `devicefarm-connect.log` in the per-user runtime directory alongside the pidfile and
control socket). `--allow-adb-reverse` belongs to the same command. Without it,
`adb reverse` is off, and adb prints
`error: adb reverse is off for this client; restart it with --allow-adb-reverse`. To
turn it on, run `proxy stop`, then `proxy start --allow-adb-reverse`. The other
commands find the running daemon through its control socket, so they do not take
these. `--session-arn` is a per-command argument of
`proxy start`, `device add`, and `device remove`. The dial, keep-alive, stop, and
endpoint-refresh timeouts and the websocket-refresh cadence are fixed runtime
constants and are not exposed as flags, keeping the command surface small. A flag
on `proxy start` configures only a daemon that command starts: against a running
one it is accepted and ignored, so stop that daemon first.

## Platform support

Supported (verified by cross-compile): macOS Apple Silicon (`darwin/arm64`), macOS
Intel (`darwin/amd64`), Linux x86-64 (`linux/amd64`), and Linux ARM64
(`linux/arm64`). Windows is not supported yet. The binary compiles for Windows but
exits with a "not supported" message: the daemon's control socket and lifecycle use
Unix primitives that Windows lacks.

## Limits

These limits may change in later releases:

- One device at a time. Routing several attachments is the next step.
- Forward and reverse specs are tcp-only.
- Forward rules per device are capped (`MaxForwardRules`). One more prints
  `error: too many forward rules (limit <n>)` with the cap as `<n>`; `--remove` frees a slot.
- Reverse rules per device are capped (`MaxReverseRules`). One more prints
  `error: too many reverse rules (limit <n>)`. The remote access service keeps the rules; the daemon
  counts them and admits a device's connection only to a port they name.
- Open adb connections per device are capped (`MaxDeviceStreams`), shared by local, forwarded and
  reverse connections. The cap is the number of websockets one remote access service process
  admits, less a reserve. One more local connection prints
  `error: too many adb connections (limit <n>)`; one more forwarded connection closes with no bytes;
  one more reverse connection is refused and closes when the remote access service reaps it.
- No websocket-level ping on a proxied connection: `x/net/websocket` cannot send one. Liveness comes
  from TCP keepalive, the refresh cycle and a reactive reconnect. The reverse notifier, the websocket
  on which the remote access service announces device-originated connections, is the exception: an
  application-level ping keeps it registered.

The tool needs a remote access session whose adb path speaks the session model; `adb reverse` needs
the reverse path as well.

## Security notes

- **Any local process can drive the attached device.** The listener has no authentication, like
  a real adb server, so any process that reaches the port is as privileged as the customer's own
  `adb`.
- **Keep the listener on loopback.** `--listen-addr` defaults to `127.0.0.1` and is only checked
  for non-empty; a routable address publishes the device to whoever can reach this host.
- **`adb reverse` makes the device an initiator.** A reverse rule has the daemon dial
  `127.0.0.1:<port>` (then `[::1]:<port>` if that connect is refused; adb tries the same order
  on any failure) for the device. On each announced connection the daemon refuses its own adb
  port, any port a forward rule of the attachment binds, and any port no reverse rule of the
  attachment names; every other loopback port a rule names is reachable.
  Reverse is off unless `proxy start` carries `--allow-adb-reverse`. Off, `adb reverse` is
  refused and no notifier opens, so nothing the device starts reaches this machine.
- **The websocket endpoint is a bearer credential.** A short-lived presigned URL, held only in
  daemon memory and renewed before it expires (`EndpointRefreshInterval`). It never crosses
  the control channel and no error text quotes it. Only `wss://` is dialed: a session that
  resolves to another scheme is refused before any connection, and the daemon logs
  `only wss endpoints are dialed` with the scheme. Revocation takes effect at the next dial,
  not on an open websocket: the reverse notifier is never swapped, so it keeps working until
  it closes.
- **Credential scope.** The daemon needs only `devicefarm:GetRemoteAccessSession`, uses the
  ambient chain it inherits across the detach, and persists no credential. When those credentials
  expire the next refresh fails and the daemon detaches the device rather than proxy to an
  endpoint it cannot renew.
- **`proxy stop` does not stop billing.** Stopping the daemon, like `device remove`, is local.
  The remote access session keeps running, and costing, until stopped through Device Farm.
- **One attachment's websockets share a server-side ceiling.** A busy attachment can crowd itself
  out: each local, forwarded or reverse connection uses one of its `MaxDeviceStreams` slots.

## Build from source

The build steps are added with the public Dockerfile.
