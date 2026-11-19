package bridge

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/aws/device-connect-devicefarm/internal/adbwire"
)

func TestGivenServicesWhenParsedAsReverseRequestsThenOnlyInstallsAndKillsCount(t *testing.T) {
	tests := []struct {
		service      string
		action       adbwire.AdbForwardAction
		deviceSpec   string
		customerPort int
		counted      bool
	}{
		{service: "reverse:forward:tcp:1;tcp:2", action: adbwire.AdbActionForward, deviceSpec: "tcp:1", customerPort: 2, counted: true},
		{service: "reverse:forward:norebind:tcp:1;tcp:2", action: adbwire.AdbActionForward, deviceSpec: "tcp:1", customerPort: 2, counted: true},
		{service: "reverse:forward:tcp:1;localabstract:app", action: adbwire.AdbActionForward, deviceSpec: "tcp:1", counted: true},
		{service: "reverse:forward:tcp:1;tcp:localhost:2", action: adbwire.AdbActionForward, deviceSpec: "tcp:1", counted: true},
		{service: "reverse:killforward:tcp:1", action: adbwire.AdbActionKillForward, deviceSpec: "tcp:1", counted: true},
		{service: "reverse:killforward-all", action: adbwire.AdbActionKillForwardAll, counted: true},
		{service: "reverse:list-forward"},
		{service: "reverse:forward:tcp:1"},
		{service: "host:forward:tcp:1;tcp:2"},
	}
	for _, test := range tests {
		t.Run(test.service, func(t *testing.T) {
			action, deviceSpec, customerPort, counted := parseReverseRequest(test.service)

			assert.Equal(t, test.counted, counted)
			assert.Equal(t, test.action, action)
			assert.Equal(t, test.deviceSpec, deviceSpec)
			assert.Equal(t, test.customerPort, customerPort)
		})
	}
}

func TestGivenARuleWithANonTcpCustomerSpecWhenCountedThenItHoldsNoPort(t *testing.T) {
	rules := newReverseRules(testMaxReverseRules)
	_, deviceSpec, customerPort, counted := parseReverseRequest("reverse:forward:tcp:1;localabstract:app")
	require.True(t, counted)
	install, full := rules.begin(adbwire.AdbActionForward, deviceSpec, customerPort)
	require.False(t, full)

	install.settle(true)

	assert.True(t, rules.holdsAny(), "the rule counts against the limit")
	assert.Zero(t, rules.rules["tcp:1"].customerPort, "a spec the server would never announce records no port")
}

func TestGivenAnInstalledRuleWhenARebindFailsThenTheConfirmedPortStays(t *testing.T) {
	rules := installedReverseRules(9001)
	rebind, _ := rules.begin(adbwire.AdbActionForward, "tcp:1", 9002)
	require.True(t, rules.allowsCustomerPort(9002), "a rebind awaiting its verdict names its port")

	rebind.settle(false)

	assert.True(t, rules.allowsCustomerPort(9001), "the confirmed port stays")
	assert.False(t, rules.allowsCustomerPort(9002), "a refused rebind names no port")
}

func TestGivenAnInstalledRuleWhenKillforwardIsConfirmedThenOnlyAPendingInstallStillNamesAPort(t *testing.T) {
	rules := installedReverseRules(9001)
	pending, _ := rules.begin(adbwire.AdbActionForward, "tcp:1", 9002)
	kill, _ := rules.begin(adbwire.AdbActionKillForward, "tcp:1", 0)

	kill.settle(true)

	assert.False(t, rules.allowsCustomerPort(9001), "a removed rule names no port")
	assert.True(t, rules.allowsCustomerPort(9002), "an install awaiting its verdict keeps its port")
	pending.settle(false)
	assert.False(t, rules.holdsAny())
}

func TestGivenARelayedInstallWhenItsConnectionEndsWithNoVerdictThenItsPortIsNamedAsTheRuleIs(t *testing.T) {
	rules := installedReverseRules(9001)
	fresh, _ := rules.begin(adbwire.AdbActionForward, "tcp:2", 9003)
	rebind, _ := rules.begin(adbwire.AdbActionForward, "tcp:1", 9002)
	fresh.markRelayed()
	rebind.markRelayed()

	// The server finishes what it started: the rule counts as installed, and so does its port.
	fresh.release()
	rebind.release()

	assert.True(t, rules.allowsCustomerPort(9003), "an install the server received names its port")
	assert.True(t, rules.allowsCustomerPort(9002), "a rebind the server received moves the port")
	assert.False(t, rules.allowsCustomerPort(9001), "the old port goes with the old rule")
}

func TestGivenRacingInstallsOfNewSpecsWhenBegunThenExactlyTheLimitTakeSlots(t *testing.T) {
	rules := newReverseRules(testMaxReverseRules)
	outcomes := make(chan bool, 2*testMaxReverseRules)
	var wg sync.WaitGroup
	for index := range 2 * testMaxReverseRules {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, full := rules.begin(adbwire.AdbActionForward, tcpSpec(index), 0)
			outcomes <- full
		}()
	}
	wg.Wait()
	close(outcomes)

	refused := 0
	for full := range outcomes {
		if full {
			refused++
		}
	}
	assert.Equal(t, testMaxReverseRules, refused, "racing installs must fill the table exactly")
}

func TestGivenTwoInstallsOfOneNewSpecWhenTheFirstFailsFirstThenTheSecondStillHoldsTheSlot(t *testing.T) {
	rules := newReverseRules(testMaxReverseRules)
	first, full := rules.begin(adbwire.AdbActionForward, "tcp:1", 9001)
	require.False(t, full)
	second, full := rules.begin(adbwire.AdbActionForward, "tcp:1", 9002)
	require.False(t, full, "a known spec is a rebind, not a new rule")

	first.settle(false)

	_, full = rules.begin(adbwire.AdbActionForward, "tcp:2", 0)
	require.False(t, full)
	_, full = rules.begin(adbwire.AdbActionForward, "tcp:3", 0)
	assert.True(t, full, "an install awaiting its verdict holds the slot")
	assert.False(t, rules.allowsCustomerPort(9001), "a failed install names no port")
	assert.True(t, rules.allowsCustomerPort(9002), "an install awaiting its verdict names its port")
	second.settle(true)
	_, full = rules.begin(adbwire.AdbActionForward, "tcp:3", 0)
	assert.True(t, full, "the confirmed rule keeps the slot")
	assert.True(t, rules.allowsCustomerPort(9002), "the confirmed rule keeps its port")
}

func TestGivenAnInstallAwaitingItsVerdictWhenKillforwardAllIsConfirmedThenItsSlotSurvives(t *testing.T) {
	rules := newReverseRules(testMaxReverseRules)
	install, full := rules.begin(adbwire.AdbActionForward, "tcp:1", 0)
	require.False(t, full)
	killAll, _ := rules.begin(adbwire.AdbActionKillForwardAll, "", 0)

	killAll.settle(true)

	_, full = rules.begin(adbwire.AdbActionForward, "tcp:2", 0)
	require.False(t, full)
	_, full = rules.begin(adbwire.AdbActionForward, "tcp:3", 0)
	assert.True(t, full, "a removal frees no slot an install may still fill")
	install.release()
	_, full = rules.begin(adbwire.AdbActionForward, "tcp:3", 0)
	assert.False(t, full, "the install's own end frees it")
}
