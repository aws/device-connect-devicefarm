// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package forward

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/aws/device-connect-devicefarm/internal/adbwire"
)

// servingOpener stands in for the device-stream opener: it writes the copied
// destination back to the dialer as one line (so a test sees exactly which
// destination the accept path captured) and then echoes bytes until EOF (so a
// test can prove an accepted connection outlives its rule's destination).
type servingOpener struct{}

func (servingOpener) OpenStream(conn net.Conn, target adbwire.AdbSpec, turn OpenTurn) {
	turn.Done()
	defer conn.Close()
	if _, err := fmt.Fprintln(conn, target); err != nil {
		return
	}
	io.Copy(conn, conn) // echo until the dialer hangs up
}

func newTestTable(t *testing.T) *ForwardRulesTable {
	t.Helper()
	table := NewForwardRulesTable(servingOpener{}, testMaxRules, nil)
	t.Cleanup(table.RemoveAll)
	return table
}

// testTimeout bounds every blocking wait so a hung test fails instead of stalling the run.
const testTimeout = 5 * time.Second

// testMaxRules is the table tests' rule limit. The FAIL text pinned below checks the
// format, not the production default.
const testMaxRules = 8

// fillTable installs testMaxRules tcp:0 rules and returns their resolved ports.
func fillTable(t *testing.T, table *ForwardRulesTable) []int {
	t.Helper()
	ports := make([]int, 0, testMaxRules)
	for range testMaxRules {
		ports = append(ports, installResolved(t, table, "tcp:9090"))
	}
	return ports
}

// releasedPort returns an ephemeral loopback port nothing listens on.
func releasedPort(t *testing.T) int {
	t.Helper()
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	require.NoError(t, probe.Close())
	return probe.Addr().(*net.TCPAddr).Port
}

// tcpSpec renders a port as adb spec text.
func tcpSpec(port int) string { return fmt.Sprintf("tcp:%d", port) }

// installResolved installs a tcp:0 rule and returns the resolved local port.
func installResolved(t *testing.T, table *ForwardRulesTable, remote string) int {
	t.Helper()
	result := table.Install("tcp:0", remote, false)
	require.Equal(t, AdbInstallOK, result.Status, "reason %q", result.FailReason)
	require.NotZero(t, result.ResolvedPort, "tcp:0 must report the resolved port")
	return result.ResolvedPort
}

// dialRule connects to a rule's port and returns the destination line the
// opener served plus the still-open connection and its reader.
func dialRule(t *testing.T, port int) (string, net.Conn, *bufio.Reader) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), testTimeout)
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })
	require.NoError(t, conn.SetDeadline(time.Now().Add(testTimeout)))
	reader := bufio.NewReader(conn)
	line, err := reader.ReadString('\n')
	require.NoError(t, err)
	return strings.TrimSuffix(line, "\n"), conn, reader
}

// ruleCount reads the table size under its mutex.
func (table *ForwardRulesTable) ruleCount() int {
	table.rulesMutex.Lock()
	defer table.rulesMutex.Unlock()
	return len(table.rules)
}

func TestGivenLocalSpecTextsWhenCheckedThenOnlyTcpPortsPass(t *testing.T) {
	tests := []struct {
		name       string
		text       string
		spec       adbwire.AdbSpec
		port       int
		failReason string
	}{
		{name: "tcp port", text: "tcp:1234", spec: adbwire.AdbSpec{Kind: "tcp", Value: "1234"}, port: 1234},
		{name: "tcp zero resolves at bind time", text: "tcp:0", spec: adbwire.AdbSpec{Kind: "tcp", Value: "0"}},
		{name: "a socket kind the device end allows", text: "localabstract:sock", failReason: "unknown socket specification:localabstract:sock"},
		{name: "no kind", text: "garbage", failReason: "unknown socket specification:garbage"},
		{name: "port out of range", text: "tcp:70000", failReason: "bad port number '70000'"},
		{name: "port not numeric", text: "tcp:abc", failReason: "listening on specified hostname currently unsupported"},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			spec, port, failReason := parseLocalSpec(testCase.text)

			assert.Equal(t, testCase.spec, spec)
			assert.Equal(t, testCase.port, port)
			assert.Equal(t, testCase.failReason, failReason)
		})
	}
}

func TestGivenRemoteSpecTextsWhenCheckedThenTcpAndTheDeviceSocketKindsPass(t *testing.T) {
	tests := []struct {
		name       string
		text       string
		spec       adbwire.AdbSpec
		failReason string
	}{
		{name: "tcp port", text: "tcp:1234", spec: adbwire.AdbSpec{Kind: "tcp", Value: "1234"}},
		{name: "abstract socket", text: "localabstract:chrome_devtools_remote", spec: adbwire.AdbSpec{Kind: "localabstract", Value: "chrome_devtools_remote"}},
		{name: "reserved socket", text: "localreserved:adbd", spec: adbwire.AdbSpec{Kind: "localreserved", Value: "adbd"}},
		{name: "filesystem socket", text: "localfilesystem:/data/local/tmp/app.sock", spec: adbwire.AdbSpec{Kind: "localfilesystem", Value: "/data/local/tmp/app.sock"}},
		{name: "jdwp pid", text: "jdwp:1234", spec: adbwire.AdbSpec{Kind: "jdwp", Value: "1234"}},
		{name: "empty socket name is adbd's to refuse", text: "localabstract:", spec: adbwire.AdbSpec{Kind: "localabstract"}},
		{name: "character device", text: "dev:/dev/null", failReason: "unknown socket specification:dev:/dev/null"},
		{name: "raw character device", text: "dev-raw:/dev/null", failReason: "unknown socket specification:dev-raw:/dev/null"},
		{name: "vm socket", text: "vsock:1:2", failReason: "unknown socket specification:vsock:1:2"},
		{name: "accept fd", text: "acceptfd:3", failReason: "unknown socket specification:acceptfd:3"},
		{name: "reserved socket under its device alias", text: "local:x", failReason: "unknown socket specification:local:x"},
		{name: "a device service", text: "shell:ls", failReason: "unknown socket specification:shell:ls"},
		{name: "no kind", text: "garbage", failReason: "unknown socket specification:garbage"},
		{name: "port out of range", text: "tcp:70000", failReason: "bad port number '70000'"},
		{name: "port not numeric", text: "tcp:abc", failReason: "listening on specified hostname currently unsupported"},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			spec, failReason := parseRemoteSpec(testCase.text)

			assert.Equal(t, testCase.spec, spec)
			assert.Equal(t, testCase.failReason, failReason)
		})
	}
}

func TestGivenInstalledRuleWhenConnectionAcceptedThenOpenerGetsTheCopiedTarget(t *testing.T) {
	table := newTestTable(t)
	port := installResolved(t, table, "tcp:9090")

	target, _, _ := dialRule(t, port)

	assert.Equal(t, "tcp:9090", target)
}

func TestGivenExistingRuleWhenReboundThenFutureAcceptsUseTheNewDestination(t *testing.T) {
	table := newTestTable(t)
	port := installResolved(t, table, "tcp:9090")
	before, earlier, earlierReader := dialRule(t, port)
	same := table.Install(tcpSpec(port), "tcp:9090", false)

	result := table.Install(tcpSpec(port), "tcp:9091", false)

	require.Equal(t, AdbInstallResult{Status: AdbInstallOK}, same, "re-installing the same rule is a no-op")
	require.Equal(t, AdbInstallResult{Status: AdbInstallOK}, result)
	assert.Equal(t, 1, table.ruleCount())
	after, _, _ := dialRule(t, port)
	assert.Equal(t, "tcp:9090", before)
	assert.Equal(t, "tcp:9091", after)
	// The earlier connection copied its destination and keeps flowing.
	_, err := earlier.Write([]byte("ping\n"))
	require.NoError(t, err)
	echo, err := earlierReader.ReadString('\n')
	require.NoError(t, err)
	assert.Equal(t, "ping\n", echo)
}

func TestGivenExistingRuleWhenNoRebindInstallThenCannotRebind(t *testing.T) {
	table := newTestTable(t)
	port := installResolved(t, table, "tcp:9090")

	result := table.Install(tcpSpec(port), "tcp:9091", true)

	assert.Equal(t, AdbInstallResult{Status: AdbInstallCannotRebind, FailReason: adbwire.MsgCannotRebind}, result)
	target, _, _ := dialRule(t, port)
	assert.Equal(t, "tcp:9090", target, "a refused rebind must leave the destination alone")
}

func TestGivenTCPZeroWhenInstalledThenRuleIsReKeyedOnTheResolvedPort(t *testing.T) {
	table := newTestTable(t)
	first := installResolved(t, table, "tcp:9090")

	// tcp:0 is never a key, so a norebind install of it lands a second rule.
	second := table.Install("tcp:0", "tcp:9092", true)

	assert.Equal(t, AdbInstallOK, second.Status, second.FailReason)
	assert.NotZero(t, second.ResolvedPort)
	assert.Equal(t, 2, table.ruleCount())
	target, _, _ := dialRule(t, first)
	assert.Equal(t, "tcp:9090", target, "the first rule still answers on its resolved port")
}

func TestGivenADeviceSocketRemoteWhenInstalledThenTheOpenerGetsTheSpecVerbatim(t *testing.T) {
	table := newTestTable(t)
	tests := []struct {
		kind   string
		remote string
	}{
		{"localabstract", "localabstract:chrome_devtools_remote"},
		{"localreserved", "localreserved:adbd"},
		{"localfilesystem", "localfilesystem:/data/local/tmp/app.sock"},
		{"jdwp", "jdwp:1234"},
	}

	for _, testCase := range tests {
		t.Run(testCase.kind, func(t *testing.T) {
			port := installResolved(t, table, testCase.remote)

			target, _, _ := dialRule(t, port)

			assert.Equal(t, testCase.remote, target, "the opener must get the spec as the client spelled it")
		})
	}
}

func TestGivenPortAlreadyBoundWhenInstalledThenCannotBindCarriesTheCause(t *testing.T) {
	table := newTestTable(t)
	squatter, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer squatter.Close()
	port := squatter.Addr().(*net.TCPAddr).Port

	result := table.Install(tcpSpec(port), "tcp:9090", false)

	assert.Equal(t, AdbInstallResult{Status: AdbInstallCannotBind, FailReason: "cannot bind listener: Address already in use"}, result)
	assert.Zero(t, table.ruleCount())
}

func TestGivenUnsupportedSpecsWhenInstalledThenFailsClosedWithAdbsSpecTexts(t *testing.T) {
	table := newTestTable(t)
	tests := []struct {
		name   string
		local  string
		remote string
		want   AdbInstallResult
	}{
		{"non-tcp local", "localabstract:sock", "tcp:9090", AdbInstallResult{Status: AdbInstallCannotBind, FailReason: "cannot bind listener: unknown socket specification:localabstract:sock"}},
		{"malformed local", "garbage", "tcp:9090", AdbInstallResult{Status: AdbInstallCannotBind, FailReason: "cannot bind listener: unknown socket specification:garbage"}},
		{"local port out of range", "tcp:70000", "tcp:9090", AdbInstallResult{Status: AdbInstallCannotBind, FailReason: "cannot bind listener: bad port number '70000'"}},
		{"remote kind outside the allowlist", "tcp:0", "dev:/dev/null", AdbInstallResult{Status: AdbInstallUnsupportedRemote, FailReason: "unknown socket specification:dev:/dev/null"}},
		{"malformed remote is not wrapped", "tcp:0", "garbage", AdbInstallResult{Status: AdbInstallUnsupportedRemote, FailReason: "unknown socket specification:garbage"}},
		{"remote port out of range", "tcp:0", "tcp:70000", AdbInstallResult{Status: AdbInstallUnsupportedRemote, FailReason: "bad port number '70000'"}},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			assert.Equal(t, testCase.want, table.Install(testCase.local, testCase.remote, false))
		})
	}
	assert.Zero(t, table.ruleCount(), "failed installs must not leave rules behind")
}

func TestGivenRemovedRuleWhenDialedThenNoNewConnectionIsAccepted(t *testing.T) {
	table := newTestTable(t)
	port := installResolved(t, table, "tcp:9090")

	require.True(t, table.Remove(tcpSpec(port)))

	_, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), testTimeout)
	assert.ErrorIs(t, err, syscall.ECONNREFUSED, "the listener must be closed before Remove returns")
	assert.Empty(t, table.List())
	assert.Zero(t, table.ruleCount(), "a closed listener must not leave its rule behind")
	assert.False(t, table.Remove(tcpSpec(port)), "a second killforward must miss")
}

func TestGivenLiveConnectionWhenItsRuleIsRemovedThenTheStreamKeepsFlowing(t *testing.T) {
	table := newTestTable(t)
	port := installResolved(t, table, "tcp:9090")
	_, conn, reader := dialRule(t, port)

	require.True(t, table.Remove(tcpSpec(port)))

	_, err := conn.Write([]byte("ping\n"))
	require.NoError(t, err)
	echo, err := reader.ReadString('\n')
	require.NoError(t, err)
	assert.Equal(t, "ping\n", echo, "removing a rule closes the door, not the people inside")
}

func TestGivenSeveralRulesWhenListedThenRowsHoldInsertionOrder(t *testing.T) {
	table := newTestTable(t)
	first := installResolved(t, table, "tcp:1111")
	second := installResolved(t, table, "tcp:2222")
	third := installResolved(t, table, "tcp:3333")

	rebind := table.Install(tcpSpec(first), "tcp:4444", false)
	removed := table.Remove(tcpSpec(second))
	fourth := installResolved(t, table, "tcp:5555")

	require.Equal(t, AdbInstallOK, rebind.Status, rebind.FailReason)
	require.True(t, removed)
	// A rebind edits the rule in place, so it keeps its slot; a rule installed
	// after a removal lands last.
	assert.Equal(t, []adbwire.AdbListedForward{
		{Local: tcpSpec(first), Remote: "tcp:4444"},
		{Local: tcpSpec(third), Remote: "tcp:3333"},
		{Local: tcpSpec(fourth), Remote: "tcp:5555"},
	}, table.List())
}

func TestGivenARuleBoundOnAResolvedPortWhenTheTableIsAskedThenOnlyThatPortIsBound(t *testing.T) {
	table := newTestTable(t)
	port := installResolved(t, table, "tcp:9090")

	assert.True(t, table.BindsPort(port), "a rule installed as tcp:0 binds its resolved port")
	assert.False(t, table.BindsPort(0), "no listener answers on port 0")
	assert.False(t, table.BindsPort(9090), "the remote port is dialed, not bound")

	require.True(t, table.Remove(tcpSpec(port)))

	assert.False(t, table.BindsPort(port), "a removed rule frees its port")
}

func TestGivenARuleSpelledWithALeadingZeroWhenTheTableIsAskedThenItsPortIsBound(t *testing.T) {
	table := newTestTable(t)
	// A port the OS just handed out and we released, so the alias spelling can bind it.
	port := releasedPort(t)

	alias := fmt.Sprintf("tcp:0%d", port)
	result := table.Install(alias, "tcp:9090", false)
	require.Equal(t, AdbInstallOK, result.Status, "reason %q", result.FailReason)

	assert.True(t, table.BindsPort(port), "a rule keyed by an alias spelling still binds its port")
	assert.False(t, table.Remove(tcpSpec(port)), "the key keeps the spelling adb sent")
	require.True(t, table.Remove(alias))
	assert.False(t, table.BindsPort(port), "a removed alias rule frees its port")
}

func TestGivenMissingOrUnparseableLocalWhenRemovedThenReportsNotFound(t *testing.T) {
	table := newTestTable(t)
	port := installResolved(t, table, "tcp:9090")
	tests := []struct {
		name  string
		local string
	}{
		{name: "port with no rule", local: "tcp:1"},
		{name: "unparseable spec", local: "garbage"},
		{name: "kind this daemon never installs", local: "localabstract:sock"},
		{name: "the resolved port under another kind", local: fmt.Sprintf("localabstract:%d", port)},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			assert.False(t, table.Remove(testCase.local))
		})
	}
	assert.Len(t, table.List(), 1, "a missed remove must leave the table alone")
}

func TestGivenSeveralRulesWhenRemoveAllThenEveryListenerCloses(t *testing.T) {
	table := newTestTable(t)
	ports := []int{installResolved(t, table, "tcp:1111"), installResolved(t, table, "tcp:2222")}

	table.RemoveAll()

	for _, port := range ports {
		_, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), testTimeout)
		assert.Error(t, err, "port %d still accepting after RemoveAll", port)
	}
	assert.Zero(t, table.ruleCount())
	assert.Empty(t, table.List(), "RemoveAll must drop the list order with the rules")
	// The freed port binds again: killforward-all then forward works.
	assert.Equal(t, AdbInstallOK, table.Install(tcpSpec(ports[0]), "tcp:1111", false).Status)
}

func TestGivenAClosedTableWhenInstalledThenItRefusesAndBindsNothing(t *testing.T) {
	table := newTestTable(t)
	installedPort := installResolved(t, table, "tcp:9090")
	port := releasedPort(t)

	table.Close()

	result := table.Install(tcpSpec(port), "tcp:9090", false)
	assert.Equal(t, AdbInstallResult{Status: AdbInstallCannotBind, FailReason: adbwire.MsgNoDevices}, result)
	for _, checkedPort := range []int{installedPort, port} {
		_, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(checkedPort)), testTimeout)
		assert.ErrorIs(t, err, syscall.ECONNREFUSED, "port %d listens after Close", checkedPort)
	}
	assert.Zero(t, table.ruleCount())
	assert.Empty(t, table.List())
}

func TestGivenAClosedTableWhenInstalledWithAMalformedSpecThenNoDevicesOutranksTheSpecError(t *testing.T) {
	table := newTestTable(t)
	table.Close()
	port := releasedPort(t)
	tests := []struct {
		name   string
		local  string
		remote string
	}{
		{"malformed local", "garbage", "tcp:9090"},
		{"remote kind outside the allowlist", tcpSpec(port), "vsock:1:2"},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			assert.Equal(t, AdbInstallResult{Status: AdbInstallCannotBind, FailReason: adbwire.MsgNoDevices},
				table.Install(testCase.local, testCase.remote, false))
		})
	}
	_, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), testTimeout)
	assert.ErrorIs(t, err, syscall.ECONNREFUSED, "port %d listens after Close", port)
	assert.Zero(t, table.ruleCount())
}

func TestGivenAClosedTableWhenClosedAgainThenItStaysClosed(t *testing.T) {
	table := newTestTable(t)
	table.Close()

	table.Close()

	assert.Equal(t, AdbInstallCannotBind, table.Install("tcp:0", "tcp:9090", false).Status)
}

func TestGivenInstallsRacingACloseWhenTheCloseReturnsThenNoListenerSurvives(t *testing.T) {
	table := newTestTable(t)
	results := make(chan AdbInstallResult, testMaxRules)
	var waitGroup sync.WaitGroup
	for range testMaxRules {
		waitGroup.Go(func() {
			results <- table.Install("tcp:0", "tcp:9090", false)
		})
	}

	table.Close()

	waitGroup.Wait()
	close(results)
	for result := range results {
		if result.Status != AdbInstallOK {
			assert.Equal(t, AdbInstallResult{Status: AdbInstallCannotBind, FailReason: adbwire.MsgNoDevices}, result)
			continue
		}
		_, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(result.ResolvedPort)), testTimeout)
		assert.ErrorIs(t, err, syscall.ECONNREFUSED, "port %d outlived Close", result.ResolvedPort)
	}
	assert.Zero(t, table.ruleCount())
}

func TestGivenAFullTableWhenANewLocalIsInstalledThenTooManyRulesAndNothingBinds(t *testing.T) {
	table := newTestTable(t)
	fillTable(t, table)
	// A squatter holds the port: a bind attempt would report cannot-bind, not the limit.
	squatter, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer squatter.Close()
	port := squatter.Addr().(*net.TCPAddr).Port

	result := table.Install(tcpSpec(port), "tcp:9091", false)

	// The literal, not the constant: adb prints this after "error: ".
	assert.Equal(t, AdbInstallResult{Status: AdbInstallTooManyRules, FailReason: "too many forward rules (limit 8)"}, result)
	assert.Equal(t, testMaxRules, table.ruleCount())
}

func TestGivenConcurrentInstallsAtTheLimitWhenRacedThenOnlyTheLimitInstall(t *testing.T) {
	table := newTestTable(t)
	statuses := make(chan AdbInstallStatus, 2*testMaxRules)
	var wg sync.WaitGroup
	for range 2 * testMaxRules {
		wg.Add(1)
		go func() {
			defer wg.Done()
			statuses <- table.Install("tcp:0", "tcp:9090", false).Status
		}()
	}
	wg.Wait()
	close(statuses)

	installed := 0
	for status := range statuses {
		if status == AdbInstallOK {
			installed++
			continue
		}
		assert.Equal(t, AdbInstallTooManyRules, status)
	}
	assert.Equal(t, testMaxRules, installed, "racing installs must fill the table exactly")
	assert.Equal(t, testMaxRules, table.ruleCount())
}

func TestGivenAFullTableWhenAnExistingLocalIsReboundThenItIsAcceptedButNorebindIsNot(t *testing.T) {
	table := newTestTable(t)
	ports := fillTable(t, table)

	rebind := table.Install(tcpSpec(ports[0]), "tcp:9091", false)
	refused := table.Install(tcpSpec(ports[0]), "tcp:9092", true)

	assert.Equal(t, AdbInstallResult{Status: AdbInstallOK}, rebind, "a rebind is not a new rule")
	// Rebind branch before the count: norebind keeps adb's own refusal.
	assert.Equal(t, AdbInstallResult{Status: AdbInstallCannotRebind, FailReason: adbwire.MsgCannotRebind}, refused)
	target, _, _ := dialRule(t, ports[0])
	assert.Equal(t, "tcp:9091", target)
	assert.Equal(t, testMaxRules, table.ruleCount())
}

func TestGivenAFullTableWhenARuleIsRemovedThenTheSlotIsFree(t *testing.T) {
	table := newTestTable(t)
	ports := fillTable(t, table)
	require.True(t, table.Remove(tcpSpec(ports[0])))

	installResolved(t, table, "tcp:9091")

	assert.Equal(t, testMaxRules, table.ruleCount())
	assert.Equal(t, AdbInstallTooManyRules, table.Install("tcp:0", "tcp:9092", false).Status,
		"one removal frees one slot")
}

func TestGivenAFullTableWhenRemoveAllThenEveryRuleInstallsAgain(t *testing.T) {
	table := newTestTable(t)
	fillTable(t, table)

	table.RemoveAll()

	fillTable(t, table)
	assert.Equal(t, AdbInstallTooManyRules, table.Install("tcp:0", "tcp:9091", false).Status)
}

func TestGivenAFullTableWhenTcpZeroIsInstalledThenItCountsAsANewRule(t *testing.T) {
	table := newTestTable(t)
	fillTable(t, table)

	result := table.Install("tcp:0", "tcp:9091", false)

	assert.Equal(t, AdbInstallTooManyRules, result.Status, "tcp:0 is never a rebind")
	assert.Zero(t, result.ResolvedPort, "a refused tcp:0 resolves no port")
	assert.Len(t, table.List(), testMaxRules)
}

func TestGivenConcurrentInstallsAndRemovesWhenRacedThenTheTableStaysConsistent(t *testing.T) {
	table := newTestTable(t)
	var wg sync.WaitGroup

	// One goroutine per slot: all may hold a rule at once.
	for range testMaxRules {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result := table.Install("tcp:0", "tcp:9090", false)
			if !assert.Equal(t, AdbInstallOK, result.Status, result.FailReason) {
				return
			}
			local := tcpSpec(result.ResolvedPort)
			listed := slices.ContainsFunc(table.List(), func(row adbwire.AdbListedForward) bool {
				return row.Local == local
			})
			assert.True(t, listed, "a concurrent install lost its list slot")
			assert.True(t, table.Remove(local))
		}()
	}
	wg.Wait()

	assert.Empty(t, table.List(), "the race leaked rules")
}

// scriptedListener replays Accept outcomes in order, then reports closed. It
// records when each Accept ran, so a test can measure the retry back-off from
// the loop's own calls instead of from its start.
type scriptedListener struct {
	outcomes    []acceptOutcome
	acceptTimes []time.Time
}

type acceptOutcome struct {
	conn net.Conn
	err  error
}

func (listener *scriptedListener) Accept() (net.Conn, error) {
	listener.acceptTimes = append(listener.acceptTimes, time.Now())
	if len(listener.outcomes) == 0 {
		return nil, net.ErrClosed
	}
	next := listener.outcomes[0]
	listener.outcomes = listener.outcomes[1:]
	return next.conn, next.err
}

func (*scriptedListener) Close() error   { return nil }
func (*scriptedListener) Addr() net.Addr { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)} }

// recordingOpener hands every accepted connection and its turn to the test.
type recordingOpener struct {
	opened chan handoff
}

// handoff is one OpenStream call as the opener got it.
type handoff struct {
	conn net.Conn
	turn OpenTurn
}

func (opener *recordingOpener) OpenStream(conn net.Conn, _ adbwire.AdbSpec, turn OpenTurn) {
	opener.opened <- handoff{conn: conn, turn: turn}
}

// acceptTurn dials the rule's port and returns the turn the accept handed off.
func acceptTurn(t *testing.T, opener *recordingOpener, port int) OpenTurn {
	t.Helper()
	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), testTimeout)
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })
	select {
	case handed := <-opener.opened:
		t.Cleanup(func() { handed.conn.Close() })
		return handed.turn
	case <-time.After(testTimeout):
		t.Fatal("the accepted connection never reached the opener")
		return OpenTurn{}
	}
}

// newRecordingTable builds a table whose accepts reach the returned opener.
func newRecordingTable(t *testing.T) (*ForwardRulesTable, *recordingOpener) {
	t.Helper()
	opener := &recordingOpener{opened: make(chan handoff, 1)}
	table := NewForwardRulesTable(opener, testMaxRules, nil)
	t.Cleanup(table.RemoveAll)
	return table, opener
}

func TestGivenTwoAcceptsOnOneRuleWhenHandedOffThenTheSecondTurnWaitsForTheFirst(t *testing.T) {
	table, opener := newRecordingTable(t)
	port := installResolved(t, table, "localabstract:scrcpy")

	first := acceptTurn(t, opener, port)
	second := acceptTurn(t, opener, port)

	assert.True(t, isReady(first.Ready), "a rule's first accept must not wait")
	assert.False(t, isReady(second.Ready), "the second accept must wait for the first")
	first.Done()
	assert.True(t, isReady(second.Ready), "the first accept's Done must release the second")
}

func TestGivenTwoAcceptsOnATcpRuleWhenHandedOffThenNeitherWaits(t *testing.T) {
	// A turn would add head-of-line blocking, and the servers known to need accept order
	// listen on abstract sockets.
	table, opener := newRecordingTable(t)
	port := installResolved(t, table, "tcp:9090")

	acceptTurn(t, opener, port)
	second := acceptTurn(t, opener, port)

	assert.True(t, isReady(second.Ready), "a tcp rule's accept must never wait")
}

func TestGivenATcpRuleReboundToASocketKindWhenTwoAcceptsArriveThenOnlyTheSecondWaits(t *testing.T) {
	table, opener := newRecordingTable(t)
	port := installResolved(t, table, "tcp:9090")
	acceptTurn(t, opener, port)
	require.Equal(t, AdbInstallOK, table.Install(tcpSpec(port), "localabstract:scrcpy", false).Status)

	first := acceptTurn(t, opener, port)
	second := acceptTurn(t, opener, port)

	assert.True(t, isReady(first.Ready), "the rebind's first socket accept must not wait for a tcp accept")
	assert.False(t, isReady(second.Ready), "the rebound rule must order its socket accepts")
}

func TestGivenAnAcceptErrorThatIsNotACloseWhenServingThenTheLoopRetries(t *testing.T) {
	server, client := net.Pipe()
	defer client.Close()
	defer server.Close()
	listener := &scriptedListener{outcomes: []acceptOutcome{
		{err: errors.New("accept: too many open files")},
		{conn: server},
	}}
	opener := &recordingOpener{opened: make(chan handoff, 1)}
	table := NewForwardRulesTable(opener, testMaxRules, slog.New(slog.NewTextHandler(io.Discard, nil)))

	returned := make(chan struct{})
	go func() {
		defer close(returned)
		table.acceptLoop(&rule{local: adbwire.AdbSpec{Kind: "tcp", Value: "1"}, remote: adbwire.AdbSpec{Kind: "tcp", Value: "2"}, listener: listener})
	}()

	select {
	case handed := <-opener.opened:
		assert.Same(t, server, handed.conn)
	case <-time.After(testTimeout):
		t.Fatal("the connection accepted after the transient error never reached the opener")
	}
	select {
	case <-returned:
	case <-time.After(testTimeout):
		t.Fatal("the loop kept running after the listener reported closed")
	}

	require.Len(t, listener.acceptTimes, 3, "the loop must accept again after the error, then read the close")
	assert.GreaterOrEqual(t, listener.acceptTimes[1].Sub(listener.acceptTimes[0]), acceptRetryFirstDelay,
		"the retry must wait one back-off step")
}
