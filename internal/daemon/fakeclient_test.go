// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"errors"
	"slices"
	"sync"
	"time"

	"github.com/aws/device-connect-devicefarm/internal/session"
)

// Well-formed session ARNs for every add the daemon tests send: the add handler
// validates the ARN before anything else, so a shorthand like "arn:1" is refused.
const (
	testSessionArnOne   = "arn:aws:devicefarm:us-west-2:111122223333:session:aaaaaaaa-1111-4111-a111-111111111111"
	testSessionArnTwo   = "arn:aws:devicefarm:us-west-2:111122223333:session:bbbbbbbb-2222-4222-a222-222222222222"
	testSessionArnThree = "arn:aws:devicefarm:us-west-2:111122223333:session:cccccccc-3333-4333-a333-333333333333"
	testSessionArnOther = "arn:aws:devicefarm:us-west-2:111122223333:session:dddddddd-4444-4444-a444-444444444444"
)

// Resolved endpoints for the daemon tests. The daemon stores whatever the
// RemoteAccessClient returns verbatim: path-less, because each dial site names its
// route at dial time. testEndpointRenewed stands in for the URL a later refresh
// resolves to. Both name loopback port 0, which nothing can ever listen on, never a
// routable host: an attachment dials its notifier on its own goroutine, so a unit test
// must fail that dial at once instead of putting bytes on the network. A test that needs
// the dial to succeed passes a mock's URL.
const (
	testEndpoint        = "wss://127.0.0.1:0/websocket"
	testEndpointRenewed = "wss://127.0.0.1:0/renewed"
)

// fakeRemoteAccessClient is a session.RemoteAccessClient that returns a canned
// endpoint and label, so the daemon's add and refresh paths are exercised without
// real AWS credentials or a network call. Its fields are mutable under a mutex so
// a test can make a later resolve return a new endpoint or an error after an
// initial attach succeeded, or answer late to drive a lookup bound. It records
// each resolved ARN so a test can assert that the one-device short-circuit avoids
// the AWS call. Because the daemon depends on the application-layer
// RemoteAccessClient interface, this fake carries no aws-sdk-go-v2 types.
type fakeRemoteAccessClient struct {
	mu       sync.Mutex
	endpoint string
	label    string
	err      error
	delay    time.Duration
	arns     []string
	// options holds the lookup options of each GetSession call, in call order.
	options [][]session.LookupOption
	// returnedArns maps an asked ARN to the ARN the answer names. An ARN not in the
	// map answers with no ARN, so the add makes one lookup.
	returnedArns map[string]string
}

// newFakeClient builds a client that resolves to the given endpoint and label.
func newFakeClient(endpoint, label string) *fakeRemoteAccessClient {
	return &fakeRemoteAccessClient{endpoint: endpoint, label: label}
}

// set replaces what the next GetSession returns: a new endpoint (with err nil) or,
// when err is non-nil, that error. It models the session being renewed or having
// become unresolvable between refresh ticks.
func (f *fakeRemoteAccessClient) set(endpoint string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.endpoint = endpoint
	f.err = err
}

// returnArnFor makes lookups of asked answer with returned as the session ARN.
func (f *fakeRemoteAccessClient) returnArnFor(asked, returned string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.returnedArns == nil {
		f.returnedArns = map[string]string{}
	}
	f.returnedArns[asked] = returned
}

// resolvedARNs returns the ARNs GetSession was called with, in order.
func (f *fakeRemoteAccessClient) resolvedARNs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.arns...)
}

// delayAnswersBy makes later GetSession calls wait delay, or fail with ctx.Err() when the
// caller's bound ends first. It models a throttled lookup.
func (f *fakeRemoteAccessClient) delayAnswersBy(delay time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.delay = delay
}

// lookupOptions returns the options of each GetSession call, in order.
func (f *fakeRemoteAccessClient) lookupOptions() [][]session.LookupOption {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.options)
}

func (f *fakeRemoteAccessClient) GetSession(ctx context.Context, sessionARN string,
	options ...session.LookupOption) (session.RemoteAccessInfo, error) {
	f.mu.Lock()
	f.arns = append(f.arns, sessionARN)
	f.options = append(f.options, options)
	endpoint, label, err, delay := f.endpoint, f.label, f.err, f.delay
	returnedARN := f.returnedArns[sessionARN]
	f.mu.Unlock()
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return session.RemoteAccessInfo{}, ctx.Err()
		}
	}
	if err != nil {
		return session.RemoteAccessInfo{}, err
	}
	return session.RemoteAccessInfo{Endpoint: endpoint, ARN: returnedARN, Label: label}, nil
}

// attachDuringResolve wraps a RemoteAccessClient and runs attach before
// delegating, modeling a concurrent caller binding the device while a resolve is
// in flight, so a test can drive the add handler's Set-fails defense branch.
type attachDuringResolve struct {
	inner  session.RemoteAccessClient
	attach func()
}

func (a *attachDuringResolve) GetSession(ctx context.Context, sessionARN string,
	options ...session.LookupOption) (session.RemoteAccessInfo, error) {
	a.attach()
	return a.inner.GetSession(ctx, sessionARN, options...)
}

// failSecondLookup delegates the first GetSession and fails every later one, so a
// test can drive the error branch of the add's one re-resolve.
type failSecondLookup struct {
	inner session.RemoteAccessClient
	calls int
}

func (failing *failSecondLookup) GetSession(ctx context.Context, sessionARN string,
	options ...session.LookupOption) (session.RemoteAccessInfo, error) {
	failing.calls++
	if failing.calls > 1 {
		return session.RemoteAccessInfo{}, errors.New("second lookup failed for the test")
	}
	return failing.inner.GetSession(ctx, sessionARN, options...)
}
