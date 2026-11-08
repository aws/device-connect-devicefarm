// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package bridge

import (
	"context"
	"hash/maphash"
	"time"
)

// WSRefreshPolicy is the cadence on which a proxied connection's websocket is replaced by
// a fresh one re-attached to the same server-side session.
//
// The zero value disables refreshing (a zero Interval).
type WSRefreshPolicy struct {
	// Interval is how long a websocket is used before a refresh is attempted, counted
	// from its dial.
	Interval time.Duration

	// FreeSecondWait bounds the hunt for a QuietWindow-long idle gap; past it the
	// swap proceeds anyway, so a permanently busy websocket still gets recycled.
	FreeSecondWait time.Duration

	// QuietWindow is the idle gap, in both directions, that makes a swap lossless.
	// Only outbound is at risk: a request written just before the seam is dropped
	// with no error.
	QuietWindow time.Duration

	// PollInterval is how often quiescence is re-checked during the hunt.
	PollInterval time.Duration

	// Jitter pulls Interval earlier by up to this fraction, never later, so concurrent
	// connections' swaps decorrelate and do not overrun the server's per-host cap.
	Jitter float64

	// MaxFailures is how many consecutive failed refreshes are tolerated before the
	// connection is torn down.
	MaxFailures int

	// ReattachTimeout bounds waiting for a successor's reattach ack. Defaults to
	// defaultReattachTimeout; the daemon seeds it from the dial timeout.
	ReattachTimeout time.Duration
}

const (
	// defaultReattachTimeout is the fallback for an unset ReattachTimeout.
	defaultReattachTimeout = 10 * time.Second

	// defaultPollInterval is the fallback for an unset PollInterval.
	defaultPollInterval = 100 * time.Millisecond

	// refreshCycleHeadroom is the slack added to a cycle's worst-case duration to
	// derive WSDeadline.
	refreshCycleHeadroom = 30 * time.Second

	// drainStallMultiple scales WSDeadline into the time a predecessor gets to end (see
	// adbConn.enforceWSDeadline). The server closes a healthy one much sooner.
	drainStallMultiple = 2
)

// normalized returns a copy with each unset optional field defaulted. Never fills
// Interval: a zero Interval is how a caller disables refreshing.
func (p WSRefreshPolicy) normalized() WSRefreshPolicy {
	if p.ReattachTimeout <= 0 {
		p.ReattachTimeout = defaultReattachTimeout
	}
	if p.PollInterval <= 0 {
		p.PollInterval = defaultPollInterval
	}
	return p
}

// reactiveTimeout bounds a whole reactive recovery; both attach attempts share it.
// Normalizes first: a zero ReattachTimeout would fail every reconnect at once.
func (p WSRefreshPolicy) reactiveTimeout() time.Duration {
	return 2 * p.normalized().ReattachTimeout
}

// WSDeadline is how long one websocket may live before it is presumed wedged: the
// worst-case healthy cycle plus headroom, so a cadence change cannot make it fire on a
// healthy connection. Counted from the websocket's dial, like the cycle. Zero Interval
// means no deadline. Bounds the websocket, not the adb TCP connection.
func (p WSRefreshPolicy) WSDeadline() time.Duration {
	if p.Interval <= 0 {
		return 0
	}
	p = p.normalized()
	// The jitter only shortens a wait, so Interval is the longest one. Two round trips
	// per attempt (dial, then ack), charged at ReattachTimeout since the daemon seeds
	// it from DialTimeout. Undercounting fires on healthy websockets: the poll after a
	// failed refresh inherits the age of every attempt just spent.
	attachBudget := AttachAttempts * 2 * p.ReattachTimeout
	return p.Interval + p.FreeSecondWait + attachBudget + refreshCycleHeadroom
}

// jitterSpan is the most Interval can be pulled earlier by.
func (p WSRefreshPolicy) jitterSpan() time.Duration {
	if p.Jitter <= 0 {
		return 0
	}
	return time.Duration(float64(p.Interval) * p.Jitter)
}

// nextInterval returns Interval less a pseudo-random offset in [0, jitterSpan].
// maphash, not rand: this needs decorrelation, not unpredictability.
func (p WSRefreshPolicy) nextInterval() time.Duration {
	offset := time.Duration(maphash.Bytes(maphash.MakeSeed(), nil) % uint64(p.jitterSpan()+1))
	return p.Interval - offset
}

// quiescenceDecision is the verdict of one poll during the hunt for a quiet moment.
type quiescenceDecision int

const (
	// quiesceWait means keep polling: recent traffic, and the hunt has time left.
	quiesceWait quiescenceDecision = iota
	// quiesceNow means quiet enough to swap immediately.
	quiesceNow
	// quiesceExpired means the hunt is over; refresh despite the traffic.
	quiesceExpired
)

// decideQuiescence is the hunt's whole timing rule, pure so it is table-testable. Quiet
// wins over expiry when both hold, since a quiet swap is the lossless one.
func decideQuiescence(quietFor, waitedFor time.Duration, p WSRefreshPolicy) quiescenceDecision {
	if quietFor >= p.QuietWindow {
		return quiesceNow
	}
	if waitedFor >= p.FreeSecondWait {
		return quiesceExpired
	}
	return quiesceWait
}

// runRefresher drives one connection's refresh cycle until ctx ends, done closes, or the
// connection is torn down. Started as pipeConn's aux goroutine, so it never outlives the
// pipe. One poll loop, not one long sleep, so WSDeadline is enforced promptly.
func runRefresher(ctx context.Context, conn *adbConn, policy WSRefreshPolicy, done <-chan struct{}) {
	if policy.Interval <= 0 {
		return
	}
	policy = policy.normalized()

	// Nothing can be re-attached to before the server announces a session UUID.
	select {
	case <-conn.uuidReady:
	case <-ctx.Done():
		return
	case <-done:
		return
	}

	// The cycle counts from the websocket's dial, so time spent parked or dialing is spent;
	// a remainder of zero or less hunts on the first poll.
	interval := policy.nextInterval()
	cycleStarted := conn.writeOpenedAt()
	hunting := false
	var huntStarted time.Time

	for {
		if !sleepFor(ctx, done, policy.PollInterval) {
			return
		}
		if conn.isClosed() {
			return
		}

		// Every poll, independent of the cycle.
		if conn.enforceWSDeadline(policy.WSDeadline()) {
			cycleStarted = time.Now()
			hunting = false
			continue
		}

		if !hunting {
			if time.Since(cycleStarted) < interval {
				continue
			}
			hunting = true
			// The hunt counts from the interval's end, so a late poll or a gate miss shortens
			// it instead of restarting it.
			huntStarted = cycleStarted.Add(interval)
		}

		decision := decideQuiescence(conn.activity.quietFor(), time.Since(huntStarted), policy)
		if decision == quiesceWait {
			continue
		}

		// No gate on the expired path: there the refresh must happen regardless.
		var gate func() bool
		if decision == quiesceNow {
			gate = func() bool { return conn.activity.quietFor() >= policy.QuietWindow }
		}

		beforeRefresh := conn.writeOpenedAt()
		err := conn.refresh(ctx, gate)
		if err != nil {
			conn.logger.Debug("websocket refresh failed", "error", err)
			if policy.MaxFailures > 0 && conn.failureCount() >= policy.MaxFailures {
				conn.logger.Warn("giving up on the proxied connection after repeated refresh failures",
					"failures", conn.failureCount())
				conn.Close()
				return
			}
		}
		if conn.isClosed() {
			return
		}

		// Re-jitter every cycle.
		interval = policy.nextInterval()
		cycleStarted = nextCycleAnchor(beforeRefresh, conn.writeOpenedAt(), err != nil, time.Now())
		hunting = false
	}
}

// nextCycleAnchor is where the next cycle counts from: the dial of the websocket now written
// to, a swap's successor or a reactive recovery's replacement. A failed refresh that left the
// same websocket in service restarts the wait from now: the deadline guard, not a retry, ends it.
func nextCycleAnchor(beforeRefresh, afterRefresh time.Time, failed bool, now time.Time) time.Time {
	if failed && afterRefresh.Equal(beforeRefresh) {
		return now
	}
	return afterRefresh
}

// sleepFor waits for d, reporting false if ctx ended or done closed first.
func sleepFor(ctx context.Context, done <-chan struct{}, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	case <-done:
		return false
	}
}
