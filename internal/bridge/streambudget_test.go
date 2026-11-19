package bridge

import (
	"math"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGivenAFullBudgetWhenASlotIsReleasedThenTheNextAdmitSucceeds(t *testing.T) {
	budget := &streamBudget{limit: 2}
	require.True(t, budget.admit())
	require.True(t, budget.admit())
	require.False(t, budget.admit(), "a full budget must refuse")

	budget.release()

	assert.True(t, budget.admit(), "a released slot must admit again")
	assert.False(t, budget.admit(), "one release frees one slot")
}

func TestGivenConcurrentAdmitsWhenRacedThenOnlyTheLimitAreHeld(t *testing.T) {
	const limit = 3
	// More racers than slots, so some must be refused.
	const racerCount = 4 * limit
	budget := &streamBudget{limit: limit}
	var admitted atomic.Int64
	var racers sync.WaitGroup
	for range racerCount {
		racers.Add(1)
		go func() {
			defer racers.Done()
			if budget.admit() {
				admitted.Add(1)
			}
		}()
	}
	racers.Wait()

	assert.Equal(t, int64(limit), admitted.Load(), "racing admits must fill the limit exactly")
	assert.Equal(t, int64(limit), budget.inUse.Load(), "a refused admit must not inflate the count")
}

func TestGivenACapPastTheInt32RangeWhenAStreamArrivesThenItIsAdmitted(t *testing.T) {
	budget := &streamBudget{limit: math.MaxInt}

	assert.True(t, budget.admit(), "a large cap must not refuse every stream")
}

func TestGivenRepeatedRefusalsAboveTheCapWhenThrottledThenOnlyOneAMinutePasses(t *testing.T) {
	throttle := &refusalThrottle{}
	firstRefusalTime := time.Date(2026, 6, 26, 12, 0, 0, 0, time.UTC)

	assert.True(t, throttle.allow(firstRefusalTime), "first refusal must log")
	assert.False(t, throttle.allow(firstRefusalTime.Add(capWarnInterval-time.Second)),
		"refusal inside the interval must not log")
	assert.True(t, throttle.allow(firstRefusalTime.Add(capWarnInterval)),
		"refusal at the interval must log")
	assert.True(t, throttle.allow(firstRefusalTime),
		"refusal after the clock steps back must log")
}
