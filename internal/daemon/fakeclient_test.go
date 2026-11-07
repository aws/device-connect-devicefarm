// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"sync"
	"time"

	"github.com/aws/device-connect-devicefarm/internal/session"
)

// testSessionStartedAt and testSessionExpiresAt are the session times every fake resolve carries.
var (
	testSessionStartedAt = time.Date(2026, 6, 26, 12, 0, 0, 0, time.UTC)
	testSessionExpiresAt = testSessionStartedAt.Add(150 * time.Minute)
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
// initial attach succeeded. It records each resolved ARN so a test can assert that
// the one-device short-circuit avoids the AWS call. Because the daemon depends on
// the application-layer RemoteAccessClient interface, this fake carries no
// aws-sdk-go-v2 types.
type fakeRemoteAccessClient struct {
	mu       sync.Mutex
	endpoint string
	label    string
	err      error
	arns     []string
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

// resolvedARNs returns the ARNs GetSession was called with, in order.
func (f *fakeRemoteAccessClient) resolvedARNs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.arns...)
}

func (f *fakeRemoteAccessClient) GetSession(_ context.Context, sessionARN string) (session.RemoteAccessInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.arns = append(f.arns, sessionARN)
	if f.err != nil {
		return session.RemoteAccessInfo{}, f.err
	}
	return session.RemoteAccessInfo{Endpoint: f.endpoint, Label: f.label,
		StartedAt: testSessionStartedAt, ExpiresAt: testSessionExpiresAt}, nil
}

// attachDuringResolve wraps a RemoteAccessClient and runs attach before
// delegating, modeling a concurrent caller binding the device while a resolve is
// in flight, so a test can drive the add handler's Set-fails defense branch.
type attachDuringResolve struct {
	inner  session.RemoteAccessClient
	attach func()
}

func (a *attachDuringResolve) GetSession(ctx context.Context, sessionARN string) (session.RemoteAccessInfo, error) {
	a.attach()
	return a.inner.GetSession(ctx, sessionARN)
}
