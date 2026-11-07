// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package forward

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// isReady reports whether ready is closed, without waiting.
func isReady(ready <-chan struct{}) bool {
	select {
	case <-ready:
		return true
	default:
		return false
	}
}

// pendingCount reads how many turns the order still holds.
func (order *openOrder) pendingCount() int {
	order.mutex.Lock()
	defer order.mutex.Unlock()
	return len(order.pending)
}

func TestGivenTurnsOnOneRuleWhenHandedOutThenEachWaitsForThePrevious(t *testing.T) {
	var order openOrder
	first, second, third := order.next(), order.next(), order.next()

	assert.False(t, isReady(second.Ready), "the second turn must wait for the first")
	first.Done()
	assert.True(t, isReady(second.Ready), "the first turn's Done must release the second")
	assert.False(t, isReady(third.Ready), "the third turn must wait for the second, not the first")
	second.Done()
	assert.True(t, isReady(third.Ready))
}

func TestGivenATurnThatEndsBeforeItsReadyWhenTheNextIsCheckedThenItWaitsForTheEarlierTurn(t *testing.T) {
	// A connection refused before its wait must not let a later one pass an earlier one.
	var order openOrder
	first, second, third := order.next(), order.next(), order.next()

	second.Done()

	assert.False(t, isReady(third.Ready), "the third turn must still wait for the first")
	first.Done()
	assert.True(t, isReady(third.Ready), "a turn done early must not hold the third once the first is done")
}

func TestGivenTurnsDoneBehindALiveHeadWhenCheckedThenTheyLeaveTheQueueAtOnce(t *testing.T) {
	// Connections refused behind a stalled open must not pile up behind it.
	var order openOrder
	head := order.next()
	for range 3 {
		order.next().Done()
	}
	assert.Equal(t, 1, order.pendingCount(), "a turn done behind the head must leave at once")

	head.Done()

	assert.Zero(t, order.pendingCount())
	assert.True(t, isReady(order.next().Ready), "the next accept on an empty queue must not wait")
}

func TestGivenATurnWhenDoneTwiceThenItClosesOnce(t *testing.T) {
	var order openOrder
	turn := order.next()
	copied := turn
	next := order.next()

	assert.NotPanics(t, func() {
		turn.Done()
		turn.Done()
		copied.Done()
		ImmediateTurn().Done()
	}, "a second Done, on the turn or a copy, must not close a closed channel")
	assert.True(t, isReady(next.Ready))
}

func TestGivenTheFirstTurnWhenReadThenItIsReadyAtOnce(t *testing.T) {
	var order openOrder

	assert.True(t, isReady(order.next().Ready), "a rule's first accept has no one to wait for")
	assert.True(t, isReady(ImmediateTurn().Ready))
}
