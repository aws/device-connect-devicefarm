//go:build !unix

// This stub lets this command build on non-unix targets (notably Windows).
// The runtime is Unix-only: the pidfile uses flock and a signal-0 liveness
// probe and has no Windows body, so internal/daemon does not compile for
// GOOS=windows and `go build ./...` fails there. internal/cli and main.go stay
// //go:build unix. To enable Windows: add a Windows pidfile, remove the unix
// tags from internal/cli and main.go, and add windows/amd64 to the
// cross-compile matrix.
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
