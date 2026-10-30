package config

import (
	"math"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGivenDefaultConfigWhenConstructedThenHasExpectedDefaults(t *testing.T) {
	cfg := Default()

	assert.Equal(t, DefaultListenAddr, cfg.ListenAddr)
	assert.Equal(t, DefaultListenPort, cfg.ListenPort)
	assert.False(t, cfg.Debug, "debug logging is off by default (info level)")
	assert.Equal(t, DefaultStopTimeout, cfg.StopTimeout)
	assert.Equal(t, DefaultDialTimeout, cfg.DialTimeout)
	assert.Equal(t, DefaultEndpointRefreshInterval, cfg.EndpointRefreshInterval)
	assert.Equal(t, DefaultWSRefreshInterval, cfg.WSRefreshInterval)
	assert.Equal(t, DefaultFreeSecondWait, cfg.FreeSecondWait)
	assert.Equal(t, DefaultQuietWindow, cfg.QuietWindow)
	assert.Equal(t, DefaultQuiescePoll, cfg.QuiescePoll)
}

func TestGivenValidDefaultsWhenValidatedThenAccepted(t *testing.T) {
	require.NoError(t, Default().Validate())
}

func TestGivenInvalidFieldValuesWhenValidatedThenRejected(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*ProxyConfig)
	}{
		{name: "empty listen addr", mutate: func(c *ProxyConfig) { c.ListenAddr = "" }},
		{name: "zero port", mutate: func(c *ProxyConfig) { c.ListenPort = 0 }},
		{name: "port too high", mutate: func(c *ProxyConfig) { c.ListenPort = 70000 }},
		{name: "zero refresh interval", mutate: func(c *ProxyConfig) { c.EndpointRefreshInterval = 0 }},
		{name: "zero websocket refresh interval", mutate: func(c *ProxyConfig) { c.WSRefreshInterval = 0 }},
		{name: "zero free-second wait", mutate: func(c *ProxyConfig) { c.FreeSecondWait = 0 }},
		{name: "zero quiet window", mutate: func(c *ProxyConfig) { c.QuietWindow = 0 }},
		// A swap dials an endpoint up to one EndpointRefreshInterval old, so an
		// interval at or past the URL validity lets a swap dial an expired URL.
		{name: "endpoint refresh too close to url expiry",
			mutate: func(c *ProxyConfig) { c.EndpointRefreshInterval = endpointURLValidity }},
		// The margin check must not overflow into a pass.
		{name: "endpoint refresh at the maximum duration",
			mutate: func(c *ProxyConfig) { c.EndpointRefreshInterval = math.MaxInt64 }},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Default()
			tc.mutate(&cfg)

			require.Error(t, cfg.Validate())
		})
	}
}

func TestGivenNonAddressListenAddrWhenValidatedThenRejected(t *testing.T) {
	// The port comes from --port. Without this check a host:port, bracketed or
	// broken address fails only when the detached daemon binds.
	for _, addr := range []string{"127.0.0.1:5037", "localhost:5037", "[::1]:5037", "[::1]", "::1::2"} {
		t.Run(addr, func(t *testing.T) {
			cfg := Default()
			cfg.ListenAddr = addr

			require.ErrorContains(t, cfg.Validate(), "no port or brackets")
		})
	}
}

func TestGivenBareListenAddrWhenValidatedThenAccepted(t *testing.T) {
	// Any bare IP or hostname passes, loopback or not; the README security note
	// carries the warning, not Validate. IPv6 colons are not host:port syntax.
	for _, addr := range []string{"0.0.0.0", "::", "example.com", "::1", "::1%lo", "::ffff:127.0.0.1"} {
		t.Run(addr, func(t *testing.T) {
			cfg := Default()
			cfg.ListenAddr = addr

			require.NoError(t, cfg.Validate())
		})
	}
}

func TestGivenFullyPopulatedConfigWhenConvertedToDaemonArgsThenForwardedFlagsRoundTrip(t *testing.T) {
	cfg := ProxyConfig{
		ListenAddr: "127.0.0.1",
		ListenPort: 5037,
		LogPath:    "/var/log/devicefarm-connect.log",
	}

	args := cfg.AsDaemonArgs()

	flags := pairFlags(args)
	assert.Equal(t, "127.0.0.1", flags["--listen-addr"])
	assert.Equal(t, "5037", flags["--port"])

	// The durations are fixed constants seeded into the child by Default, so they
	// are not forwarded; the parent owns the logfile. None of these must appear on
	// the child's argv.
	for _, absent := range []string{
		"--log", "--dial-timeout", "--refresh-interval",
		"--endpoint-refresh-interval", "--stop-timeout",
	} {
		_, present := flags[absent]
		assert.Falsef(t, present, "%s must not be forwarded to the daemon child", absent)
	}
}

func TestGivenDebugFlagWhenConvertedToDaemonArgsThenForwardedOnlyWhenSet(t *testing.T) {
	// --debug is a boolean flag, so it is forwarded as a bare flag (no value) and
	// only when set: the child must inherit the parent's debug mode, but a default
	// (info) config must not carry it.
	off := Default()
	assert.NotContains(t, off.AsDaemonArgs(), "--debug", "info-level config must not forward --debug")

	on := Default()
	on.Debug = true
	assert.Contains(t, on.AsDaemonArgs(), "--debug", "a debug config must forward --debug to the child")
}

func TestGivenReverseOnWhenConvertedToDaemonArgsThenTheFlagIsForwarded(t *testing.T) {
	// The child opens the notifier and runs the seam. A flag lost at the re-exec refuses a
	// customer who opted in.
	on := Default()
	on.ReverseEnabled = true

	assert.Contains(t, on.AsDaemonArgs(), "--allow-adb-reverse")
}

func TestGivenReverseOffWhenConvertedToDaemonArgsThenTheFlagIsAbsent(t *testing.T) {
	assert.NotContains(t, Default().AsDaemonArgs(), "--allow-adb-reverse",
		"reverse is off by default and the child seeds the same default")
}

func TestGivenRefreshEnvOverrideWhenReadFromEnvThenPositiveMillisWinElseFallback(t *testing.T) {
	tests := []struct {
		name string
		// set reports whether the env var should be set at all, and to what.
		set   bool
		value string
		want  time.Duration
	}{
		{name: "unset falls back", set: false, want: DefaultWSRefreshInterval},
		{name: "positive millis override", set: true, value: "500", want: 500 * time.Millisecond},
		{name: "empty falls back", set: true, value: "", want: DefaultWSRefreshInterval},
		{name: "zero falls back", set: true, value: "0", want: DefaultWSRefreshInterval},
		{name: "negative falls back", set: true, value: "-100", want: DefaultWSRefreshInterval},
		{name: "garbage falls back", set: true, value: "abc", want: DefaultWSRefreshInterval},
		// These two wrap to a negative duration and to exactly zero. See durationFromEnv.
		{name: "overflow to negative falls back", set: true, value: "9223372036855", want: DefaultWSRefreshInterval},
		{name: "overflow to zero falls back", set: true, value: "4611686018427387904", want: DefaultWSRefreshInterval},
		{name: "the largest non-overflowing count is honored", set: true, value: "9223372036854",
			want: 9223372036854 * time.Millisecond},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.set {
				t.Setenv(wsRefreshIntervalEnvVar, tc.value)
			} else {
				// t.Setenv both sets and (via cleanup) restores the var; there is no
				// t.Unsetenv, so clear it explicitly to model the unset case.
				t.Setenv(wsRefreshIntervalEnvVar, "")
				require.NoError(t, os.Unsetenv(wsRefreshIntervalEnvVar))
			}

			assert.Equal(t, tc.want, Default().WithRefreshOverridesFromEnv().WSRefreshInterval)
		})
	}
}

func TestGivenOneRefreshOverrideSetWhenReadFromEnvThenOtherFieldsKeepTheirValues(t *testing.T) {
	// Each override falls back independently, so shrinking the interval for a test
	// must not silently disturb the quiescence hunt's own durations.
	t.Setenv(wsRefreshIntervalEnvVar, "250")

	cfg := Default().WithRefreshOverridesFromEnv()

	assert.Equal(t, 250*time.Millisecond, cfg.WSRefreshInterval)
	assert.Equal(t, DefaultFreeSecondWait, cfg.FreeSecondWait)
	assert.Equal(t, DefaultQuietWindow, cfg.QuietWindow)
}

func TestGivenAllRefreshOverridesSetWhenReadFromEnvThenEachFieldTakesItsOwnValue(t *testing.T) {
	t.Setenv(wsRefreshIntervalEnvVar, "1500")
	t.Setenv(freeSecondWaitEnvVar, "800")
	t.Setenv(quietWindowEnvVar, "50")

	cfg := Default().WithRefreshOverridesFromEnv()

	assert.Equal(t, 1500*time.Millisecond, cfg.WSRefreshInterval)
	assert.Equal(t, 800*time.Millisecond, cfg.FreeSecondWait)
	assert.Equal(t, 50*time.Millisecond, cfg.QuietWindow)
}

func TestGivenDefaultConfigWhenConstructedThenTheNewKnobsCarryTheirDefaults(t *testing.T) {
	// Relationships, not literals: each one is a reason the knob exists.
	cfg := Default()

	assert.Equal(t, cfg.DialTimeout, cfg.DeviceStreamStatusTimeout,
		"a status-word read is one round trip to the endpoint, like a dial")
	assert.Less(t, cfg.NotifierKeepAlive, loadBalancerIdleTimeout,
		"a PING must land before the load balancer idles the notifier out")
	assert.LessOrEqual(t, cfg.NotifierBackoffInitial, cfg.NotifierBackoffMax)
	assert.Less(t, cfg.MaxDeviceStreams, adbAdmissionCeiling,
		"the shell, the probe and swap overlaps need admissions too")
	assert.LessOrEqual(t, cfg.MaxForwardRules, cfg.MaxDeviceStreams,
		"every rule with one connection each must fit the stream budget")
	assert.False(t, cfg.ReverseEnabled, "reverse is off until --allow-adb-reverse sets it")
}

func TestGivenInvalidForwardReverseKnobsWhenValidatedThenRejected(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*ProxyConfig)
	}{
		{name: "zero status timeout", mutate: func(c *ProxyConfig) { c.DeviceStreamStatusTimeout = 0 }},
		{name: "negative status timeout", mutate: func(c *ProxyConfig) { c.DeviceStreamStatusTimeout = -time.Second }},
		{name: "zero stream cap", mutate: func(c *ProxyConfig) { c.MaxDeviceStreams = 0 }},
		{name: "negative stream cap", mutate: func(c *ProxyConfig) { c.MaxDeviceStreams = -1 }},
		{name: "stream cap at the admission ceiling", mutate: func(cfg *ProxyConfig) { cfg.MaxDeviceStreams = adbAdmissionCeiling }},
		{name: "stream cap above the admission ceiling", mutate: func(cfg *ProxyConfig) { cfg.MaxDeviceStreams = adbAdmissionCeiling + 1 }},
		{name: "zero rule cap", mutate: func(c *ProxyConfig) { c.MaxForwardRules = 0 }},
		{name: "negative rule cap", mutate: func(c *ProxyConfig) { c.MaxForwardRules = -1 }},
		{name: "zero reverse rule cap", mutate: func(c *ProxyConfig) { c.MaxReverseRules = 0 }},
		{name: "negative reverse rule cap", mutate: func(c *ProxyConfig) { c.MaxReverseRules = -1 }},
		{name: "zero keepalive", mutate: func(c *ProxyConfig) { c.NotifierKeepAlive = 0 }},
		{name: "negative keepalive", mutate: func(c *ProxyConfig) { c.NotifierKeepAlive = -time.Second }},
		{name: "keepalive at the load balancer idle timeout", mutate: func(c *ProxyConfig) { c.NotifierKeepAlive = loadBalancerIdleTimeout }},
		{name: "zero initial backoff", mutate: func(c *ProxyConfig) { c.NotifierBackoffInitial = 0 }},
		{name: "negative initial backoff", mutate: func(c *ProxyConfig) { c.NotifierBackoffInitial = -time.Second }},
		{name: "zero max backoff", mutate: func(c *ProxyConfig) { c.NotifierBackoffMax = 0 }},
		{name: "negative max backoff", mutate: func(c *ProxyConfig) { c.NotifierBackoffMax = -time.Second }},
		{name: "max backoff below the initial", mutate: func(c *ProxyConfig) {
			c.NotifierBackoffInitial = 10 * time.Second
			c.NotifierBackoffMax = 9 * time.Second
		}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Default()
			tc.mutate(&cfg)

			require.Error(t, cfg.Validate())
		})
	}
}

func TestGivenNotifierEnvOverridesWhenReadFromEnvThenPositiveMillisWinElseFallback(t *testing.T) {
	tests := []struct {
		name  string
		set   bool
		value string
		want  time.Duration
	}{
		{name: "unset falls back", set: false, want: DefaultNotifierKeepAlive},
		{name: "positive millis override", set: true, value: "500", want: 500 * time.Millisecond},
		{name: "empty falls back", set: true, value: "", want: DefaultNotifierKeepAlive},
		{name: "zero falls back", set: true, value: "0", want: DefaultNotifierKeepAlive},
		{name: "negative falls back", set: true, value: "-100", want: DefaultNotifierKeepAlive},
		{name: "garbage falls back", set: true, value: "soon", want: DefaultNotifierKeepAlive},
		{name: "overflow falls back", set: true, value: "9223372036855", want: DefaultNotifierKeepAlive},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.set {
				t.Setenv(notifierKeepAliveEnvVar, tc.value)
			} else {
				t.Setenv(notifierKeepAliveEnvVar, "")
				require.NoError(t, os.Unsetenv(notifierKeepAliveEnvVar))
			}

			cfg := Default().WithForwardReverseOverridesFromEnv()

			assert.Equal(t, tc.want, cfg.NotifierKeepAlive)
			// The other knobs fall back independently.
			assert.Equal(t, DefaultDeviceStreamStatusTimeout, cfg.DeviceStreamStatusTimeout)
			assert.Equal(t, DefaultNotifierBackoffInitial, cfg.NotifierBackoffInitial)
			assert.Equal(t, DefaultMaxDeviceStreams, cfg.MaxDeviceStreams)
			assert.Equal(t, DefaultMaxForwardRules, cfg.MaxForwardRules)
			assert.Equal(t, DefaultMaxReverseRules, cfg.MaxReverseRules)
		})
	}
}

func TestGivenEveryForwardReverseOverrideSetWhenReadFromEnvThenEachFieldTakesItsOwnValueAndTheBackoffCapStays(t *testing.T) {
	t.Setenv(deviceStreamStatusEnvVar, "1500")
	t.Setenv(maxDeviceStreamsEnvVar, "3")
	t.Setenv(maxForwardRulesEnvVar, "4")
	t.Setenv(maxReverseRulesEnvVar, "5")
	t.Setenv(notifierKeepAliveEnvVar, "800")
	t.Setenv(notifierBackoffEnvVar, "50")

	cfg := Default().WithForwardReverseOverridesFromEnv()

	assert.Equal(t, 1500*time.Millisecond, cfg.DeviceStreamStatusTimeout)
	assert.Equal(t, 3, cfg.MaxDeviceStreams)
	assert.Equal(t, 4, cfg.MaxForwardRules)
	assert.Equal(t, 5, cfg.MaxReverseRules)
	assert.Equal(t, 800*time.Millisecond, cfg.NotifierKeepAlive)
	assert.Equal(t, 50*time.Millisecond, cfg.NotifierBackoffInitial)
	assert.Equal(t, DefaultNotifierBackoffMax, cfg.NotifierBackoffMax,
		"the backoff override moves the initial delay only; the cap has no override")
}

func TestGivenMaxDeviceStreamsOverrideWhenReadFromEnvThenPositiveCountsWinElseFallback(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  int
	}{
		{name: "a positive count is honored", value: "7", want: 7},
		{name: "zero falls back", value: "0", want: DefaultMaxDeviceStreams},
		{name: "negative falls back", value: "-4", want: DefaultMaxDeviceStreams},
		{name: "unparseable falls back", value: "many", want: DefaultMaxDeviceStreams},
		{name: "out of range falls back", value: "99999999999999999999", want: DefaultMaxDeviceStreams},
		{name: "empty falls back", value: "", want: DefaultMaxDeviceStreams},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(maxDeviceStreamsEnvVar, tc.value)

			assert.Equal(t, tc.want, Default().WithForwardReverseOverridesFromEnv().MaxDeviceStreams)
		})
	}
}

// pairFlags turns a flat ["--flag", "value", ...] slice into a lookup map. It
// asserts the slice is well-formed (even length) so a malformed AsDaemonArgs
// surfaces as a test failure rather than a silent panic.
func pairFlags(args []string) map[string]string {
	flags := make(map[string]string, len(args)/2)
	for i := 0; i+1 < len(args); i += 2 {
		flags[args[i]] = args[i+1]
	}
	return flags
}

func TestGivenAStreamCapOverrideAtTheAdmissionCeilingWhenReadFromEnvThenValidateRejectsIt(t *testing.T) {
	t.Setenv(maxDeviceStreamsEnvVar, strconv.Itoa(adbAdmissionCeiling))

	cfg := Default().WithForwardReverseOverridesFromEnv()

	require.Equal(t, adbAdmissionCeiling, cfg.MaxDeviceStreams, "the override is read as given, not clamped")
	require.Error(t, cfg.Validate(), "an override cannot plan for slots the service does not have")
}
