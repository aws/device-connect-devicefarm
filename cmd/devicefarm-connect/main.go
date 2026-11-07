// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"

	"github.com/aws/device-connect-devicefarm/internal/cli"
)

func main() {
	os.Exit(cli.Execute())
}
