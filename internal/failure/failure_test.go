// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package failure

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func render(err error) string {
	var out bytes.Buffer
	Write(&out, "prog", err)
	return out.String()
}

func TestGivenPlainErrorWhenWrittenThenOneLineWithTheProgramPrefix(t *testing.T) {
	assert.Equal(t, "prog: error: unknown command \"bogus\"\n", render(errors.New(`unknown command "bogus"`)))
}

func TestGivenPlainWrappedChainWhenWrittenThenOneLine(t *testing.T) {
	err := fmt.Errorf("create runtime dir \"/x/d\": %w", fmt.Errorf("mkdir /x/d: %w", fs.ErrNotExist))

	assert.Equal(t, "prog: error: create runtime dir \"/x/d\": mkdir /x/d: file does not exist\n", render(err))
}

func TestGivenExplainedWithCauseAndFixWhenWrittenThenProblemCauseFixLines(t *testing.T) {
	err := &Explained{
		Problem: "could not start the bridge daemon",
		Cause:   WithFix(errors.New("timed out after 5s waiting for pidfile \"/run/d.pid\""), "read the daemon log: /run/d.log"),
	}

	assert.Equal(t,
		"prog: error: could not start the bridge daemon\n"+
			"  cause: timed out after 5s waiting for pidfile \"/run/d.pid\"\n"+
			"  fix:   read the daemon log: /run/d.log\n",
		render(err))
}

func TestGivenExplainedOverAPlainChainWhenWrittenThenTheChainIsOneCauseLine(t *testing.T) {
	err := &Explained{
		Problem: "cannot use the working directory",
		Cause:   fmt.Errorf("create runtime dir \"/x/d\": %w", fmt.Errorf("mkdir /x/d: %w", errors.New("not a directory"))),
	}

	assert.Equal(t,
		"prog: error: cannot use the working directory\n"+
			"  cause: create runtime dir \"/x/d\": mkdir /x/d: not a directory\n",
		render(err))
}

func TestGivenPlainWrapOverAnExplainedWhenWrittenThenSplitAtTheExplained(t *testing.T) {
	err := fmt.Errorf("invalid configuration: %w", WithFix(errors.New("--port must be from 1 to 65535, got 0"), "set --port"))

	assert.Equal(t,
		"prog: error: invalid configuration\n"+
			"  cause: --port must be from 1 to 65535, got 0\n"+
			"  fix:   set --port\n",
		render(err))
}

func TestGivenFixesAtTwoLevelsWhenWrittenThenInnermostFixFirst(t *testing.T) {
	err := &Explained{
		Problem: "the daemon started but could not attach the device",
		Cause:   WithFix(errors.New("session not RUNNING"), "start the session"),
		Fix:     "retry 'device add'",
	}

	assert.Equal(t,
		"prog: error: the daemon started but could not attach the device\n"+
			"  cause: session not RUNNING\n"+
			"  fix:   start the session\n"+
			"  fix:   retry 'device add'\n",
		render(err))
}

func TestGivenWrapThatDoesNotEndWithItsExplainedWhenWrittenThenWholeTextOnceAndTheWalkStops(t *testing.T) {
	// The Explained sits in the middle of the text, so it cannot be cut off the end.
	err := fmt.Errorf("find daemon process 12: %w; stop it from an elevated shell", WithFix(errors.New("permission denied"), "run as root"))

	assert.Equal(t,
		"prog: error: find daemon process 12: permission denied; run as root; stop it from an elevated shell\n",
		render(err))
}

func TestGivenTwoWrappedErrorsWhenWrittenThenOneLine(t *testing.T) {
	err := fmt.Errorf("%w: %w", errors.New("daemon not running"), errors.New("dial unix /run/d.sock: no such file"))

	assert.Equal(t, "prog: error: daemon not running: dial unix /run/d.sock: no such file\n", render(err))
}

func TestGivenCommaJoinedWrapOverAnExplainedWhenWrittenThenTheSeparatorIsCutToo(t *testing.T) {
	err := fmt.Errorf("operation error Device Farm: GetRemoteAccessSession, %w", &Explained{Problem: "not found"})

	assert.Equal(t,
		"prog: error: operation error Device Farm: GetRemoteAccessSession\n"+
			"  cause: not found\n",
		render(err))
}

func TestGivenBareWrapOverAnExplainedWhenWrittenThenNoEmptyLine(t *testing.T) {
	err := fmt.Errorf("%w", &Explained{Problem: "inner"})

	assert.Equal(t, "prog: error: inner\n", render(err))
}

func TestGivenFixAloneWhenWrittenThenTheFlatTextIsTheHeadline(t *testing.T) {
	// No caller builds this; the guard keeps Write from indexing an empty line list.
	assert.Equal(t, "prog: error: run it again\n  fix:   run it again\n", render(&Explained{Fix: "run it again"}))
}

func TestGivenNilErrorWhenWrittenThenNothing(t *testing.T) {
	assert.Empty(t, render(nil))
}

func TestGivenExplainedWhenErrorCalledThenFlatProblemColonCauseSemicolonFix(t *testing.T) {
	cause := errors.New("timed out")

	assert.Equal(t, "could not start: timed out; read the log", (&Explained{Problem: "could not start", Cause: cause, Fix: "read the log"}).Error())
	assert.Equal(t, "timed out; read the log", WithFix(cause, "read the log").Error())
	assert.Equal(t, "could not start", (&Explained{Problem: "could not start"}).Error())
}

func TestGivenExplainedWhenUnwrappedThenErrorsIsReachesTheCause(t *testing.T) {
	err := &Explained{Problem: "could not stop", Cause: WithFix(fmt.Errorf("find process: %w", fs.ErrPermission), "use an elevated shell")}

	require.ErrorIs(t, err, fs.ErrPermission)
}

func TestGivenAChainWithFixesWhenSplitThenTheTextLeavesThemOutAndWithFixRebuildsTheFixLines(t *testing.T) {
	err := fmt.Errorf("resolve session: %w",
		WithFix(WithFix(errors.New("no usable AWS credentials"), "get working credentials"), "run 'bridge start'"))

	cause, fixes := Split(err)

	assert.Equal(t, "resolve session: no usable AWS credentials", cause)
	assert.Equal(t, []string{"get working credentials", "run 'bridge start'"}, fixes)
	rebuilt := errors.New(cause)
	for _, fix := range fixes {
		rebuilt = WithFix(rebuilt, fix)
	}
	assert.Equal(t, "prog: error: could not attach the device\n"+
		"  cause: resolve session: no usable AWS credentials\n"+
		"  fix:   get working credentials\n"+
		"  fix:   run 'bridge start'\n",
		render(&Explained{Problem: "could not attach the device", Cause: rebuilt}))
}

func TestGivenAnErrorWithoutAFixWhenSplitThenItsTextAndNoFix(t *testing.T) {
	cause, fixes := Split(fmt.Errorf("fetch the device serial: %w", errors.New("dial refused")))

	assert.Equal(t, "fetch the device serial: dial refused", cause)
	assert.Empty(t, fixes)
}
