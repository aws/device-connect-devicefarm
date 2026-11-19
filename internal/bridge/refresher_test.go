package bridge

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// quiescencePolicy is the timing policy the decideQuiescence table exercises: a
// 1-second quiet window hunted for up to 60 seconds, matching production defaults.
func quiescencePolicy() WSRefreshPolicy {
	return WSRefreshPolicy{
		Interval:       3 * time.Minute,
		FreeSecondWait: 60 * time.Second,
		QuietWindow:    time.Second,
		PollInterval:   100 * time.Millisecond,
	}
}

func TestGivenQuiescenceInputsWhenDecidedThenVerdictMatchesTable(t *testing.T) {
	policy := quiescencePolicy()

	tests := []struct {
		name      string
		quietFor  time.Duration
		waitedFor time.Duration
		want      quiescenceDecision
	}{
		{name: "busy early in the hunt waits", quietFor: 0, waitedFor: 0, want: quiesceWait},
		{name: "briefly quiet but under the window waits", quietFor: 999 * time.Millisecond, waitedFor: time.Second, want: quiesceWait},
		{name: "exactly at the quiet window swaps now", quietFor: time.Second, waitedFor: time.Second, want: quiesceNow},
		{name: "well past the quiet window swaps now", quietFor: 10 * time.Second, waitedFor: 0, want: quiesceNow},
		{name: "busy just under the hunt limit waits", quietFor: 0, waitedFor: 59999 * time.Millisecond, want: quiesceWait},
		{name: "busy exactly at the hunt limit expires", quietFor: 0, waitedFor: 60 * time.Second, want: quiesceExpired},
		{name: "busy past the hunt limit expires", quietFor: 10 * time.Millisecond, waitedFor: 90 * time.Second, want: quiesceExpired},
		// Quiet wins over expiry when both hold: a quiet swap is the lossless one.
		{name: "quiet and expired prefers the quiet path", quietFor: 2 * time.Second, waitedFor: 90 * time.Second, want: quiesceNow},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, decideQuiescence(tc.quietFor, tc.waitedFor, policy))
		})
	}
}

func TestGivenZeroQuietWindowWhenDecidedThenAlwaysSwapsImmediately(t *testing.T) {
	// A zero window means "do not hunt", which must resolve immediately rather than
	// waiting out the whole free-second budget.
	policy := WSRefreshPolicy{FreeSecondWait: time.Minute}

	assert.Equal(t, quiesceNow, decideQuiescence(0, 0, policy))
}

func TestGivenJitteredPolicyWhenIntervalDerivedThenStaysWithinTheBand(t *testing.T) {
	policy := quiescencePolicy()
	policy.Jitter = 0.10

	low := policy.Interval - policy.jitterSpan()
	high := policy.Interval + policy.jitterSpan()
	// Many draws, since the point of jitter is that successive values differ.
	distinct := make(map[time.Duration]struct{})
	for range 200 {
		got := policy.nextInterval()
		assert.GreaterOrEqual(t, got, low, "a jittered interval must not fall below the band")
		assert.LessOrEqual(t, got, high, "a jittered interval must not exceed the band")
		distinct[got] = struct{}{}
	}

	assert.Greater(t, len(distinct), 1, "jitter must actually vary the interval, not return a constant")
}

func TestGivenNoJitterWhenIntervalDerivedThenExactIntervalReturned(t *testing.T) {
	policy := quiescencePolicy()

	assert.Equal(t, policy.Interval, policy.nextInterval(), "a zero jitter must be exact")
}

func TestGivenPolicyWhenWSDeadlineDerivedThenExceedsTheWorstCaseHealthyCycle(t *testing.T) {
	// The deadline must exceed the longest healthy cycle, or it fires mid-refresh.
	// ReattachTimeout is set above refreshCycleHeadroom so the headroom cannot mask a
	// missing term.
	policy := quiescencePolicy()
	policy.Jitter = 0.10
	policy.ReattachTimeout = refreshCycleHeadroom + 10*time.Second

	roundTripsPerAttempt := 2 // the dial, then the ack
	worstCaseHealthyCycle := policy.Interval + policy.jitterSpan() + policy.FreeSecondWait +
		attachAttempts*time.Duration(roundTripsPerAttempt)*policy.ReattachTimeout

	assert.Greater(t, policy.wsDeadline(), worstCaseHealthyCycle,
		"the deadline must leave headroom above the worst-case healthy cycle")
}

func TestGivenRefreshThatSpentItsWholeAttachBudgetWhenItFailedThenTheGuardDoesNotFireOnTheNextPoll(t *testing.T) {
	// failRefreshLocked restores phaseSteady without re-arming readSince, so the next poll
	// sees an age that includes every attempt the failed refresh spent. The deadline must
	// budget for those attempts.
	policy := quiescencePolicy()
	policy.Jitter = 0.10
	policy.ReattachTimeout = 10 * time.Second

	dialTimeout := policy.ReattachTimeout // DaemonManager.refreshPolicy seeds them together
	ageAfterAFailedRefresh := policy.Interval + policy.jitterSpan() + policy.FreeSecondWait +
		attachAttempts*(dialTimeout+policy.ReattachTimeout)

	assert.Greater(t, policy.wsDeadline(), ageAfterAFailedRefresh,
		"a websocket that merely lived through one exhausted refresh must survive the next poll")
}

func TestGivenRefreshingDisabledWhenWSDeadlineDerivedThenNoDeadline(t *testing.T) {
	// With refreshing off there is no cycle to guard, so the websocket must never be
	// dropped on a timer.
	assert.Zero(t, WSRefreshPolicy{}.wsDeadline())
}

func TestGivenUnsetTimeoutsWhenNormalizedThenDefaultsApply(t *testing.T) {
	// A zero-valued policy must not yield a zero poll interval (which would spin) or
	// a zero reattach timeout (which would fail every refresh instantly).
	policy := WSRefreshPolicy{}.normalized()

	assert.Equal(t, defaultPollInterval, policy.PollInterval)
	assert.Equal(t, defaultReattachTimeout, policy.ReattachTimeout)
	assert.Equal(t, 2*defaultReattachTimeout, policy.reactiveTimeout())
}

func TestGivenExplicitTimeoutsWhenNormalizedThenExplicitValuesWin(t *testing.T) {
	policy := WSRefreshPolicy{
		PollInterval:    5 * time.Millisecond,
		ReattachTimeout: 2 * time.Second,
	}.normalized()

	assert.Equal(t, 5*time.Millisecond, policy.PollInterval)
	assert.Equal(t, 2*time.Second, policy.ReattachTimeout)
	assert.Equal(t, 4*time.Second, policy.reactiveTimeout())
}

func TestGivenDisabledRefreshingWhenNormalizedThenIntervalStaysZero(t *testing.T) {
	// A zero Interval is how a caller disables refreshing, and several tests pass
	// WSRefreshPolicy{} for exactly that. Normalizing must not resurrect it.
	assert.Zero(t, WSRefreshPolicy{}.normalized().Interval,
		"normalizing must not turn refreshing back on")
}
