# Device Connect for AWS Device Farm

AWS Device Farm hosts real mobile devices in the cloud. A remote access session gives you live control of one
of them. Device Connect for AWS Device Farm lets you use `adb`, the Android command-line tool, with that device
as if it were plugged into your computer. You run one program, `devicefarm-connect`. It acts as the adb server
for your local `adb` command and relays each adb connection over an encrypted WebSocket to the device. It is
for developers and testers who use Device Farm devices from the command line.

## Requirements

- A Mac with macOS 13 or later (Apple silicon or Intel), or Linux on x86-64 or ARM64. This release has no
  Windows download; see "Platform support".
- The `adb` command from the Android SDK platform tools. The program works with adb 1.0.41 and is
  tested with platform-tools 37.
- A Device Farm remote access session and its ARN (Amazon Resource Name).
- AWS credentials that allow `devicefarm:GetRemoteAccessSession` on that session.

## Install

1. Open this repository's Releases page and download the file for your computer. On Linux, `uname -m`
   prints `x86_64` or `aarch64`.

   | Your computer | File |
   |---|---|
   | Mac, Apple silicon or Intel | `devicefarm-connect_darwin_universal` |
   | Linux, x86-64 (`x86_64`) | `devicefarm-connect_linux_amd64` |
   | Linux, ARM64 (`aarch64`) | `devicefarm-connect_linux_arm64` |

2. Verify the file (see "Verify this download") before you rename it. The checksum list uses the original
   file name.

3. In the download directory, rename the file to `devicefarm-connect` and make it executable. This example
   uses the Linux x86-64 file.

   ```sh
   mv devicefarm-connect_linux_amd64 devicefarm-connect
   chmod +x devicefarm-connect
   ```

4. Move it to a directory on your `PATH`, such as `/usr/local/bin`.

   ```sh
   sudo mv devicefarm-connect /usr/local/bin/
   ```

5. Check that it runs. It prints the version.

   ```sh
   devicefarm-connect --version
   ```

The macOS file is signed with Amazon's Apple Developer ID and notarized by Apple. On the first run, macOS
checks the notarization online and then runs the file without a warning.

## Verify this download

For each release, there are two ways to verify that the release was built by the AWS Device Farm team. Way 1
works on every platform and covers every release file. Way 2 works on macOS only and covers the macOS file. On
a Mac, do both.

### Way 1: check the signed checksum list

A checksum is a short code computed from a file's bytes. Change one byte and the checksum changes.
`SHA256SUMS` lists the SHA-256 checksum of every release file. `SHA256SUMS.sig` is the team's OpenPGP
signature of that list. A good signature proves the team wrote the list, and a matching checksum proves your
file is on it.

You need `curl` and GnuPG (`gpg` and `gpgv`). The team's public key is published on the AWS Device Farm
documentation page at `<KEY_URL>`. The release notes give the key's fingerprint.

1. Download `SHA256SUMS`, `SHA256SUMS.sig` and your release file into one directory.

2. Download the public key and convert it to a keyring file. The command prints nothing.

   ```sh
   curl -fsSL <KEY_URL> | gpg --dearmor > release-key.gpg
   ```

3. Check the signature of the checksum list.

   ```sh
   gpgv --keyring ./release-key.gpg SHA256SUMS.sig SHA256SUMS
   ```

   The output must contain `Good signature from`. If it does not, stop. A matching checksum proves nothing
   without a good signature.

4. Check the checksum of your file. Put your file's name in the `grep` pattern. On Linux:

   ```sh
   grep ' devicefarm-connect_linux_amd64$' SHA256SUMS | sha256sum -c -
   ```

   On macOS, `shasum -a 256 -c -` replaces `sha256sum -c -`:

   ```sh
   grep ' devicefarm-connect_darwin_universal$' SHA256SUMS | shasum -a 256 -c -
   ```

   The output must end with `OK`.

### Way 2 (macOS only): confirm who signed the file and that Apple notarized it

Notarization means Apple checked the file and recorded it under the developer who signed it. Your Mac must be
online for step 2.

1. Print the signature.

   ```sh
   codesign -dv --verbose=4 devicefarm-connect_darwin_universal
   ```

   The first `Authority=` line starts with `Developer ID Application:` and names the Amazon company that
   holds the Developer ID. The `TeamIdentifier=` line is that company's Apple team id.

2. Ask macOS to assess the file.

   ```sh
   spctl --assess --verbose --type execute devicefarm-connect_darwin_universal
   ```

   The output must say `accepted` and `source=Notarized Developer ID`. Those lines prove that Apple notarized
   this exact file under that Developer ID.

## Release files

| File | What it is |
|---|---|
| `devicefarm-connect_darwin_universal` | The program for macOS. One file for Apple silicon and Intel. |
| `devicefarm-connect_linux_amd64` | The program for Linux on x86-64. |
| `devicefarm-connect_linux_arm64` | The program for Linux on ARM64. |
| `devicefarm-connect-vX.Y.Z-src.tar.gz` | The source code at the release tag, in one top directory `devicefarm-connect-vX.Y.Z/`. |
| `SHA256SUMS` | The SHA-256 checksum of each file above. |
| `SHA256SUMS.sig` | The team's OpenPGP signature of `SHA256SUMS`. |

## Usage

`devicefarm-connect` runs a bridge, a background program that stays up until you stop it. Start it once,
attach a device, then use `adb` as usual.

1. Create a remote access session in the Device Farm console or API and copy its ARN.

2. Attach the device. When no bridge runs, `device add` starts one with the default settings, the way `adb`
   starts its own server. It then prints `* bridge not running; starting it now`, the line
   `devicefarm-connect listening on port 5037 with connected device:` and the device name.

   ```sh
   devicefarm-connect device add --session-arn arn:aws:devicefarm:us-west-2:123456789012:session:11111111-2222-3333-4444-5555555abcde
   ```

   If adb's own server holds port 5037, the bridge stops it first, as `adb kill-server` does. If a program
   such as Android Studio restarts adb, quit it, run this, then open it again: it then uses the bridge as its
   adb server. If another bridge holds the port, started from another working directory or by another user,
   the command names its process id and stops nothing. Run `bridge stop` in the shell that started that
   bridge. You can also start the bridge yourself on another port. `bridge start` takes `--port`, and its
   `--session-arn` is optional. Omit it to start with no device, then attach one with `device add`.

   ```sh
   devicefarm-connect bridge start --port 5037 --session-arn arn:aws:devicefarm:us-west-2:123456789012:session:11111111-2222-3333-4444-5555555abcde
   ```

3. Point `adb` at the bridge with `-P` and the same port. The device appears in the list.

   ```sh
   adb -P 5037 devices
   ```

4. Forward a local port to the device, list the forward rules, then remove one. A forward rule sends
   connections made to a port on your computer on to a port or a socket on the device.

   ```sh
   adb -P 5037 forward tcp:8080 tcp:8080
   adb -P 5037 forward --list
   adb -P 5037 forward --remove tcp:8080
   ```

5. Optional: let the device reach a server on your computer with a reverse rule. Reverse is off by default
   and needs `--allow-adb-reverse` on `bridge start`. Read "Security notes" first.

   ```sh
   adb -P 5037 reverse tcp:9000 tcp:9000
   ```

6. Switch devices. `device list` prints a table with one row per attached device: the device name, the adb
   serial, the attach age, and the session start and expiry (see "Device list"). The bridge serves one device
   at a time, so remove the current device before you add another. `device remove` without `--session-arn`
   detaches the attached device. With `--session-arn`, it detaches the device only when the ARN matches. The
   bridge keeps running between the commands. The removed device's forward rules are gone.

   ```sh
   devicefarm-connect device list
   devicefarm-connect device remove
   devicefarm-connect device add --session-arn arn:aws:devicefarm:us-west-2:123456789012:session:66666666-7777-8888-9999-aaaaaaaaaaaa
   ```

7. Check the status, then stop the bridge. Stopping the bridge does not stop your remote access session or
   its billing. The status report goes to stdout; `--json` prints it as one object (`running`,
   `pid`, `port`, `reverseEnabled`, `devices`, `lastDetach`).

   ```sh
   devicefarm-connect bridge status
   devicefarm-connect bridge stop
   ```

### Rules for forward and reverse

- The local end of a forward rule must be `tcp:<port>`. The device end can be `tcp:<port>`,
  `localabstract:<name>`, `localreserved:<name>`, `localfilesystem:<path>` or `jdwp:<pid>`. The bridge
  rejects any other device end, such as `dev:`, and a bad port number when you create the rule.
- The bridge does not check that the socket or process exists. If nothing listens there, the bridge still
  creates the rule, and each connection to it closes with no data.
- `tcp:0` as the local end of a forward rule picks a free port and prints it.
- Both ends of a reverse rule must be `tcp:<port>`. The remote access service rejects other kinds and
  `tcp:0`.
- The bridge rejects a reverse rule that targets its own port. adb prints
  `error: cannot reverse to tcp:<port>: the bridge listens on that port`.
- Forward rules belong to the attached device. `device remove`, `bridge stop`, or a failed refresh of the
  session endpoint removes them all.
- Reverse rules are stored in the remote access session, so `bridge stop` leaves them in place. Each time you
  attach the device with reverse on, the bridge clears them. Run your `adb reverse` commands again.
- The USB device discovery of the `chrome://inspect` page of Chrome on your computer does not work through
  the program. To inspect Chrome on the device, open Chrome there and forward a local port to its socket.
  Then check that `localhost:9222` is listed under "Discover network targets" on that page, or add it.

  ```sh
  adb -P 5037 forward tcp:9222 localabstract:chrome_devtools_remote
  ```

### Device list

`device list` prints one table row per attached device on stdout; a long device name wraps inside
its row. The table follows the terminal width, or `COLUMNS` when stdout is not a terminal, between
60 and 120 columns. At 80 columns:

```
+----------------+-------------+----------------+------------------------------+
| Device         | Serial      | Attached       | Session                      |
+----------------+-------------+----------------+------------------------------+
| Google Pixel 9 | R3CN30ABCDE | 12 minutes ago | started 25 minutes ago       |
|   (OS 16)      |             |                | expires in 2 hours 5 minutes |
+----------------+-------------+----------------+------------------------------+
```

`device list --list` prints one tab-separated line of bare values per device instead: the
session ARN, the name, the serial, the attach age (`<age> ago`), the session age (`<age> ago`)
and the time to expiry (`in <span>`). An unknown value reads `unknown`; a passed expiry reads
`expired`.
`device list --json` prints one JSON object. `devices` holds one object per device: `sessionArn`,
`label`, `serial`, `attachedAt`, `sessionStartedAt` and `sessionExpiresAt`. The bridge reads the
session's time left from the server at `device add`, computes the expiry from it, and derives
the start as the expiry minus the session limit. The times are RFC 3339; a time the bridge
could not read is absent. With no device, `devices` is `[]`, and `lastDetach`
(`reason`, `at`) appears when the bridge detached the last device itself. Without `--json`, no
device prints `no device attached; use 'device add' to connect one` on stderr, then `the last
device detached <age> ago: <reason>` when the bridge detached it itself.

## Doctor

```sh
# Check this machine and this shell before 'bridge start'.
devicefarm-connect doctor

# Also check the session, the connection to the device, and the remote adb server's version.
devicefarm-connect doctor --session-arn arn:aws:devicefarm:us-west-2:123456789012:session:11111111-2222-3333-4444-5555555abcde
```

`doctor` runs readiness checks for `bridge start`, `device add`, an adb connection, a reconnect to
the device, `adb forward`, and `adb reverse`, and prints one table row per category on stdout:
Local machine (the working directory, the bridge, and the ports it needs), AWS (this
shell's credentials and the remote access session), Network (the adb connection to the device
through Device Farm, its reconnect, and adb reverse), and Dependencies (the adb installed here
against the device's adb server). A row is `FAIL` when a check in it fails, `WARN` when one
works with a caveat, `SKIP` when none run, else `PASS`; the Details column names each check that
did not pass and why. On a machine with no bridge running, `devicefarm-connect doctor --port 60001`
(a port nothing holds) prints:

```
+---------------+--------+-----------------------------------------------------+
| Category      | Status | Details                                             |
+---------------+--------+-----------------------------------------------------+
| Local machine | PASS   | working directory, bridge, listen port, adb forward |
+---------------+--------+-----------------------------------------------------+
| AWS           | PASS   | AWS credentials                                     |
|               |        | remote access session not checked: pass             |
|               |        |   --session-arn to check the session, the device    |
|               |        |   connection, and adb versions                      |
+---------------+--------+-----------------------------------------------------+
| Network       | SKIP   | not checked: needs remote access session            |
+---------------+--------+-----------------------------------------------------+
| Dependencies  | SKIP   | not checked: needs remote access session            |
+---------------+--------+-----------------------------------------------------+
doctor: 2 of 4 categories passed, 2 not checked
```

A `WARN` or `FAIL` check adds a numbered item to a "What to do" list under the table: the cause,
the exact command or setting with the check's values filled in, and what the command changes.
With an adb server holding the listen port, `devicefarm-connect doctor --port 35477` prints this
and exits 0:

```
+---------------+--------+-----------------------------------------------------+
| Category      | Status | Details                                             |
+---------------+--------+-----------------------------------------------------+
| Local machine | WARN   | listen port: an adb server (version 41) is          |
|               |        |   listening on 127.0.0.1:35477                      |
|               |        | also passed: working directory, bridge, adb forward |
+---------------+--------+-----------------------------------------------------+
| AWS           | PASS   | AWS credentials                                     |
|               |        | remote access session not checked: pass             |
|               |        |   --session-arn to check the session, the device    |
|               |        |   connection, and adb versions                      |
+---------------+--------+-----------------------------------------------------+
| Network       | SKIP   | not checked: needs remote access session            |
+---------------+--------+-----------------------------------------------------+
| Dependencies  | SKIP   | not checked: needs remote access session            |
+---------------+--------+-----------------------------------------------------+

What to do
1. Local machine, listen port: an adb server is listening on 127.0.0.1:35477, so
   the bridge cannot bind it until that server stops. Run
   'devicefarm-connect bridge start --port 35477': it stops that server first,
   as 'adb kill-server' would. If a program that restarts adb, such as Android
   Studio, takes the port back, start the bridge on another port with
   'devicefarm-connect bridge start --port <n>' and point adb at it with
   'adb -P <n>'.
doctor: 1 of 4 categories passed, 1 warning, 2 not checked
```

Exit code 0 unless a check is `FAIL`; a `WARN` leaves it 0. The table follows the terminal width,
or `COLUMNS` when stdout is not a terminal, between 60 and 120 columns. Flags: `--session-arn`
(optional; defaults to the session of the one device the running bridge holds; with no bridge, no
device, or several devices, the session, the connection to the device, and the adb versions are not
checked), `--port` and `--listen-addr` (the address the bridge would bind, same defaults as
`bridge start`), `--verbose` (one line per check, with its ID, instead of the table), `--json` (one
compact JSON object instead of the table; same exit code), and the global `--debug`. Run the doctor
from the shell that will run `bridge start`: the bridge inherits that shell's environment. The
doctor reports a running bridge in the Local machine row and does not change it. The row's
`bridge` check is `WARN` when:

- the bridge runs another version of `devicefarm-connect` than this command, for example after
  you install another `devicefarm-connect` while the bridge runs. The fix restarts the bridge with
  the port, session, and reverse setting that the bridge reports. The doctor reports this first,
  because it can misread the status of such a bridge.
- the bridge detached its device. The fix quotes the bridge's reason.
- the `--session-arn` session is not attached. The fix is the `device add` command for it.
- reverse is off. The fix restarts the bridge with `--allow-adb-reverse`. That flag lets the
  device open connections to this machine, so use it only with a device you trust; a public
  physical device can run software you do not control.

Side effects:

- creates the client's working directory if absent, as every command does, and forgets a bridge
  that no longer runs, as the `bridge` commands do;
- binds and frees the listen address when no bridge holds it, and one port on `127.0.0.1` for
  `adb forward`;
- asks whatever holds the listen address for its adb version;
- makes one `GetRemoteAccessSession` call;
- opens one short connection to the device per Network and Dependencies check; the session and
  reconnect checks leave one adb session open on the device side until Device Farm ends it;
- runs `adb version` on each adb it finds on PATH, in `ANDROID_HOME` or `ANDROID_SDK_ROOT`, or in
  Android Studio's default SDK location; none starts an adb server.

`doctor` starts no bridge and installs no forward or reverse rule. Each check has its own timeout;
a run in which every check times out ends in under two minutes.

`--json` prints one compact object on stdout instead of the table, with the same exit code:

```sh
devicefarm-connect doctor --json | jq .ok
```

| Field | Type | Meaning |
|---|---|---|
| `ok` | boolean | `true` when no check is `fail` (the exit code is 0) |
| `sessionArn` | string | the session the doctor checked: the `--session-arn` value, or the one attached session, empty when none |
| `checks` | array | one object per check in run order: `id`, `title`, `category` (`local`, `aws`, `network`, or `dependencies`, the table row), `status` (`pass`, `warn`, `fail`, `skip`), `summary`, `hint` (absent when the row has none: every `pass`, and a `fail` from a timeout or Ctrl-C), `needs` (IDs of the checks it needs), `elapsedMs` |
| `summary` | object | `pass`, `warn`, `fail`, `skip` counts |

Fields may be added in any release; a field is renamed, removed or retyped only in a new major
version of `devicefarm-connect`.

## Credentials and permissions

The bridge makes the AWS calls, not the command you type, so the shell where you start the bridge (`bridge
start`, or `device add` when no bridge runs) must have your AWS credentials. The bridge reads them from the standard AWS SDK credential chain: environment variables,
then your AWS config and credentials files.

The bridge needs one permission: `devicefarm:GetRemoteAccessSession` on the session ARN.

Device Farm remote access is offered only in `us-west-2` today. The bridge targets that region and rejects a
session ARN from any other region.

## Flags

Run `devicefarm-connect <command> --help` to see the flags of a command.

| Flag | Command | What it does |
|---|---|---|
| `--debug` | every command | Prints the command's log records on stderr; a bridge it starts logs at debug level. Without it a command prints no log records. |
| `--version`, `-v` | `devicefarm-connect` alone | Prints the release version. |
| `--port` | `bridge start`, `doctor` | The local TCP port for adb clients. Default `5037`, adb's own default. |
| `--log` | `bridge start` | The bridge log file. Default `devicefarm-connect.log` in the runtime directory. |
| `--allow-adb-reverse` | `bridge start` | Turns on `adb reverse`. Off by default. |
| `--session-arn` | `bridge start`, `device add`, `device remove`, `doctor` | The remote access session ARN. `device remove` without it detaches the attached device. |

- The runtime directory is `$XDG_RUNTIME_DIR/devicefarm-connect`, or a per-user directory under the system
  temp directory when that variable is not set. The log, PID file, and control socket live there.
- `device add` and `device list` start the bridge with the default settings when none runs. They take no
  listener flags, so to pick a port or turn reverse on, run `bridge start` first. `bridge start` stops a
  running bridge in this runtime directory, says so, and starts a new one with the given flags. The restart
  drops attached devices and forward rules, and reverse rules no longer reach your computer.
- `doctor` takes `--port` and `--listen-addr` too: it checks the address the bridge would bind.
- Without `--allow-adb-reverse`, `adb reverse` prints:

  ```
  error: adb reverse is off for this client; restart it with --allow-adb-reverse
  ```

  To turn reverse on, run `bridge start --allow-adb-reverse` with your `--session-arn` and other flags. It
  restarts the bridge.

## Platform support

The release has a program for macOS (Apple silicon and Intel), Linux on x86-64 and Linux on ARM64. The
maintainers run the automated tests on Linux x86-64 only. For the other platforms, the maintainers compile
the program and run `go vet` over all of the code, tests included. No automated test runs on them.

The build also produces a Windows x86-64 binary, but this release has no Windows download. A signed Windows
build follows in a later release. If you build the Windows binary yourself:

- You need Windows 10 build 17063 or later. The program uses a Unix socket for its control channel, and
  older Windows builds have no Unix sockets.
- `adb.exe` connects to `127.0.0.1` only. A bridge that listens on `::1` is not reachable from it.
- `adb reverse` reaches only servers that listen on `127.0.0.1`. A server that listens on `::1` alone is not
  reachable.

## Limits

- One device at a time.
- The local end of a forward rule and both ends of a reverse rule are tcp only.
- Forward rules per device are capped. One more prints `error: too many forward rules (limit <n>)`, where
  `<n>` is the cap. `adb forward --remove` frees a slot.
- Reverse rules per device are capped. One more prints `error: too many reverse rules (limit <n>)`. The
  remote access service stores the rules and the bridge counts them.
- Open adb connections per device are capped. Local, forwarded and reverse connections share one cap, set by
  the remote access service less a reserve, so a busy device can crowd itself out. One more local connection
  prints `error: too many adb connections (limit <n>)`. One more forwarded connection closes with no data. One
  more reverse connection is refused.
- `adb devices`, `adb devices -l` and device trackers answer from a copy of the server's reply. The copy does
  not change while the device stays attached. During an `adb reconnect`, a device crash or a dropped
  connection, it still lists the device as `device` while commands fail.
- A device tracker (`adb track-devices`, Android Studio's device chooser) stays open with or without a
  device. It gets the list again at every `device add`, `device remove` and session end, and nothing in
  between. It ends when its client closes or writes to it, when the bridge stops, when a newly attached
  device's list cannot be copied, or when the client does not read a push in time.
- With no matching device attached, `adb wait-for-device` waits and returns when you attach one,
  `adb wait-for-disconnect` returns at once, and a wait for another state, such as `adb wait-for-recovery`,
  never returns. With a matching device attached, the device's adb server answers them. If that device
  detaches during the wait, the command fails with a protocol error; run it again.

## Security notes

- **Any program on your computer can use the attached device.** The listener has no authentication, like a
  normal adb server. Any process that reaches the port has the same power as your own `adb`.
- **Keep the listener on loopback.** The bridge listens on `127.0.0.1` by default.
  `bridge start --listen-addr <address>` can move it to any address or hostname, and nothing stops you.
  `--help` does not list the flag. Any machine that can reach that address can drive the attached
  device and use the bridge's remote access session, with no password or token. With
  `--allow-adb-reverse`, that machine can also add reverse rules and reach ports that are open only to
  your computer. Move the listener off loopback only on a network you control. Never put it on an
  address the internet can reach. Scanners such as Shodan find open ports fast.
- **`adb reverse` lets the device open connections to your computer.** For each connection the device
  announces, the bridge connects to `127.0.0.1:<port>`, then to `[::1]:<port>` if that is refused. It allows
  only a loopback port that a reverse rule of the attached device names, never its own adb port or a port a
  forward rule uses. With reverse off, `adb reverse` is refused and no notifier opens, so nothing the device
  starts reaches your computer.
- **Credential scope.** The bridge needs only `devicefarm:GetRemoteAccessSession`. It uses the credentials
  it inherited at start and stores none. When they expire, the next refresh fails and the bridge detaches the
  device. `device list`, `bridge status` and every adb command the bridge refuses for want of a device then
  say `the AWS credentials the bridge started with have expired`. Refresh the credentials, then run the
  `bridge start` command that message quotes from that shell. It restarts the bridge and attaches the session again.
- **`bridge stop` does not stop billing.** `bridge stop` and `device remove` act on your computer only. The
  remote access session keeps running, and keeps costing, until you stop it in Device Farm.

## Build from source

There are two ways to build the program from source. Build it with Go, or rebuild the release binaries with
Docker. You can do one without the other.

First, get the source code. Download the source archive from this repository's Releases page and unpack it,
or clone this repository. Run every command in this section from the top directory of the source code. The
top directory holds `go.mod` and the `Dockerfile`.

### Build with Go

You need Go 1.26 or later and an internet connection. Go downloads the modules that the program uses.

1. Build the program. Go writes the file `devicefarm-connect` into the current directory. On Windows, the
   file is `devicefarm-connect.exe`.

   ```sh
   go build ./cmd/devicefarm-connect
   ```

2. Run the tests on Linux. The maintainers do not run the tests on other platforms. See "Platform
   support".

   ```sh
   go test ./...
   ```

3. Optional: install the program. Go puts the program into Go's `bin` directory. By default, that directory
   is `go/bin` in your home directory.

   ```sh
   go install ./cmd/devicefarm-connect
   ```

### Rebuild the release binaries with Docker

The `Dockerfile` rebuilds the release binaries with the same Go version and build flags as the release. The
build does not need Go on your computer.

To compare a rebuild with a release, build the source code of that release. The source archive of the
release holds that source code. In a clone, check out the tag of the release first.

You need Docker and an internet connection. The build downloads the official Go image, which the Docker
community maintains, from the Amazon ECR Public Gallery, and Go modules from the Go module proxy.

1. Run the build. The `--pull` option makes Docker download the newest copy of the Go image.

   ```sh
   docker build --pull --output type=local,dest=dist .
   ```

   Docker writes six files into the directory `dist/`:

   | File | What it is |
   |---|---|
   | `devicefarm-connect_darwin_universal` | The program for macOS. One file for Apple silicon and Intel. |
   | `devicefarm-connect_darwin_arm64` | The Apple silicon half of the macOS file. |
   | `devicefarm-connect_darwin_amd64` | The Intel half of the macOS file. |
   | `devicefarm-connect_linux_amd64` | The program for Linux on x86-64. |
   | `devicefarm-connect_linux_arm64` | The program for Linux on ARM64. |
   | `devicefarm-connect_windows_amd64.exe` | The program for Windows on x86-64. This release has no Windows download to compare it with. |

2. Compare the build information of a rebuilt binary and a release binary. This step needs Go on your
   computer. Run the command below on each file. Both outputs show the same Go version, the same module
   line and the same build settings.

   ```sh
   go version -m dist/devicefarm-connect_linux_amd64
   ```

3. Compare the checksums. The release notes give the SHA-256 checksum of each release binary. The release
   notes also give the checksum of a rebuild that the AWS Device Farm team made with this `Dockerfile`.
   Compare the checksum of your rebuild with both. On macOS, use `shasum -a 256` in place of `sha256sum`.

   ```sh
   sha256sum dist/devicefarm-connect_linux_amd64
   ```

**NOTE**: A rebuild and a release binary are the same program. The AWS Device Farm team does not promise that
the two files match byte for byte. The release build and this `Dockerfile` get Go in different ways, and
they run in different build environments.

The macOS release binary differs from a rebuild in two more ways. The AWS Device Farm team signs the release
binary with a Developer ID, and Apple notarizes it. A rebuilt macOS binary has no Developer ID signature and
no notarization. A rebuilt macOS binary still runs on Apple silicon. The Go linker gives the Apple silicon
half an ad-hoc signature, which is a signature with no developer identity.

## Contributing

Read `CONTRIBUTING.md` first. It explains how a maintainer imports a pull request instead of merging it on
GitHub.

## Security

To report a security issue, follow `SECURITY.md`. Never open a public issue for it.

## License

This project is licensed under the Apache License 2.0. The license text is in `LICENSE`. The copyright
notice is in `NOTICE`.
