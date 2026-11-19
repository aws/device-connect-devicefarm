//go:build !unix

// This stub exists so the module compiles on non-unix platforms (notably
// Windows). The proxy's runtime is currently Unix-only: the daemon lifecycle
// depends on Unix signals (SIGTERM/SIGINT), process-existence probing via
// signal 0, setsid-based detachment, and a Unix-domain control socket, none of
// which have a Windows equivalent yet.
//
// Enabling Windows is gated on the control-socket piece in particular: the CLI
// talks to the running daemon over a Unix-domain socket (internal/control), and
// the daemon binds it before announcing readiness. A Windows port needs a
// portable replacement for that channel (a named pipe, or a loopback TCP control
// port) plus a Windows-native detach + stop mechanism (process-creation flags or
// the Service Control Manager) to replace setsid and SIGTERM. Once the control
// channel is portable, add windows/amd64 to the cross-compile matrix and gate the
// daemon/cli/control packages on that transport rather than //go:build unix.
//
// Building still succeeds so cross-platform CI and `go build ./...` stay green;
// only running is gated.
package main

import (
	"fmt"
	"os"
	"runtime"

	"github.com/aws/device-connect-devicefarm/internal/config"
)

func main() {
	fmt.Fprintf(os.Stderr, "%s is not yet supported on %s; it currently runs on macOS and Linux only\n", config.BinaryName, runtime.GOOS)
	os.Exit(1)
}
