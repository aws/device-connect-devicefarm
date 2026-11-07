// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package report

import (
	"bytes"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/term"
)

func TestGivenAFileThatIsNotATerminalWhenTheWidthIsMeasuredThenColumnsDecides(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "stdout")
	require.NoError(t, err)
	defer file.Close()

	t.Setenv("COLUMNS", "120")
	assert.Equal(t, 120, TerminalWidth(file))
	t.Setenv("COLUMNS", "-5")
	assert.Equal(t, defaultWidth, TerminalWidth(file))
	t.Setenv("COLUMNS", "")
	assert.Equal(t, defaultWidth, TerminalWidth(file))
	t.Setenv("COLUMNS", "120")
	assert.Equal(t, 120, TerminalWidth(&bytes.Buffer{}), "a writer that is no file measures nothing")
}

func TestGivenATerminalThatReportsNoSizeWhenTheWidthIsMeasuredThenColumnsDecides(t *testing.T) {
	ptyPrimary, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		t.Skip("no pseudo-terminal on this host:", err)
	}
	defer ptyPrimary.Close()
	if !term.IsTerminal(int(ptyPrimary.Fd())) {
		t.Skip("the pseudo-terminal primary is not a terminal on this host")
	}
	if width, _, sizeErr := term.GetSize(int(ptyPrimary.Fd())); sizeErr == nil && width > 0 {
		t.Skip("this pseudo-terminal reports a size:", width)
	}

	t.Setenv("COLUMNS", "100")
	assert.Equal(t, 100, TerminalWidth(ptyPrimary))
}

func TestGivenAHintWithShellOperatorsWhenWrittenAsJSONThenOneLineKeepsThemLiteralAndOnlyANilListBecomesEmpty(t *testing.T) {
	var out bytes.Buffer
	value := struct {
		Hint   string   `json:"hint"`
		Needs  []string `json:"needs"`
		Checks []string `json:"checks"`
	}{Hint: "run 'bridge stop && bridge start --port <n>'", Needs: NonNil[string](nil), Checks: NonNil([]string{"local.daemon"})}

	require.NoError(t, WriteJSON(&out, value))

	assert.Equal(t, `{"hint":"run 'bridge stop && bridge start --port <n>'","needs":[],"checks":["local.daemon"]}`+"\n", out.String())
}
