// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

// Package report renders what a command prints: a wrapped ASCII table sized to the terminal,
// the width to size it to, and one compact JSON line. No cobra, no domain words.
package report

import (
	"encoding/json"
	"io"
	"os"
	"strconv"

	"golang.org/x/term"
)

// defaultWidth is the table width when stdout is not a terminal and COLUMNS is unset.
const defaultWidth = 80

// TerminalWidth is the table width for stdout: a positive terminal size when stdout is a
// terminal, else COLUMNS when it is a positive integer, else defaultWidth. Render clamps it.
func TerminalWidth(stdout io.Writer) int {
	if file, isFile := stdout.(*os.File); isFile && term.IsTerminal(int(file.Fd())) {
		if width, _, err := term.GetSize(int(file.Fd())); err == nil && width > 0 {
			return width
		}
	}
	if columns, err := strconv.Atoi(os.Getenv("COLUMNS")); err == nil && columns > 0 {
		return columns
	}
	return defaultWidth
}

// WriteJSON writes value as one compact JSON line. Not HTML: && and <n> stay literal.
func WriteJSON(writer io.Writer, value any) error {
	encoder := json.NewEncoder(writer)
	encoder.SetEscapeHTML(false)
	return encoder.Encode(value)
}

// NonNil makes a nil list encode as [], not null.
func NonNil[Item any](items []Item) []Item {
	if items == nil {
		return []Item{}
	}
	return items
}
