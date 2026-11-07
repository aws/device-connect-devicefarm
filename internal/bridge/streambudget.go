// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package bridge

import (
	"sync/atomic"
	"time"
)

// streamBudget is one attachment's device-stream slots. Local adb connections, forwarded
// streams and connectors share it: each spends one websocket from the same server-side pool.
type streamBudget struct {
	limit int
	inUse atomic.Int64 // slots taken
}

// admit takes a slot, or returns false if all are taken. CAS, not add-and-back-off: a
// refusal burst would inflate the count and refuse a caller with free slots.
func (budget *streamBudget) admit() bool {
	for {
		inUse := budget.inUse.Load()
		if inUse >= int64(budget.limit) {
			return false
		}
		if budget.inUse.CompareAndSwap(inUse, inUse+1) {
			return true
		}
	}
}

// release returns one slot: once per successful admit.
func (budget *streamBudget) release() {
	budget.inUse.Add(-1)
}

// capWarnInterval limits refusal Warns to one per interval per caller. A refusal costs no
// round trip, so a retry loop would flood the log. Source: pick.
const capWarnInterval = time.Minute

// refusalThrottle is one caller's refusal-Warn throttle. One per caller, not per budget: a
// forwarded retry loop must not silence the connector refusals.
type refusalThrottle struct {
	lastWarn atomic.Int64 // unix nanos of the last logged refusal
}

// allow reports whether this refusal logs: one per capWarnInterval. CAS on the stamp, so
// racing refusals log once. A clock stepped back counts as expired.
func (throttle *refusalThrottle) allow(now time.Time) bool {
	lastWarn := throttle.lastWarn.Load()
	if sinceWarn := now.UnixNano() - lastWarn; sinceWarn >= 0 && sinceWarn < int64(capWarnInterval) {
		return false
	}
	return throttle.lastWarn.CompareAndSwap(lastWarn, now.UnixNano())
}
