//go:build unix

package main

import (
	"log/slog"
	"os"

	"github.com/aws/device-connect-devicefarm/internal/cli"
	"github.com/aws/device-connect-devicefarm/internal/config"
)

func main() {
	if err := cli.Execute(); err != nil {
		slog.Error(config.BinaryName+" failed", "error", err)
		os.Exit(1)
	}
}
