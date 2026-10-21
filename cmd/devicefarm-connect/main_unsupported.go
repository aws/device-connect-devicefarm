//go:build !unix

// This stub lets the module compile on non-Unix platforms, Windows included.
// The real entrypoint and the CLI package still carry a Unix-only build tag.
// To enable Windows: remove those tags, delete this stub, and add
// windows/amd64 to the cross-compile list.
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
