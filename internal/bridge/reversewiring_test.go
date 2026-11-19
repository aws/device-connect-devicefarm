package bridge

import (
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/aws/device-connect-devicefarm/internal/adbwire"
	"github.com/aws/device-connect-devicefarm/internal/testsupport"
)

// tooManyReverseRulesFail is the framed FAIL a client reads at testMaxReverseRules.
const tooManyReverseRulesFail = "FAIL0020too many reverse rules (limit 2)"

// serverRefusal is the framed FAIL the reverse mock refuses an install with.
const serverRefusal = "FAIL0008bad port"

// reverseMockBehavior is how the reverse mock answers installs: a device spec in refuse gets
// serverRefusal; the spec in split gets its two OKAYs in two messages, cut inside the first word;
// the spec in silent gets no answer at all; refuseKillAll turns every killforward-all away.
type reverseMockBehavior struct {
	refuse        []string
	split         string
	silent        string
	refuseKillAll bool
}

// answer plays the server: OKAY to the daemon's select, two OKAYs to an install or a kill, OKAY and
// an empty table to a listing, an echo for everything else.
func (behavior reverseMockBehavior) answer(session *testsupport.MockSession, data []byte) []byte {
	request := string(data)
	if request == canonicalSelect {
		return []byte("OKAY")
	}
	_, command, isReverse := strings.Cut(request, "reverse:")
	if !isReverse {
		return data
	}
	parsed, isForwardFamily, parseErr := adbwire.ParseForwardService(command)
	switch {
	case !isForwardFamily || parseErr != nil:
		return []byte(serverRefusal)
	case parsed.Action == adbwire.AdbActionListForward:
		return []byte("OKAY0000")
	case parsed.Action == adbwire.AdbActionKillForwardAll && behavior.refuseKillAll:
		return []byte(serverRefusal)
	case parsed.Action == adbwire.AdbActionForward && slices.Contains(behavior.refuse, parsed.Local):
		return []byte(serverRefusal)
	case parsed.Action == adbwire.AdbActionForward && parsed.Local == behavior.split:
		session.Send([]byte("OK"))
		return []byte("AYOKAY")
	case parsed.Action == adbwire.AdbActionForward && parsed.Local == behavior.silent:
		return nil
	default:
		return []byte("OKAYOKAY")
	}
}

// reverseProxy is a proxy with attachment features in front of a mock playing the server.
type reverseProxy struct {
	address string
	mock    *testsupport.SessionMockWSS
	device  *ActiveDevice
}

func startReverseProxy(t *testing.T, behavior reverseMockBehavior) *reverseProxy {
	t.Helper()
	mock := testsupport.NewSessionMockWSS(t, testsupport.SessionMockConfig{OnMessage: behavior.answer})
	device := newForwardingDevice(t)
	require.True(t, device.Set(DeviceInfo{ARN: "arn:test", Endpoint: mock.URL, Label: "Test Device - OS 1"}))
	address, _ := runProxy(t, device, testPolicy())
	return &reverseProxy{address: address, mock: mock, device: device}
}

// command sends one reverse-family service as a modern adb client does, a select first.
func (proxy *reverseProxy) command(t *testing.T, service string) string {
	t.Helper()
	return proxy.exchange(t, service, true)
}

// loneCommand is command with no select first, as a raw client may.
func (proxy *reverseProxy) loneCommand(t *testing.T, service string) string {
	t.Helper()
	return proxy.exchange(t, service, false)
}

// exchange sends the service, behind a select when selectFirst, and returns the reply: both OKAYs,
// or the FAIL with its reason. The client then closes, as adb does, and the call returns once the
// connection's budget slot is back.
func (proxy *reverseProxy) exchange(t *testing.T, service string, selectFirst bool) string {
	t.Helper()
	budget := attachedFeatures(t, proxy.device).budget
	client := dialClient(t, proxy.address)
	if selectFirst {
		selectAnyTransport(t, client)
	}
	writeAll(t, client, framed(service))
	reply := string(readN(t, client, statusWordLen))
	if reply == statusFail {
		digits := string(readN(t, client, failLenDigits))
		length, err := strconv.ParseUint(digits, 16, 16)
		require.NoError(t, err)
		reply += digits + string(readN(t, client, int(length)))
	} else {
		reply += string(readN(t, client, statusWordLen))
	}
	require.NoError(t, client.Close())
	waitFor(t, func() bool { return budget.inUse.Load() == 0 }, "the ended command never freed its slot")
	return reply
}

// abandonInstall sends an install the mock never answers, closes the client once the request
// reaches it, as a user who gives up does, and waits for the connection to end.
func (proxy *reverseProxy) abandonInstall(t *testing.T, service string) {
	t.Helper()
	budget := attachedFeatures(t, proxy.device).budget
	client := dialClient(t, proxy.address)
	selectAnyTransport(t, client)
	writeAll(t, client, framed(service))
	waitFor(t, func() bool { return strings.Contains(string(soleSessionInbound(proxy.mock)), service) },
		"the install never reached the server")
	require.NoError(t, client.Close())
	waitFor(t, func() bool { return budget.inUse.Load() == 0 }, "the abandoned connection never ended")
}

// fill installs testMaxReverseRules distinct device specs, each confirmed by the mock.
func (proxy *reverseProxy) fill(t *testing.T) {
	t.Helper()
	for index := range testMaxReverseRules {
		require.Equal(t, "OKAYOKAY", proxy.command(t, reverseInstall(index+1)))
	}
}

// reverseInstall spells `adb reverse tcp:<port> tcp:9000` as the service behind the select.
func reverseInstall(port int) string { return fmt.Sprintf("reverse:forward:tcp:%d;tcp:9000", port) }

func TestGivenAnInstallThroughTheSeamWhenAnOpenIsScreenedThenOnlyThePortItNamedIsAdmitted(t *testing.T) {
	// The seam feeds the table and the attachment hands it to the supervisor; a daemon port is set so
	// the screen reaches the rule check.
	params := forwardingParams()
	params.ListenPort = notifierListenPort
	mock := testsupport.NewSessionMockWSS(t, testsupport.SessionMockConfig{OnMessage: reverseMockBehavior{}.answer})
	device := newAttachedDevice(t, params)
	require.True(t, device.Set(DeviceInfo{ARN: "arn:test", Endpoint: mock.URL, Label: "Test Device - OS 1"}))
	address, _ := runProxy(t, device, testPolicy())
	proxy := &reverseProxy{address: address, mock: mock, device: device}
	require.Equal(t, "OKAYOKAY", proxy.command(t, "reverse:forward:tcp:1;tcp:9001"))

	supervisor := attachedFeatures(t, device).notifier

	assert.Empty(t, supervisor.refuseCustomerPort(9001), "the port the seam read is admitted")
	assert.Equal(t, "no reverse rule names it", supervisor.refuseCustomerPort(9002))
}

func TestGivenTheReverseRuleLimitWhenOneMoreNewSpecIsInstalledThenTheLimitFailAndNoDial(t *testing.T) {
	proxy := startReverseProxy(t, reverseMockBehavior{})
	proxy.fill(t)
	dials := proxy.mock.ConnectionsAccepted()

	reply := proxy.command(t, reverseInstall(testMaxReverseRules+1))

	// The literal, not the constant: adb prints this after "error: ".
	assert.Equal(t, tooManyReverseRulesFail, reply)
	assert.Equal(t, dials, proxy.mock.ConnectionsAccepted(), "a refused install must not dial")
}

func TestGivenAHostPrefixedInstallWithNoSelectWhenScreenedThenItCountsAsTheBareFormDoes(t *testing.T) {
	// adb selects a transport before any reverse request; a raw client can name the device in a
	// host prefix instead, and the screen must read that as the reverse-off refusal does. The mock
	// OKAYs the prefixed form the real server refuses, so the hold shows as a slot taken.
	proxy := startReverseProxy(t, reverseMockBehavior{})
	for port := 1; port < testMaxReverseRules; port++ {
		require.Equal(t, "OKAYOKAY", proxy.command(t, reverseInstall(port)))
	}
	require.Equal(t, "OKAYOKAY", proxy.loneCommand(t, "host-serial:emulator-5554:"+reverseInstall(testMaxReverseRules)),
		"a prefixed install below the limit is relayed")
	dials := proxy.mock.ConnectionsAccepted()

	bareReply := proxy.command(t, reverseInstall(testMaxReverseRules+1))
	prefixedReply := proxy.loneCommand(t, "host:"+reverseInstall(testMaxReverseRules+2))

	assert.Equal(t, tooManyReverseRulesFail, bareReply, "the prefixed install took the last slot")
	assert.Equal(t, tooManyReverseRulesFail, prefixedReply, "a prefixed install is screened at the limit")
	assert.Equal(t, dials, proxy.mock.ConnectionsAccepted(), "a refused install must not dial")
}

func TestGivenAFullReverseTableWhenASpecIsReboundThenKilledThenExactlyOneSlotFrees(t *testing.T) {
	proxy := startReverseProxy(t, reverseMockBehavior{})
	proxy.fill(t)

	require.Equal(t, "OKAYOKAY", proxy.command(t, "reverse:forward:tcp:1;tcp:9001"), "a rebind is relayed")
	require.Equal(t, tooManyReverseRulesFail, proxy.command(t, reverseInstall(3)), "a rebind is not a new rule")
	require.Equal(t, "OKAYOKAY", proxy.command(t, "reverse:killforward:tcp:1"))

	assert.Equal(t, "OKAYOKAY", proxy.command(t, reverseInstall(3)), "the rebound spec held one slot")
	assert.Equal(t, tooManyReverseRulesFail, proxy.command(t, reverseInstall(4)), "one removal frees one slot")
}

func TestGivenAFullReverseTableWhenKillforwardAllIsRelayedThenTheLimitInstallsAgain(t *testing.T) {
	proxy := startReverseProxy(t, reverseMockBehavior{})
	proxy.fill(t)

	require.Equal(t, "OKAYOKAY", proxy.command(t, "reverse:killforward-all"))

	proxy.fill(t)
	assert.Equal(t, tooManyReverseRulesFail, proxy.command(t, reverseInstall(testMaxReverseRules+1)))
}

func TestGivenAnInstallTheServerRefusesWhenRelayedThenTheClientReadsThatFailAndNoSlotIsSpent(t *testing.T) {
	proxy := startReverseProxy(t, reverseMockBehavior{refuse: []string{"tcp:13"}})
	require.Equal(t, "OKAYOKAY", proxy.command(t, reverseInstall(1)))

	assert.Equal(t, serverRefusal, proxy.command(t, "reverse:forward:tcp:13;tcp:9000"), "the server's own FAIL reaches the client")

	assert.Equal(t, "OKAYOKAY", proxy.command(t, reverseInstall(2)), "a refused install spends no slot")
	assert.Equal(t, tooManyReverseRulesFail, proxy.command(t, reverseInstall(3)))
}

func TestGivenRoomForOneRuleWhenListForwardIsRelayedThenItCountsNothing(t *testing.T) {
	proxy := startReverseProxy(t, reverseMockBehavior{})
	require.Equal(t, "OKAYOKAY", proxy.command(t, reverseInstall(1)))

	assert.Equal(t, "OKAY0000", proxy.command(t, "reverse:list-forward"), "the server answers the listing")

	assert.Equal(t, "OKAYOKAY", proxy.command(t, reverseInstall(2)), "a listing takes no slot")
}

func TestGivenAStatusWordSplitAcrossMessagesWhenRelayedThenTheClientReadsItWholeAndTheRuleCountsOnce(t *testing.T) {
	proxy := startReverseProxy(t, reverseMockBehavior{split: "tcp:1"})

	require.Equal(t, "OKAYOKAY", proxy.command(t, reverseInstall(1)))

	require.Equal(t, "OKAYOKAY", proxy.command(t, reverseInstall(2)))
	assert.Equal(t, tooManyReverseRulesFail, proxy.command(t, reverseInstall(3)), "the split install holds one slot")
}

func TestGivenAnInstallAbandonedBeforeTheServerAnswersWhenTheClientClosesThenItCountsUntilRemoved(t *testing.T) {
	// The server finishes an install the client walked out on, so the daemon must count it.
	proxy := startReverseProxy(t, reverseMockBehavior{silent: "tcp:9"})

	proxy.abandonInstall(t, reverseInstall(9))

	require.Equal(t, "OKAYOKAY", proxy.command(t, reverseInstall(1)))
	assert.Equal(t, tooManyReverseRulesFail, proxy.command(t, reverseInstall(2)), "an unanswered install counts as installed")
	require.Equal(t, "OKAYOKAY", proxy.command(t, "reverse:killforward:tcp:9"))
	assert.Equal(t, "OKAYOKAY", proxy.command(t, reverseInstall(2)), "its removal frees the slot")
}

func TestGivenAFullReverseTableWhenTheDeviceIsReattachedThenTheNewAttachmentStartsEmpty(t *testing.T) {
	proxy := startReverseProxy(t, reverseMockBehavior{})
	proxy.fill(t)
	_, cleared := proxy.device.Clear()
	require.True(t, cleared)
	require.True(t, proxy.device.Set(DeviceInfo{ARN: "arn:test", Endpoint: proxy.mock.URL}))

	assert.Equal(t, "OKAYOKAY", proxy.command(t, reverseInstall(testMaxReverseRules+1)), "a table is one attachment's")
}

func TestGivenRepeatedReverseRefusalsWhenLoggedThenOneWarnNamesTheLimit(t *testing.T) {
	// The seam is asked directly, so the log is read without a proxy in front of it.
	_, forwarding := forwardingFor(t)
	for index := range testMaxReverseRules {
		_, full := forwarding.reverseRules.begin(adbwire.AdbActionForward, tcpSpec(index), 0)
		require.False(t, full)
	}
	logs := &strings.Builder{}
	logger := slog.New(slog.NewTextHandler(logs, nil))
	clientOpening := opening{service: reverseInstall(9), selected: true}

	for range 2 {
		client := newScriptedConn("")
		request, refused := screenReverseRule(client, clientOpening, forwarding, logger)
		assert.True(t, refused)
		assert.Nil(t, request)
		assert.Equal(t, tooManyReverseRulesFail, client.replies.String(), "every refusal gets the FAIL")
	}

	warn := `level=WARN msg="refused a reverse rule: the attachment holds its limit"`
	assert.Equal(t, 1, strings.Count(logs.String(), warn), "refusals inside one interval must log once")
	assert.Contains(t, logs.String(), "maxReverseRules=2", "the Warn must name the limit")
}
