package bridge

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGivenFreshTrackerWhenQuietForCheckedThenMeasuredFromConstruction(t *testing.T) {
	// A connection that never moved a byte reports a gap from construction, not the zero
	// time.
	clock := newFakeClock()
	tracker := newActivityTracker(clock.now)

	clock.advance(5 * time.Second)

	assert.Equal(t, 5*time.Second, tracker.quietFor())
}

func TestGivenTrafficWhenQuietForCheckedThenGapResets(t *testing.T) {
	// A mark must defeat the quiet window: the seam fences the outbound direction, so
	// recent traffic in either direction makes a swap unsafe.
	clock := newFakeClock()
	tracker := newActivityTracker(clock.now)
	clock.advance(30 * time.Second)
	require.Equal(t, 30*time.Second, tracker.quietFor())

	tracker.mark()

	assert.Zero(t, tracker.quietFor(), "traffic must reset the gap")
	clock.advance(2 * time.Second)
	assert.Equal(t, 2*time.Second, tracker.quietFor())
}

func TestGivenRepeatedTrafficWhenQuietForCheckedThenMeasuredFromTheLatest(t *testing.T) {
	// The gap is measured from the MOST RECENT mark, so earlier traffic cannot make a
	// busy connection look quiet.
	clock := newFakeClock()
	tracker := newActivityTracker(clock.now)

	tracker.mark()
	clock.advance(10 * time.Second)
	tracker.mark()
	clock.advance(3 * time.Second)

	assert.Equal(t, 3*time.Second, tracker.quietFor(), "the latest mark must win")
}

func TestGivenNilClockWhenTrackerConstructedThenTheWallClockApplies(t *testing.T) {
	// A nil clock must default to the wall clock rather than the zero time.
	tracker := newActivityTracker(nil)

	tracker.mark()
	tracker.mark()

	assert.Less(t, tracker.quietFor(), time.Minute, "the default clock must be the wall clock")
}

func TestGivenAnInFlightWriteWhenQuietForIsReadThenItReportsZeroUntilTheWriteEnds(t *testing.T) {
	clock := newFakeClock()
	tracker := newActivityTracker(clock.now)
	clock.advance(80 * time.Second)
	require.Equal(t, 80*time.Second, tracker.quietFor())

	tracker.beginWrite()
	assert.Zero(t, tracker.quietFor(), "a write in flight counts even before any mark")

	tracker.endWrite()
	assert.Equal(t, 80*time.Second, tracker.quietFor(), "endWrite alone must not stamp the clock")
}
