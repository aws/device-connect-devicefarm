package main

import (
	"os"

	"github.com/aws/device-connect-devicefarm/internal/cli"
)

func main() {
	os.Exit(cli.Execute())
}
