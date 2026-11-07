// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package forward

import (
	"slices"
	"sync"
)

// OpenTurn is one accepted connection's place in its rule's open order. Ready closes
// once every earlier accept on the same rule has read its device-service status word
// or ended its open. Done takes this turn out of the order. It never blocks, and a
// copy or a second call is safe.
type OpenTurn struct {
	Ready <-chan struct{}
	// done takes the turn out of its rule's queue, once. Nil for a turn in no queue.
	done func()
}

// Done takes the turn out of the order. The next turn becomes ready once every turn
// before it is out, so a failed open never lets a later connection pass an earlier one.
func (turn OpenTurn) Done() {
	if turn.done != nil {
		turn.done()
	}
}

// closedReady is closed from the start: the Ready of a turn that never waits.
var closedReady = func() chan struct{} {
	ready := make(chan struct{})
	close(ready)
	return ready
}()

// ImmediateTurn returns a turn that never waits and releases nothing: for a tcp
// remote and for a caller outside a rule.
func ImmediateTurn() OpenTurn {
	return OpenTurn{Ready: closedReady}
}

// openOrder is one rule's queue of turns in accept order. The head's Ready is closed
// and every other turn waits. The zero value is an empty queue. A rule holds one and
// is never copied.
type openOrder struct {
	mutex   sync.Mutex
	pending []chan struct{} // the Ready of each turn handed out and not yet done, in accept order
}

// next returns the turn for the next accept on the rule.
func (order *openOrder) next() OpenTurn {
	order.mutex.Lock()
	defer order.mutex.Unlock()
	ready := make(chan struct{})
	if len(order.pending) == 0 {
		close(ready)
	}
	order.pending = append(order.pending, ready)
	return OpenTurn{Ready: ready, done: sync.OnceFunc(func() { order.leave(ready) })}
}

// leave takes ready's turn out of the queue. When the head leaves, the new head's Ready
// closes. A turn behind the head leaves at once and closes nothing.
func (order *openOrder) leave(ready chan struct{}) {
	order.mutex.Lock()
	defer order.mutex.Unlock()
	index := slices.Index(order.pending, ready)
	order.pending = slices.Delete(order.pending, index, index+1)
	if index == 0 && len(order.pending) > 0 {
		close(order.pending[0])
	}
}
