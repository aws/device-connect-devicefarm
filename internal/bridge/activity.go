package bridge

import (
	"sync"
	"sync/atomic"
	"time"
)

// activityTracker records when one proxied connection last moved bytes in either
// direction. The refresher reads quietFor to find a safe moment to swap websockets;
// onMark forwards each mark to ActiveDevice.MarkActivity.
//
// The zero value is not usable; construct with newActivityTracker.
type activityTracker struct {
	mutex        sync.Mutex
	clock        func() time.Time
	lastActivity time.Time
	onMark       func()

	// inFlightWrites counts writes that entered the socket and have not been stamped. It
	// holds one invariant: quietFor never reports quiet while bytes are still in the
	// socket, nor in the gap between a write completing and its mark. Without it a
	// refresher polling in that gap reads the pre-write silence and swaps into the write.
	inFlightWrites atomic.Int32
}

// newActivityTracker builds a tracker. A nil clock defaults to time.Now; a nil onMark is
// a no-op. The timestamp starts at construction, so a connection that never moves a byte
// reads as quiet rather than idle since the zero time.
func newActivityTracker(clock func() time.Time, onMark func()) *activityTracker {
	if clock == nil {
		clock = time.Now
	}
	if onMark == nil {
		onMark = func() {}
	}
	return &activityTracker{
		clock:        clock,
		lastActivity: clock(),
		onMark:       onMark,
	}
}

// mark stamps the activity clock and notifies the device-level tracker.
func (a *activityTracker) mark() {
	a.mutex.Lock()
	a.lastActivity = a.clock()
	a.mutex.Unlock()
	a.onMark()
}

// beginWrite marks one outbound write as started. Pair every call with endWrite.
func (tracker *activityTracker) beginWrite() { tracker.inFlightWrites.Add(1) }

// endWrite marks one outbound write as finished. It does not stamp the clock: the caller
// marks separately once the write succeeded, and must call this after that mark.
func (tracker *activityTracker) endWrite() { tracker.inFlightWrites.Add(-1) }

// quietFor returns how long since the most recent traffic in either direction, or zero
// while an outbound write is still in the socket.
func (a *activityTracker) quietFor() time.Duration {
	if a.inFlightWrites.Load() > 0 {
		return 0
	}
	a.mutex.Lock()
	defer a.mutex.Unlock()
	return a.clock().Sub(a.lastActivity)
}
