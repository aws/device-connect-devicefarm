# Device Connect for AWS Device Farm

```text
                               .-----.
  .----------.             .--(       )--.             .-----.
  |          |           .(               ).           |  o  |
  |  $ adb   |==========(  AWS Device Farm  )=========>|     |
  |__________|           '-----------------'           |     |
 / :::::::::: \                                        | ___ |
'--------------'                                       '-----'
```

Device Connect for AWS Device Farm connects adb on your computer to the Android device of a Device Farm remote
access session. You can use it to run `adb`, Android Studio, and all other Android tests as though the device
was plugged in locally.

Device Connect is a small, self-contained CLI program (named `devicefarm-connect`) that runs on your local
machine, and can be installed in just a few steps. It runs a bridge to Device Farm as a background process
that acts in place of the adb server on your localhost at port `5037`. Once it starts up, it responds to all
Android tools (like `adb` and Android Studio) as though it was an adb server.

The bridge uses your AWS credentials to get the Device Farm session endpoint via an API call to AWS. Using
this endpoint, the bridge securely relays adb connections over an encrypted WebSocket to the Device Farm adb
server for the device. While a device is attached, the bridge renews the endpoint and keeps its connection to
the device. This means that long-running Espresso tests and commands like `adb shell` keep running, and Device
Farm does not end the session for being idle between commands.

Device Connect supports nearly all of the features of adb with Device Farm devices, including `adb forward`
and `adb reverse`. You can file an issue in GitHub to request support for additional adb functionality that
you would find useful.

Device Connect supports public Device Farm devices that run Android 9 or later. Read
[Security notes](#security-notes) before you use it.

**Documentation:** [Using local adb tools with Device Connect](https://docs.aws.amazon.com/devicefarm/latest/developerguide/device-connect.html)

## Quick start

You need the following:

- `adb` from Android SDK Platform-Tools 29.0.0 or later. `adb version` must print version 1.0.41.
  Alternatively, you can use Device Connect fully from within Android Studio using any modern version of
  Android Studio.
- AWS credentials that allow `devicefarm:GetRemoteAccessSession` on the session. Device Connect finds your
  credentials the same way that the AWS Command Line Interface (AWS CLI) does. In most cases, if `aws`
  commands work in your terminal, Device Connect works there too. For all of the ways to provide
  credentials, see
  [AWS SDKs and Tools standardized credential providers](https://docs.aws.amazon.com/sdkref/latest/guide/standardized-credentials.html).
- The AWS CLI or the Device Farm console, to create and stop the session.

On Windows, run the commands in PowerShell. Where the PowerShell command differs from the macOS and Linux
command, this page shows both, Windows first. All other commands are the same on Windows, macOS, and Linux.

### Install

At this time, Device Connect supports Windows, macOS, and Linux computers:

| Computer | Release file |
|---|---|
| Windows 10 or later, x86-64 | `devicefarm-connect_windows_amd64.exe` |
| macOS 13 or later, Apple silicon or Intel | `devicefarm-connect_darwin_universal` |
| Linux on x86-64 | `devicefarm-connect_linux_amd64` |
| Linux on ARM64 | `devicefarm-connect_linux_arm64` |

There are two ways to install Device Connect: with the install script, or with a manual download.

**Install script.** The script downloads the release file for your computer and verifies its integrity before
installing it. It doesn't require administrator rights or `sudo` access. If you have any questions about what
the install script executes on your computer, we recommend that you download and read the script before you
run it.

On Windows (in PowerShell):

```powershell
irm https://github.com/aws/device-connect-devicefarm/releases/download/v1.0.0/install.ps1 | iex
```

On macOS or Linux:

```sh
curl -fsSL https://github.com/aws/device-connect-devicefarm/releases/download/v1.0.0/install.sh | sh
```

The script installs `devicefarm-connect` v1.0.0 in `.local/bin` in your home directory. If that directory is
not on your `PATH`, the script prints how to add it.

**Manual download.** Do the following steps:

1. Download the release file for your computer from the
   [v1.0.0 release](https://github.com/aws/device-connect-devicefarm/releases/tag/v1.0.0).
2. Verify the file. See [Verify this download](#verify-this-download).
3. Move the file to `.local/bin` in your home directory, which is the directory that the install script uses.
   Name the file `devicefarm-connect` (`devicefarm-connect.exe` on Windows). On macOS or Linux, also make the
   file executable. In PowerShell:
   ```powershell
   New-Item -ItemType Directory -Force "$env:USERPROFILE\.local\bin" | Out-Null
   Move-Item -Force .\devicefarm-connect_windows_amd64.exe "$env:USERPROFILE\.local\bin\devicefarm-connect.exe"
   ```
   On macOS or Linux, use the name of your release file. For example, on Linux on x86-64:
   ```sh
   mkdir -p ~/.local/bin
   chmod 755 devicefarm-connect_linux_amd64
   mv devicefarm-connect_linux_amd64 ~/.local/bin/devicefarm-connect
   ```
4. If `devicefarm-connect --version` does not run yet, add the directory to your `PATH` one time. In
   PowerShell, the first command saves the directory for new terminals, and the second command adds it to the
   open terminal:
   ```powershell
   [Environment]::SetEnvironmentVariable('Path', "$env:USERPROFILE\.local\bin;" + [Environment]::GetEnvironmentVariable('Path', 'User'), 'User')
   $env:Path += ";$env:USERPROFILE\.local\bin"
   ```
   On macOS or Linux, run the following command. For new terminals, also add the line to your shell profile,
   such as `~/.zshrc` for zsh or `~/.bashrc` for bash on Linux:
   ```sh
   export PATH="$HOME/.local/bin:$PATH"
   ```

Then confirm the install:

```sh
devicefarm-connect --version
```

`--version` prints a line such as `devicefarm-connect version v1.0.0`.

For other install options, such as another version or another directory, see
[Install Device Connect](https://docs.aws.amazon.com/devicefarm/latest/developerguide/device-connect-getting-started.html#device-connect-install)
in the *AWS Device Farm Developer Guide*.

### Create a remote access session

> **Important:** Device Farm charges for the session until it ends. For more information, see
> [AWS Device Farm pricing](https://aws.amazon.com/device-farm/pricing/).

1. Create a session on a device that runs Android 9 or later. Device Connect only supports public devices
   which use the metered, pay-as-you-go billing method. Use the Device Farm console or the AWS CLI. For more
   information, see
   [Creating a remote access session in AWS Device Farm](https://docs.aws.amazon.com/devicefarm/latest/developerguide/how-to-create-session.html).
2. Store the Amazon Resource Name (ARN) of the session in a shell variable. On the console, choose
   **Session ARN** to copy the ARN of a running session. Use your ARN instead of the following example. In
   PowerShell:
   ```powershell
   $SESSION_ARN = 'arn:aws:devicefarm:us-west-2:111122223333:session:a1b2c3d4-5678-90ab-cdef-EXAMPLE11111/c3d4e5f6-7890-12cd-ef01-EXAMPLE33333/00000'
   ```
   On macOS or Linux:
   ```sh
   SESSION_ARN='arn:aws:devicefarm:us-west-2:111122223333:session:a1b2c3d4-5678-90ab-cdef-EXAMPLE11111/c3d4e5f6-7890-12cd-ef01-EXAMPLE33333/00000'
   ```

### Attach the device and run adb

If you have a local adb server running on port 5037, the bridge stops it automatically, just as
`adb kill-server` would. `adb` then shows only the Device Farm device, not locally plugged-in devices or local
emulators. To use the bridge without stopping your local adb server, see
[Using a different port](https://docs.aws.amazon.com/devicefarm/latest/developerguide/device-connect-interaction.html#device-connect-custom-port)
in the *AWS Device Farm Developer Guide*.

The following commands attach the device and run adb. The lines that start with `#` show example output:

```sh
devicefarm-connect device add --session-arn "$SESSION_ARN"
# * bridge not running; starting it now
# * bridge started (PID 4242); log file: /run/user/1000/devicefarm-connect/devicefarm-connect.log
# devicefarm-connect listening on port 5037 with connected device: Google Pixel 11 (OS 17)

adb devices
# List of devices attached
# 12345DFDEVICE    device
```

The lines that start with `*` print when `device add` starts the bridge. If `device add` stops a local adb
server first, one more `*` line says so. The last line of `device add` shows the port of the bridge and the
name of the device. `adb devices` lists the device by its serial.

If a command fails, run `doctor`. It shows each problem with your computer, your AWS credentials, the session,
or the connection to the device, and what to do about it:

```sh
devicefarm-connect doctor --session-arn "$SESSION_ARN"
```

Most adb commands now work as with a local device: `shell`, `install`, `uninstall`, `push`, `pull`, `logcat`,
and `forward`. `adb reverse` is disabled by default unless you start the bridge with `--allow-adb-reverse`. To
turn it on, see
[Reaching your computer from the device with adb reverse](https://docs.aws.amazon.com/devicefarm/latest/developerguide/device-connect-interaction.html#device-connect-reverse)
in the *AWS Device Farm Developer Guide*. Once the bridge starts, any program on your computer can control the
device. For more information, see [Security notes](#security-notes).

### Use Android Studio with the bridge

To use Device Connect with Android Studio, first quit any existing Android Studio program windows running on
your computer, and only launch Android Studio again after starting Device Connect from a separate terminal
window. We highly recommend that you start Device Connect before Android Studio. After Android Studio opens,
all of its standard features for the device work, except device mirroring (the **Running Devices** window).

If Android Studio or another program that restarts adb takes port 5037 back, the bridge cannot start. Quit
the program, attach the device, then open the program again. It then uses the bridge as its adb server.

### Clean up

Stop the bridge with `bridge stop`, not `adb kill-server`. Stop the session too, because `bridge stop` and
`device remove` do not end it. On the console, choose **Stop Session**. The following commands stop the bridge
and the session:

```sh
devicefarm-connect bridge stop
aws devicefarm stop-remote-access-session --arn "$SESSION_ARN" --region us-west-2
```

The next `adb` command starts adb's own server again.

## Common tasks

Device Connect supports most common use cases and functionality of adb. Examples include:

| Use case | Example command |
|---|---|
| Installing an app | `adb install example-app.apk` |
| Copying a file to the device | `adb push example-file.txt /sdcard` |
| Running a shell command | `adb shell echo "hello world"` |
| Forwarding a port on your computer to the device | `adb forward tcp:8080 tcp:8080` |
| Forwarding a port on the device to your computer | `adb reverse tcp:9090 tcp:9090` |
| Reading the device log | `adb logcat` |
| Running instrumented tests | `adb shell am instrument -w com.android.example/androidx.test.runner.AndroidJUnitRunner` |

The `adb reverse` example needs a bridge that runs with `--allow-adb-reverse`.

For more information, see
[Interacting with devices using adb](https://docs.aws.amazon.com/devicefarm/latest/developerguide/device-connect-interaction.html)
in the *AWS Device Farm Developer Guide*.

## Device Connect commands

`devicefarm-connect --help` lists the commands, and `devicefarm-connect <command> --help` lists the flags of
a command. For a table of the commands, and for each flag and its default, see
[Device Connect command reference](https://docs.aws.amazon.com/devicefarm/latest/developerguide/device-connect-command-reference.html#device-connect-command-table)
in the *AWS Device Farm Developer Guide*.

## Troubleshooting

A failed command prints `devicefarm-connect: error: <problem>`, then `cause:` and `fix:` lines when it has
them. For more checks, run `devicefarm-connect doctor --session-arn "$SESSION_ARN"` from the shell that starts
the bridge. If the bridge uses another port, add `--port <port>`.

`doctor` checks that Device Connect can properly run on your computer and using the credentials of that shell.
With `--session-arn` or an attached device, it also checks the session, the connection to the device, and the
adb versions. It exits with 0 unless a check fails. A warning is normal for a bridge that runs with
`adb reverse` off.

For more information, see
[Troubleshooting Device Connect](https://docs.aws.amazon.com/devicefarm/latest/developerguide/device-connect-troubleshooting.html).

## Limits

Device Connect has the following limits. For more details, see
[Device Connect limits](https://docs.aws.amazon.com/devicefarm/latest/developerguide/limits.html#device-connect-limits)
in the *AWS Device Farm Developer Guide*.

- Device Connect supports pay-as-you-go per-minute billing. For more information about pricing, see
  [AWS Device Farm pricing](https://aws.amazon.com/device-farm/pricing/).
- You can attach one device at a time. To attach the device of another session, run `device remove` first.
- Device Farm blocks commands that can disrupt the device, such as `adb reboot`. For more information, see
  [Supported platforms and features](https://docs.aws.amazon.com/devicefarm/latest/developerguide/device-connect-support.html).
- `adb forward` and `adb reverse` rules must use a TCP port (`tcp:<port>`) on both your computer and the
  device.
- The bridge limits the number of open adb connections and forward/reverse rules for each device. The adb
  error shows the limit as `(limit <n>)`.
- Device Farm only supports the us-west-2 region.
- A remote access session has a maximum length of 150 minutes. For more information, see
  [Limits in AWS Device Farm](https://docs.aws.amazon.com/devicefarm/latest/developerguide/limits.html).

## Security notes

Like a normal adb server, the bridge does not authenticate adb clients. Any program that any user runs on your
computer can use the attached device if the program reaches the bridge's port. Only your user can attach or
detach a device or stop the bridge. By default, the bridge listens on `127.0.0.1`, a loopback address. Forward
rules always listen on `127.0.0.1`. For more information, see
[Infrastructure security for Device Connect](https://docs.aws.amazon.com/devicefarm/latest/developerguide/infrastructure-security.html#infrastructure-security-device-connect).

The bridge needs one AWS permission, `devicefarm:GetRemoteAccessSession`, and stores no credentials. The
program sends no telemetry. Its outbound traffic is the Device Farm API call, the traffic of your credential
source, and WebSocket connections to the session endpoint over TLS. The API call respects any local
`HTTPS_PROXY` environment variables that you set, but the WebSocket connection to the device does not.

The bridge runs as your user and needs no elevated privileges. It keeps running after you close the terminal
as a background process, but it does not start at login or restart itself.

The bridge creates a local log file which logs certain events that happen on the bridge. The log file contains
device names and session ARNs, which contain your AWS account ID. The bridge never writes any AWS keys or the
signed endpoint URL to the log. If you open an AWS Support case related to an issue with Device Connect, AWS
Support may ask that you include your log file with your request.

## Verify this download

Under the Releases tab in GitHub, for each release, the `SHA256SUMS` file lists the SHA-256 checksums of the
release files. The AWS Device Farm team signs it with an OpenPGP key, and `SHA256SUMS.sig` is the signature.
The list covers each program, the two install scripts, and the source archive
`devicefarm-connect-vX.Y.Z-src.tar.gz`. It does not cover the two **Source code** links that GitHub adds to
every release by itself. To check the source, use the `-src.tar.gz` file. To verify the signature, you need
GnuPG 2.2.8 or later (`gpg` and `gpgv`). To install both:

- Windows: install [Gpg4win](https://www.gpg4win.org/)
- macOS: `brew install gnupg`
- Debian and Ubuntu: `sudo apt-get update && sudo apt-get install gnupg gpgv`
- Amazon Linux 2023: `sudo dnf install --allowerasing gnupg2-full`

On Windows, GnuPG is optional. The Authenticode check at the end of these steps proves who signed the file
without it.

Do the following steps in the directory that holds your release file:

1. Download `SHA256SUMS` and `SHA256SUMS.sig` from the same release as your file. In PowerShell:
   ```powershell
   $download_url_base_path = 'https://github.com/aws/device-connect-devicefarm/releases/download/v1.0.0'
   curl.exe -fsSL --fail-early -O "$download_url_base_path/SHA256SUMS" -O "$download_url_base_path/SHA256SUMS.sig"
   ```
   On macOS or Linux:
   ```sh
   download_url_base_path=https://github.com/aws/device-connect-devicefarm/releases/download/v1.0.0
   curl -fsSL --fail-early -O "$download_url_base_path/SHA256SUMS" -O "$download_url_base_path/SHA256SUMS.sig"
   ```
   If your file is from another release, use the version of that release in place of `v1.0.0`.
2. Get the AWS Device Farm service's public key. The key and its fingerprint are on the AWS documentation page
   [Verify your download](https://docs.aws.amazon.com/devicefarm/latest/developerguide/device-connect-getting-started.html#device-connect-verify-download).
   Copy the key block into a plain text file named `release-key.asc`.
3. Print the key fingerprint, make a keyring file, then check the signature of `SHA256SUMS`. In PowerShell:
   ```powershell
   gpg --show-keys --with-fingerprint release-key.asc
   gpg --yes --output release-key.gpg --dearmor release-key.asc
   gpgv --keyring ./release-key.gpg SHA256SUMS.sig SHA256SUMS
   $LASTEXITCODE
   ```
   On macOS or Linux:
   ```sh
   gpg --show-keys --with-fingerprint release-key.asc
   gpg --dearmor < release-key.asc > release-key.gpg
   LC_ALL=C gpgv --keyring ./release-key.gpg SHA256SUMS.sig SHA256SUMS
   ```
   The fingerprint must match the documentation page and the release notes. `gpg --dearmor` prints nothing.
   `LC_ALL=C` makes `gpgv` print English messages. The `gpgv` output must contain `Good signature from`. In
   PowerShell, `$LASTEXITCODE` must also print `0`. If one of these checks fails, stop. A matching checksum
   proves nothing without a good signature.
4. Check the checksum of your file. Use your file name in the command. In PowerShell:
   ```powershell
   $hash = (Get-FileHash .\devicefarm-connect_windows_amd64.exe -Algorithm SHA256).Hash
   [bool](Select-String -Path SHA256SUMS -Pattern "^$hash  devicefarm-connect_windows_amd64\.exe$" -Quiet)
   ```
   The output must be `True`. On macOS:
   ```sh
   grep ' devicefarm-connect_darwin_universal$' SHA256SUMS | shasum -a 256 -c -
   ```
   On Linux:
   ```sh
   grep ' devicefarm-connect_linux_amd64$' SHA256SUMS | LC_ALL=C sha256sum -c -
   ```
   On macOS and Linux, the output must end with `OK`. On Linux, `LC_ALL=C` makes `sha256sum` print English
   messages.

On Windows, you can also check the Authenticode signature of the file. Amazon signs the Windows file. The
following command prints the signature:

```powershell
Get-AuthenticodeSignature .\devicefarm-connect_windows_amd64.exe | Format-List Status, StatusMessage, SignerCertificate, TimeStamperCertificate
```

`Status` must be `Valid`. The `SignerCertificate` subject must name `Amazon Web Services, Inc.` as the
organization (`O`) and `AWS Dev Tools` as the organizational unit (`OU`). `TimeStamperCertificate` must not
be empty. If one of these checks fails, stop. If Windows names a publisher before the first run, it must be
Amazon Web Services, Inc. A new release can show a SmartScreen notice until Windows has seen the file often.
Verify the file first. Then choose **More info**, then **Run anyway**.

On macOS, you can also check the Apple signature of the file. Amazon signs the file with its Apple Developer
ID, and Apple notarizes it. The following commands check the signature and the notarization:

```sh
codesign -dv --verbose=4 devicefarm-connect_darwin_universal
codesign --verify --verbose=2 -R="notarized" --check-notarization devicefarm-connect_darwin_universal
```

`codesign -dv` prints the signer: the first `Authority=` line is
`Developer ID Application: AMZN Mobile LLC (94KV3E626L)`, and the `TeamIdentifier=` line is `94KV3E626L`.
`codesign --verify` checks the notarization online. The output must end with `explicit requirement satisfied`.
macOS checks the notarization of a browser download online before its first run. A `curl` download has no
quarantine attribute, so it runs at once.

## Build from source

There are two ways to build from source: with Docker (recommended), or with Go on your computer. Run the
commands in a clone or in the unpacked release file `devicefarm-connect-vX.Y.Z-src.tar.gz`.

**Docker (recommended).** Use the [Dockerfile](Dockerfile) to rebuild the release binaries with the release
build flags and the Go version that it names. The build needs BuildKit. Run the following command:

```sh
docker build --pull --output "type=local,dest=dist" .
```

In `dist/`, the build writes the release binaries and the Apple silicon and Intel halves of the macOS binary.
To compare a rebuilt file with a release file, build the source of that release, then run `go version -m` on
both. Both outputs show the same module line and build settings. The two files are the same program, but
Device Farm does not promise a byte-for-byte match at this time.

**Go.** You need Go 1.26 or later. `go install` writes the program to the `bin` directory of Go. The
following commands build, test, and install the program:

```sh
go build ./cmd/devicefarm-connect
go test ./...
go install ./cmd/devicefarm-connect
```

## Report issues and contribute

Report bugs in [GitHub Issues](https://github.com/aws/device-connect-devicefarm/issues). Give your operating
system and the output of `devicefarm-connect --version`, `adb version`, and `devicefarm-connect doctor`.
Remove session ARNs and account IDs from that output and the log first. To contribute, see
[CONTRIBUTING.md](CONTRIBUTING.md). A maintainer imports a pull request instead of merging it on GitHub.
[CHANGELOG.md](CHANGELOG.md) lists the changes.

## Report a security issue

Follow [SECURITY.md](SECURITY.md). Do not open a public issue.

## License

This project is licensed under the Apache License 2.0. See [LICENSE](LICENSE), [NOTICE](NOTICE), and
[THIRD-PARTY-LICENSES](THIRD-PARTY-LICENSES).
