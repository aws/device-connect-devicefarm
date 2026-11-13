// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package doctor

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/websocket"

	"github.com/aws/device-connect-devicefarm/internal/bridge"
	"github.com/aws/device-connect-devicefarm/internal/config"
	"github.com/aws/device-connect-devicefarm/internal/control"
	"github.com/aws/device-connect-devicefarm/internal/session"
	"github.com/aws/device-connect-devicefarm/internal/testsupport"
)

const testSessionARN = "arn:aws:devicefarm:us-west-2:123456789012:session:abc/def/00000"

// fakeResolver answers GetSession with canned values and records the ARNs it was asked for.
type fakeResolver struct {
	info session.RemoteAccessInfo
	err  error
	arns []string
}

func (resolver *fakeResolver) GetSession(_ context.Context, sessionARN string) (session.RemoteAccessInfo, error) {
	resolver.arns = append(resolver.arns, sessionARN)
	return resolver.info, resolver.err
}

// runRemote runs the remote check named by id over state, defaults applied.
func runRemote(t *testing.T, inputs Inputs, state *runState, id string) Result {
	t.Helper()
	return checkByID(t, remoteChecks(withDefaults(inputs), state), id).Run(context.Background())
}

func TestGivenACredentialsSeamWhenTheCheckRunsThenItsOutcomeIsTheRow(t *testing.T) {
	inputs := Inputs{Credentials: func(context.Context) (session.CredentialSummary, error) {
		return session.CredentialSummary{Source: "SSOProvider"}, nil
	}}
	assert.Equal(t, Result{Status: Pass, Summary: "credentials from SSOProvider"}, runRemote(t, inputs, &runState{}, checkCredentials))

	inputs.Credentials = func(context.Context) (session.CredentialSummary, error) {
		return session.CredentialSummary{}, errors.New("no EC2 IMDS role found")
	}
	assert.Equal(t, Result{Status: Fail, Summary: "this shell has no usable AWS credentials: no EC2 IMDS role found", Hint: credentialsHint},
		runRemote(t, inputs, &runState{}, checkCredentials))
}

func TestGivenNoCredentialsSeamWhenTheCheckRunsThenTheRealChainIsWalked(t *testing.T) {
	testsupport.HermeticAWSEnv(t)

	result := runRemote(t, Inputs{}, &runState{}, checkCredentials)

	assert.Equal(t, Result{Status: Pass, Summary: "credentials from EnvConfigCredentials"}, result)
}

func TestGivenACredentialProcessWhoseExpiryHasPassedWhenTheCheckRunsThenTheRowNamesOnlyTheSource(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the credential_process line quotes for sh")
	}
	testsupport.HermeticAWSEnv(t)
	t.Setenv("AWS_ACCESS_KEY_ID", "")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "")
	expiration := time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
	processOutput := fmt.Sprintf(`{"Version":1,"AccessKeyId":"test","SecretAccessKey":"test","Expiration":"%s"}`, expiration)
	configFile := filepath.Join(t.TempDir(), "config")
	require.NoError(t, os.WriteFile(configFile, []byte("[default]\ncredential_process = echo '"+processOutput+"'\n"), 0o600))
	t.Setenv("AWS_CONFIG_FILE", configFile)

	result := runRemote(t, Inputs{}, &runState{}, checkCredentials)

	assert.Equal(t, Result{Status: Pass, Summary: "credentials from ProcessProvider"}, result)
}

func TestGivenNoSessionArnWhenTheSessionCheckRunsThenItSkipsNamingTheFlag(t *testing.T) {
	tests := []struct {
		name  string
		state func() *runState
	}{
		{name: "no daemon", state: func() *runState { return &runState{} }},
		{name: "a daemon with no device", state: func() *runState {
			state := &runState{}
			state.setDaemon(control.Response{OK: true, Port: 6000}, 42)
			return state
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resolver := &fakeResolver{}

			result := runRemote(t, Inputs{Resolver: resolver}, tc.state(), checkSession)

			assert.Equal(t, Result{Status: Skip, Summary: "pass --session-arn to check the session, the device connection and adb versions"}, result)
			assert.NotRegexp(t, `(local|aws|session|relay)\.[a-z-]+`, result.Summary, "no check id in a user row")
			assert.Empty(t, resolver.arns, "no ARN, no call")
		})
	}
}

func TestGivenNoSessionArnAndOneAttachedDeviceWhenTheSessionCheckRunsThenItResolvesTheAttachedSessionAndNamesIt(t *testing.T) {
	resolver := &fakeResolver{info: session.RemoteAccessInfo{Endpoint: testsupport.PresignedUnreachableEndpoint, Label: "Pixel 9 (OS 16)"}}
	state := &runState{}
	state.setDaemon(control.Response{OK: true, Port: 6000,
		Devices: []control.AttachedDevice{{SessionARN: testSessionARN, Label: "Pixel 9 (OS 16)", Serial: "localhost:6000"}}}, 42)

	result := runRemote(t, Inputs{Config: config.Default(), Resolver: resolver}, state, checkSession)

	assert.Equal(t, []string{testSessionARN}, resolver.arns, "the attached session is the one resolved")
	assert.Equal(t, Result{Status: Pass, Summary: "RUNNING: Pixel 9 (OS 16) (the session the daemon holds: " + testSessionARN + ")"}, result)
	assert.Equal(t, testsupport.PresignedUnreachableEndpoint, state.endpoint, "the endpoint stays in the state for the relay rows")
}

func TestGivenNoSessionArnAndSeveralAttachedDevicesWhenTheSessionCheckRunsThenItSkipsWithTheCount(t *testing.T) {
	resolver := &fakeResolver{}
	state := &runState{}
	state.setDaemon(control.Response{OK: true, Port: 6000, Devices: []control.AttachedDevice{
		{SessionARN: testSessionARN, Label: "Pixel 9 (OS 16)"},
		{SessionARN: testSessionARN + "1", Label: "Pixel 8 (OS 15)"}}}, 42)

	result := runRemote(t, Inputs{Resolver: resolver}, state, checkSession)

	assert.Equal(t, Result{Status: Skip, Summary: "the daemon holds 2 devices; pass --session-arn to pick the session to check"}, result)
	assert.Empty(t, resolver.arns, "several devices, no call")
}

func TestGivenASessionArnAndAnotherAttachedSessionWhenTheSessionCheckRunsThenTheFlagIsResolved(t *testing.T) {
	resolver := &fakeResolver{info: session.RemoteAccessInfo{Endpoint: testsupport.PresignedUnreachableEndpoint, Label: "Pixel 9 (OS 16)"}}
	state := &runState{}
	state.setDaemon(control.Response{OK: true, Port: 6000,
		Devices: []control.AttachedDevice{{SessionARN: testSessionARN + "1", Label: "Pixel 8 (OS 15)"}}}, 42)

	result := runRemote(t, Inputs{Config: config.Default(), SessionARN: testSessionARN, Resolver: resolver}, state, checkSession)

	assert.Equal(t, []string{testSessionARN}, resolver.arns, "the flag wins over the attached session")
	assert.Equal(t, Result{Status: Pass, Summary: "RUNNING: Pixel 9 (OS 16)"}, result, "a flagged session is not called the daemon's")
}

func TestGivenTheAttachedSessionDoesNotResolveFromThisShellWhenTheSessionCheckRunsThenItFailsWithTheHintForThatArn(t *testing.T) {
	resolveErr := fmt.Errorf("GetRemoteAccessSession: %w", session.ErrSessionNotFound)
	resolver := &fakeResolver{err: resolveErr}
	state := &runState{}
	state.setDaemon(control.Response{OK: true, Port: 6000,
		Devices: []control.AttachedDevice{{SessionARN: testSessionARN, Label: "Pixel 9 (OS 16)"}}}, 42)
	inputs := Inputs{Config: config.Default(), Resolver: resolver}

	result := runRemote(t, inputs, state, checkSession)

	effective := inputs
	effective.SessionARN = testSessionARN
	assert.Equal(t, Result{Status: Fail, Summary: resolveErr.Error(), Hint: sessionGoneHint(effective)}, result)
	assert.Contains(t, result.Hint, testSessionARN, "the hint quotes the attached ARN")
}

func TestGivenAnAttachedArnWithControlCharactersWhenTheSummaryIsBuiltThenItStaysOnOneLine(t *testing.T) {
	forgedARN := testSessionARN + "\n[PASS] forged: injected row\x1b[0m"
	resolver := &fakeResolver{info: session.RemoteAccessInfo{Endpoint: testsupport.PresignedUnreachableEndpoint, Label: "Pixel 9 (OS 16)"}}
	state := &runState{}
	state.setDaemon(control.Response{OK: true, Port: 6000,
		Devices: []control.AttachedDevice{{SessionARN: forgedARN, Label: "Pixel 9 (OS 16)"}}}, 42)

	result := runRemote(t, Inputs{Config: config.Default(), Resolver: resolver}, state, checkSession)

	assert.Equal(t, Pass, result.Status)
	assert.NotContains(t, result.Summary, "\n", "a row is one line")
	assert.NotContains(t, result.Summary, "\x1b", "no terminal control codes")
	assert.Contains(t, result.Summary, testSessionARN+`\n[PASS] forged: injected row\x1b[0m`, "the ARN is shown escaped, not dropped")
}

func TestGivenTheAttachedSessionWhenARelayRowFailsThenTheRerunQuotesItsArn(t *testing.T) {
	noProxy(t)
	isolateLocalAdb(t) // the version row finds no adb and its WARN hint quotes the rerun
	unreachable := func(*testing.T) (string, *bridge.WSDialer) {
		return testsupport.PresignedUnreachableEndpoint, bridge.NewWSDialer(time.Second)
	}
	tests := []struct {
		name       string
		id         string
		endpoint   func(t *testing.T) (string, *bridge.WSDialer)
		wantStatus Status
	}{
		{name: "the session row fails", id: checkRelaySession, endpoint: unreachable, wantStatus: Fail},
		{name: "the reattach row fails", id: checkReattach, endpoint: unreachable, wantStatus: Fail},
		{name: "the version row warns with no adb", id: checkAdbVersion, wantStatus: Warn,
			endpoint: func(t *testing.T) (string, *bridge.WSDialer) {
				sessions, _, dialer := relayMock(t, testsupport.SessionMockConfig{
					OnMessage: func(_ *testsupport.MockSession, data []byte) []byte {
						if string(data) == "000chost:version" {
							return []byte("OKAY00040029")
						}
						return data
					},
				})
				return sessions.URL, dialer
			}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			endpoint, dialer := tc.endpoint(t)
			inputs, state := relayInputs(endpoint, dialer)
			inputs.SessionARN = ""
			state.sessionUUID = "11111111-2222-4333-8444-000000000001"
			state.setDaemon(control.Response{OK: true, Port: 6000,
				Devices: []control.AttachedDevice{{SessionARN: testSessionARN, Label: "Pixel 9 (OS 16)"}}}, 42)

			result := runRemote(t, inputs, state, tc.id)

			assert.Equal(t, tc.wantStatus, result.Status, result.Summary)
			assert.Contains(t, result.Hint, "--session-arn "+testSessionARN, "the rerun quotes the attached session")
		})
	}
}

func TestGivenTheAttachedSessionAndReverseOnWhenTheReverseRowRunsThenItPassesWithNoProbe(t *testing.T) {
	sessions, reverse, dialer := relayMock(t, answeringAdb("OKAY"))
	inputs, state := relayInputs(sessions.URL, dialer)
	inputs.SessionARN = ""
	state.setDaemon(control.Response{OK: true, Port: 6000, ARN: testSessionARN, ReverseEnabled: true,
		Devices: []control.AttachedDevice{{SessionARN: testSessionARN, Label: "Pixel 9 (OS 16)"}}}, 42)

	result := runRemote(t, inputs, state, checkReverse)

	assert.Equal(t, Result{Status: Pass, Summary: reverseOnSummary}, result)
	assert.Zero(t, reverse.NotifiersAccepted(), "no probe beside a reverse-on daemon on the attached session")
}

func TestGivenAResolverWhenTheSessionCheckRunsThenTheRowFollowsTheResolverOutcome(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		wantHint func(inputs Inputs) string
		// wantQuoted is the command or setting the hint must quote.
		wantQuoted string
	}{
		{name: "running"},
		{name: "invalid arn", err: fmt.Errorf("%w: parse session ARN", session.ErrInvalidARN), wantHint: invalidArnHint,
			wantQuoted: "'" + createSessionCommand + "'"},
		{name: "access denied", err: fmt.Errorf("GetRemoteAccessSession: %w", session.ErrAccessDenied), wantHint: accessDeniedHint,
			wantQuoted: "'devicefarm:GetRemoteAccessSession'"},
		{name: "not found", err: fmt.Errorf("GetRemoteAccessSession: %w", session.ErrSessionNotFound), wantHint: sessionGoneHint,
			wantQuoted: "'aws devicefarm list-remote-access-sessions --arn <project ARN> --region us-west-2'"},
		{name: "unsupported device", err: session.UnsupportedDeviceError{SessionARN: testSessionARN, Platform: "Android", OSVersion: "8.0.0"},
			wantHint: unsupportedDeviceHint, wantQuoted: "'--session-arn'"},
		{name: "unsupported device, untyped", err: fmt.Errorf("resolve: %w", session.ErrUnsupportedDevice),
			wantHint: unsupportedDeviceHint, wantQuoted: "'--session-arn'"},
		{name: "ios device", err: session.UnsupportedDeviceError{SessionARN: testSessionARN, Platform: "iOS", OSVersion: "17.0"},
			wantHint: func(inputs Inputs) string { return nonAndroidDeviceHint(inputs, "iOS") }, wantQuoted: "'" + createSessionCommand + "'"},
		{name: "unknown platform", err: session.UnsupportedDeviceError{SessionARN: testSessionARN, Platform: "WATCH", OSVersion: "1.0"},
			wantHint: func(inputs Inputs) string { return nonAndroidDeviceHint(inputs, "WATCH") }, wantQuoted: "'" + createSessionCommand + "'"},
		{name: "ended", err: fmt.Errorf("session %w (status COMPLETED)", session.ErrSessionEnded), wantHint: sessionEndedHint,
			wantQuoted: "'" + createSessionCommand + "'"},
		{name: "not running yet", err: fmt.Errorf("session %w (status PENDING)", session.ErrNoEndpointYet), wantHint: notRunningHint,
			wantQuoted: "'aws devicefarm get-remote-access-session --arn " + testSessionARN + " --region us-west-2'"},
		{name: "rejected", err: fmt.Errorf("GetRemoteAccessSession: %w", &session.RequestRejectedError{Code: "ExpiredTokenException",
			Cause: errors.New("api error ExpiredTokenException: The security token included in the request is expired")}),
			wantHint: requestRejectedHint, wantQuoted: "'aws sts get-caller-identity'"},
		{name: "network", err: errors.New("GetRemoteAccessSession: dial tcp: connection refused"), wantHint: sessionNetworkHint,
			wantQuoted: "'devicefarm.us-west-2.amazonaws.com'"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resolver := &fakeResolver{err: tc.err,
				info: session.RemoteAccessInfo{Endpoint: testsupport.PresignedUnreachableEndpoint, Label: "Pixel 9 (OS 16)"}}
			state := &runState{}
			inputs := Inputs{Config: config.Default(), SessionARN: testSessionARN, Resolver: resolver}

			result := runRemote(t, inputs, state, checkSession)

			assert.Equal(t, []string{testSessionARN}, resolver.arns)
			if tc.wantHint == nil {
				assert.Empty(t, result.Hint)
			} else {
				assert.Equal(t, tc.wantHint(inputs), result.Hint)
				assert.Contains(t, result.Hint, testSessionARN, "the hint names the ARN that was passed")
				assert.Contains(t, result.Hint, tc.wantQuoted, "the hint quotes the command or setting")
			}
			assert.NotContains(t, result.Summary+result.Hint, "X-Amz-", "the endpoint never reaches the row")
			assert.NotContains(t, result.Summary+result.Hint, testsupport.SignatureMarker)
			if tc.err == nil {
				assert.Equal(t, Result{Status: Pass, Summary: "RUNNING: Pixel 9 (OS 16)"}, result)
				assert.Equal(t, testsupport.PresignedUnreachableEndpoint, state.endpoint, "the endpoint stays in the state")
			} else {
				assert.Equal(t, Fail, result.Status)
				assert.Equal(t, tc.err.Error(), result.Summary)
				assert.Empty(t, state.endpoint, "a failed resolve leaves no endpoint for later rows")
			}
		})
	}
}

func TestGivenNoResolverAndAMissingProfileWhenTheSessionCheckRunsThenTheClientBuildFails(t *testing.T) {
	testsupport.HermeticAWSEnv(t)
	t.Setenv("AWS_PROFILE", "no-such-profile")

	result := runRemote(t, Inputs{SessionARN: testSessionARN}, &runState{}, checkSession)

	assert.Equal(t, Fail, result.Status)
	assert.Contains(t, result.Summary, "load AWS config")
	assert.Equal(t, credentialsHint, result.Hint)
}

func TestGivenNoResolverWhenTheSessionCheckRunsThenTheRealClientIsBuiltAndCalls(t *testing.T) {
	testsupport.HermeticAWSEnv(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	closedAddress := listener.Addr().String()
	require.NoError(t, listener.Close())
	t.Setenv("AWS_ENDPOINT_URL_DEVICE_FARM", "http://"+closedAddress)

	inputs := Inputs{Config: config.Default(), SessionARN: testSessionARN}

	result := runRemote(t, inputs, &runState{}, checkSession)

	assert.Equal(t, Fail, result.Status)
	assert.Contains(t, result.Summary, "connection refused", "the real client dialed the endpoint")
	assert.Equal(t, sessionNetworkHint(inputs), result.Hint)
}

func TestGivenTheRunFlagsWhenTheRerunIsQuotedThenItKeepsThePortAndANonDefaultListenAddress(t *testing.T) {
	inputs := Inputs{Config: config.Default(), SessionARN: testSessionARN}
	inputs.Config.ListenPort = 6000

	assert.Equal(t, "'"+doctorCommand+" --session-arn "+testSessionARN+" --port 6000'", doctorRerun(inputs))
	inputs.Config.ListenAddr = "::1"
	assert.Equal(t, "'"+doctorCommand+" --session-arn "+testSessionARN+" --port 6000 --listen-addr ::1'", doctorRerun(inputs))
	inputs.Config.ListenAddr = "::1\n[PASS] forged"
	assert.Equal(t, "'"+doctorCommand+" --session-arn "+testSessionARN+` --port 6000 --listen-addr ::1\n[PASS] forged'`, doctorRerun(inputs),
		"a listen address with a newline is escaped, so the row stays one line")
}

func TestGivenAnArnWithControlCharactersWhenAHintIsBuiltThenTheHintStaysOnOneLine(t *testing.T) {
	inputs := Inputs{Config: config.Default(), SessionARN: testSessionARN + "\n[PASS] forged: injected row\x1b[0m"}
	builders := map[string]func(Inputs) string{"invalid arn": invalidArnHint, "access denied": accessDeniedHint,
		"not found": sessionGoneHint, "unsupported device": unsupportedDeviceHint, "ended": sessionEndedHint,
		"ios device":      func(inputs Inputs) string { return nonAndroidDeviceHint(inputs, "iOS") },
		"not running yet": notRunningHint, "rejected": requestRejectedHint, "network": sessionNetworkHint, "rerun": doctorRerun}
	for name, build := range builders {
		t.Run(name, func(t *testing.T) {
			hint := build(inputs)

			assert.NotContains(t, hint, "\n", "a row is one line")
			assert.NotContains(t, hint, "\x1b", "no terminal control codes")
			assert.Contains(t, hint, testSessionARN+`\n[PASS] forged: injected row\x1b[0m`, "the ARN is shown escaped, not dropped")
		})
	}
}

// noProxy clears the proxy variables relayHint reads, so the host's own setting cannot leak in.
func noProxy(t *testing.T) {
	t.Helper()
	t.Setenv("HTTPS_PROXY", "")
	t.Setenv("https_proxy", "")
}

// relayMock serves both routes under cfg, with a dialer that trusts its CA.
func relayMock(t *testing.T, cfg testsupport.SessionMockConfig) (*testsupport.SessionMockWSS, *testsupport.ReverseMockWSS, *bridge.WSDialer) {
	t.Helper()
	noProxy(t)
	sessions, reverse := testsupport.NewServerMockWSS(t, cfg)
	return sessions, reverse, bridge.NewWSDialer(time.Second).TrustRoots(testsupport.TestRoots())
}

// answeringAdb makes the session side answer every adb request with reply.
func answeringAdb(reply string) testsupport.SessionMockConfig {
	return testsupport.SessionMockConfig{OnMessage: func(*testsupport.MockSession, []byte) []byte { return []byte(reply) }}
}

// relayInputs seeds the run state as if session.resolve passed.
func relayInputs(endpoint string, dialer *bridge.WSDialer) (Inputs, *runState) {
	return Inputs{Config: config.Default(), SessionARN: testSessionARN, Dialer: dialer}, &runState{endpoint: endpoint}
}

func TestGivenARelayThatAcceptsTheSelectWhenTheSessionIsProbedThenTheRowNamesTheMintedSession(t *testing.T) {
	sessions, _, dialer := relayMock(t, answeringAdb("OKAY"))
	inputs, state := relayInputs(sessions.URL, dialer)

	result := runRemote(t, inputs, state, checkRelaySession)

	require.Equal(t, Pass, result.Status, result.Summary)
	require.Len(t, sessions.Sessions(), 1)
	assert.Equal(t, sessions.Sessions()[0].UUID, state.sessionUUID, "the minted session is kept for the reattach row")
	assert.Equal(t, fmt.Sprintf("adb session %s opened to the device", state.sessionUUID), result.Summary)
}

func TestGivenTheDeviceRefusesTheSelectWhenTheSessionIsProbedThenTheRowFailsWithTheRelayHint(t *testing.T) {
	sessions, _, dialer := relayMock(t, answeringAdb("FAIL000edevice offline"))
	inputs, state := relayInputs(sessions.URL, dialer)

	result := runRemote(t, inputs, state, checkRelaySession)

	assert.Equal(t, Result{Status: Fail,
		Summary: "could not open an adb session to the device: the device adb server refused the transport select: device offline",
		Hint:    relayHint(inputs)}, result)
	assert.Empty(t, state.sessionUUID)
}

func TestGivenAPresignedEndpointNobodyAnswersWhenEachRelayRowRunsThenTheRowCarriesNoCredential(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://proxy.example:3128")
	isolateLocalAdb(t) // no adb: the version row needs no local step to fail on the probe
	dialer := bridge.NewWSDialer(time.Second)
	for _, id := range []string{checkRelaySession, checkReattach, checkReverse, checkAdbVersion} {
		t.Run(id, func(t *testing.T) {
			inputs, state := relayInputs(testsupport.PresignedUnreachableEndpoint, dialer)
			state.sessionUUID = "11111111-2222-4333-8444-000000000001"

			result := runRemote(t, inputs, state, id)

			assert.Equal(t, Fail, result.Status)
			assert.Contains(t, result.Summary, testsupport.ScrubbedDialText)
			for _, marker := range []string{"X-Amz-", testsupport.SignatureMarker, testsupport.SecurityTokenMarker} {
				assert.NotContains(t, result.Summary+result.Hint, marker)
			}
			switch id {
			case checkAdbVersion:
				assert.Equal(t, versionProbeHint, result.Hint, "the version row's hint quotes no rerun and names no proxy")
			case checkReattach:
				assert.Equal(t, reattachHint(inputs), result.Hint)
			default:
				assert.Contains(t, result.Hint, doctorRerun(inputs), "the hint quotes the rerun for this session")
				assert.Contains(t, result.Hint, proxyIgnoredNote, "a set proxy is named as ignored")
			}
		})
	}
}

func TestGivenNoProxyWhenTheRelayDialFailsThenTheHintCarriesNoProxyNote(t *testing.T) {
	noProxy(t)
	inputs, state := relayInputs(testsupport.PresignedUnreachableEndpoint, bridge.NewWSDialer(time.Second))

	result := runRemote(t, inputs, state, checkRelaySession)

	assert.Equal(t, Fail, result.Status)
	assert.Equal(t, relayHint(inputs), result.Hint)
	assert.NotContains(t, result.Hint, proxyIgnoredNote)
}

func TestGivenAProbedSessionWhenReattachedThenTheRowNamesTheResumedSession(t *testing.T) {
	sessions, _, dialer := relayMock(t, answeringAdb("OKAY"))
	inputs, state := relayInputs(sessions.URL, dialer)
	require.Equal(t, Pass, runRemote(t, inputs, state, checkRelaySession).Status)

	result := runRemote(t, inputs, state, checkReattach)

	assert.Equal(t, Result{Status: Pass, Summary: "reconnected to adb session " + state.sessionUUID}, result)
	assert.Equal(t, 1, sessions.Reattaches())
}

func TestGivenASilentRelayWhenTheRowDeadlineNearsThenTheProbeGivesUpFirstAndKeepsTheHint(t *testing.T) {
	noProxy(t)
	isolateLocalAdb(t) // no adb, so the version row's local step is instant
	parked := make(chan struct{})
	t.Cleanup(func() { close(parked) })
	silent := testsupport.NewMockWSS(t, func(*websocket.Conn) { <-parked })
	rows := []struct {
		id      string
		hintFor func(Inputs) string
	}{
		{checkRelaySession, relayHint},
		{checkAdbVersion, func(Inputs) string { return versionProbeHint }},
	}
	for _, row := range rows {
		t.Run(row.id, func(t *testing.T) {
			inputs, state := relayInputs(silent, bridge.NewWSDialer(time.Second).TrustRoots(testsupport.TestRoots()))
			ctx, cancel := context.WithTimeout(context.Background(), probeMargin+500*time.Millisecond)
			defer cancel()

			result := checkByID(t, remoteChecks(withDefaults(inputs), state), row.id).Run(ctx)

			assert.NoError(t, ctx.Err(), "the probe must return before the row's deadline")
			assert.Equal(t, Fail, result.Status)
			// The probe's own error, whichever of its read bound or its socket close fired first.
			assert.NotContains(t, result.Summary, "timed out after", "the runner's timeout text must not replace the probe's error")
			assert.Equal(t, row.hintFor(inputs), result.Hint, "the row keeps its hint")
		})
	}
}

func TestGivenARelayThatForgetsSessionsWhenReattachedThenTheRowFailsWithTheRerunHint(t *testing.T) {
	sessions, _, dialer := relayMock(t, testsupport.SessionMockConfig{RejectReattach: true})
	inputs, state := relayInputs(sessions.URL, dialer)
	state.sessionUUID = "11111111-2222-4333-8444-000000000001"

	result := runRemote(t, inputs, state, checkReattach)

	assert.Equal(t, Fail, result.Status)
	assert.Contains(t, result.Summary, "gone")
	assert.Equal(t, reattachHint(inputs), result.Hint)
}

func TestGivenTheReverseSlotWhenTheRowRunsThenTheRowFollowsWhoHoldsIt(t *testing.T) {
	const attachedARN = testSessionARN
	tests := []struct {
		name        string
		daemon      control.Response
		rival       bool
		wantStatus  Status
		wantText    string
		wantHint    func(inputs Inputs) string
		wantNoProbe bool
	}{
		{name: "free slot, no daemon", wantStatus: Pass, wantText: reversePassSummary},
		{name: "free slot, daemon on another session with reverse on",
			daemon: control.Response{OK: true, ARN: "arn:other", ReverseEnabled: true}, wantStatus: Pass, wantText: reversePassSummary},
		{name: "free slot, daemon on this session with reverse off",
			daemon: control.Response{OK: true, ARN: attachedARN}, wantStatus: Pass, wantText: reversePassSummary},
		{name: "daemon on this session with reverse on, ARN with stray space",
			daemon: control.Response{OK: true, ARN: " " + attachedARN, ReverseEnabled: true}, wantStatus: Pass, wantText: reverseOnSummary,
			wantNoProbe: true},
		{name: "slot held elsewhere", rival: true, wantStatus: Fail, wantText: reverseHeldSummary, wantHint: slotHeldHint},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sessions, reverse, dialer := relayMock(t, answeringAdb("OKAY"))
			if tc.rival {
				t.Cleanup(reverse.HoldNotifierSlot())
			}
			inputs, state := relayInputs(sessions.URL, dialer)
			state.daemonStatus = tc.daemon

			result := runRemote(t, inputs, state, checkReverse)

			assert.Equal(t, tc.wantStatus, result.Status, result.Summary)
			if tc.wantHint == nil {
				assert.Empty(t, result.Hint)
			} else {
				assert.Equal(t, tc.wantHint(inputs), result.Hint)
				assert.Contains(t, result.Hint, "'"+bridgeStopCommand+"'", "the hint quotes the stop for the other daemon")
				assert.Contains(t, result.Hint, doctorRerun(inputs))
			}
			if tc.wantStatus == Fail {
				assert.Contains(t, result.Summary, tc.wantText)
			} else {
				assert.Equal(t, tc.wantText, result.Summary)
			}
			switch {
			case tc.wantNoProbe:
				assert.Zero(t, reverse.NotifiersAccepted(), "no probe beside a reverse-on daemon")
			case tc.wantStatus == Pass:
				require.Eventually(t, func() bool { return reverse.NotifiersClosed() >= 1 }, time.Second, 10*time.Millisecond,
					"the probe must release the slot")
			}
		})
	}
}

const (
	adbOutput39 = "Android Debug Bridge version 1.0.39\nRevision 3db08f2c6889-android\n"
	adbOutput40 = "Android Debug Bridge version 1.0.40\n"
	adbOutput41 = "Android Debug Bridge version 1.0.41\nVersion 35.0.2-12147458\n"
	// Hint fragments the tests share.
	skewConsequence  = ". Until then no adb command reaches the device. The remote adb server is not affected."
	noAdbConsequence = ". A local adb on another version cannot connect through the daemon."
	noAdbCause       = "adb was not found on PATH or in an Android SDK ('ANDROID_HOME', 'ANDROID_SDK_ROOT' or Android Studio's " +
		"default SDK location), so the doctor could not check that its version matches the remote adb server (1.0.41)"
	sdkManagerFix = "Update Android SDK Platform-Tools in Android Studio's SDK Manager to a release whose adb is 1.0.41, such as " +
		"platform-tools 37"
)

// versionMock answers host:version with reply; relay.session's state is seeded.
func versionMock(t *testing.T, reply string) (Inputs, *runState) {
	t.Helper()
	sessions, _, dialer := relayMock(t, testsupport.SessionMockConfig{
		OnMessage: func(_ *testsupport.MockSession, data []byte) []byte {
			if string(data) == "000chost:version" {
				return []byte(reply)
			}
			return data
		},
	})
	return relayInputs(sessions.URL, dialer)
}

func TestGivenALocalAdbOrNoneWhenTheVersionIsCheckedThenTheRowComparesItWithTheRemoteAdbServer(t *testing.T) {
	rerun := doctorRerun(Inputs{Config: config.Default(), SessionARN: testSessionARN})
	tests := []struct {
		name       string
		adbOutput  string
		noAdb      bool
		hostReply  string
		wantStatus Status
		wantText   string
		wantHint   string
	}{
		{name: "matching versions", adbOutput: adbOutput41, hostReply: "OKAY00040029",
			wantStatus: Pass, wantText: "local adb 1.0.41 matches the remote adb server"},
		{name: "an older local adb", adbOutput: adbOutput39, hostReply: "OKAY00040029",
			wantStatus: Fail, wantText: "local adb 1.0.39 differs from the remote adb server (1.0.41); every adb command fails",
			wantHint: "the local adb is 1.0.39 and the remote adb server is 1.0.41, so every adb command fails: that adb tries to start " +
				"its own adb server on the daemon's port and cannot. " +
				"Install platform-tools whose adb is 1.0.41, such as platform-tools 37, or put such an adb first on PATH, then rerun " + rerun + skewConsequence},
		{name: "a newer local adb than a host with no known platform-tools", adbOutput: adbOutput41, hostReply: "OKAY00040027",
			wantStatus: Fail, wantText: "local adb 1.0.41 differs from the remote adb server (1.0.39); every adb command fails",
			wantHint: "the local adb is 1.0.41 and the remote adb server is 1.0.39, so every adb command fails: that adb tries to start " +
				"its own adb server on the daemon's port and cannot. " +
				"Install platform-tools whose adb is 1.0.39, or put such an adb first on PATH, then rerun " + rerun + skewConsequence},
		{name: "no adb anywhere", noAdb: true, hostReply: "OKAY00040029", wantStatus: Warn,
			wantText: "no adb found on PATH or in an Android SDK, so its version was not checked against the remote adb server (1.0.41)",
			wantHint: noAdbCause + ". Install Android platform-tools whose adb is 1.0.41, such as platform-tools 37, then add its directory " +
				"to PATH and rerun " + rerun + noAdbConsequence},
		{name: "no adb anywhere and a host with no known platform-tools", noAdb: true, hostReply: "OKAY00040027", wantStatus: Warn,
			wantText: "no adb found on PATH or in an Android SDK, so its version was not checked against the remote adb server (1.0.39)",
			wantHint: strings.Replace(noAdbCause, "1.0.41", "1.0.39", 1) + ". Install Android platform-tools whose adb is 1.0.39, then add " +
				"its directory to PATH and rerun " + rerun + noAdbConsequence},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.noAdb {
				isolateLocalAdb(t)
			} else {
				fakeAdbOnPath(t, fakeAdbSpec{Output: tc.adbOutput})
			}
			inputs, state := versionMock(t, tc.hostReply)

			result := runRemote(t, inputs, state, checkAdbVersion)

			assert.Equal(t, Result{Status: tc.wantStatus, Summary: tc.wantText, Hint: tc.wantHint}, result)
		})
	}
}

func TestGivenSdkAdbsBesideThePathAdbWhenTheVersionIsCheckedThenEveryFoundAdbCounts(t *testing.T) {
	// want's texts take %[1]s the PATH adb's path, %[2]s the SDK adb's path, %[3]s the quoted rerun.
	const studioClause = "; Android Studio runs the SDK's adb at '%[2]s', so Studio lists no device while that adb is on another version"
	const thatAdbConsequence = ". Until then no command from that adb reaches the device. The remote adb server is not affected."
	pathAndSdk := func(pathOutput, sdkOutput string) func(t *testing.T) (string, string) {
		return func(t *testing.T) (string, string) {
			return fakeAdbOnPath(t, fakeAdbSpec{Output: pathOutput}), installFakeAdb(t, sdkPlatformTools(t, "ANDROID_HOME"), fakeAdbSpec{Output: sdkOutput})
		}
	}
	sdkOnly := func(variable, sdkOutput string) func(t *testing.T) (string, string) {
		return func(t *testing.T) (string, string) {
			isolateLocalAdb(t)
			return "", installFakeAdb(t, sdkPlatformTools(t, variable), fakeAdbSpec{Output: sdkOutput})
		}
	}
	defaultPlatformTools := func() string { return filepath.Join(defaultSDKRoot(runtime.GOOS), "platform-tools") }
	tests := []struct {
		name  string
		setup func(t *testing.T) (pathAdb, sdkAdb string)
		want  Result
	}{
		{name: "PATH and ANDROID_HOME agree", setup: pathAndSdk(adbOutput41, adbOutput41),
			want: Result{Status: Pass, Summary: "local adb 1.0.41 (2 found) matches the remote adb server"}},
		{name: "the SDK adb differs while the PATH adb matches", setup: pathAndSdk(adbOutput41, adbOutput39),
			want: Result{Status: Fail,
				Summary: "local adb 1.0.39 at '%[2]s' differs from the remote adb server (1.0.41); every command from that adb fails",
				Hint: "Android Studio's adb at '%[2]s' is 1.0.39 and the remote adb server is 1.0.41, so every command from that adb fails: that " +
					"adb tries to start its own adb server on the daemon's port and cannot. " + sdkManagerFix + ", then rerun %[3]s" + thatAdbConsequence}},
		{name: "no PATH adb and the ANDROID_SDK_ROOT adb matches", setup: sdkOnly("ANDROID_SDK_ROOT", adbOutput41),
			want: Result{Status: Pass, Summary: "local adb 1.0.41 matches the remote adb server"}},
		{name: "no PATH adb and the SDK adb differs", setup: sdkOnly("ANDROID_HOME", adbOutput39),
			want: Result{Status: Fail,
				Summary: "local adb 1.0.39 at '%[2]s' differs from the remote adb server (1.0.41); every command from that adb fails",
				Hint: "Android Studio's adb at '%[2]s' is 1.0.39 and the remote adb server is 1.0.41, so every command from that adb fails: that " +
					"adb tries to start its own adb server on the daemon's port and cannot. " + sdkManagerFix + ", then rerun %[3]s" + thatAdbConsequence}},
		{name: "the PATH adb and the SDK adb both differ", setup: pathAndSdk(adbOutput39, adbOutput40),
			want: Result{Status: Fail,
				Summary: "local adb 1.0.39 at '%[1]s' and local adb 1.0.40 at '%[2]s' differ from the remote adb server (1.0.41); every command " +
					"from any of them fails",
				Hint: "the local adb at '%[1]s' is 1.0.39, Android Studio's adb at '%[2]s' is 1.0.40 and the remote adb server is 1.0.41, so " +
					"every command from any of them fails: that adb tries to start its own adb server on the daemon's port and cannot. " +
					sdkManagerFix + ", and put such an adb first on PATH, then rerun " +
					"%[3]s. Until then no command from any of them reaches the device. The remote adb server is not affected."}},
		{name: "one binary reached through PATH and ANDROID_HOME counts once",
			setup: func(t *testing.T) (string, string) {
				isolateLocalAdb(t)
				platformTools := sdkPlatformTools(t, "ANDROID_HOME")
				t.Setenv("PATH", platformTools)
				adb := installFakeAdb(t, platformTools, fakeAdbSpec{Output: adbOutput39})
				return adb, adb
			},
			want: Result{Status: Fail,
				Summary: "local adb 1.0.39 differs from the remote adb server (1.0.41); every adb command fails",
				Hint: "the local adb is 1.0.39 and the remote adb server is 1.0.41, so every adb command fails: that adb tries to start its " +
					"own adb server on the daemon's port and cannot" + studioClause +
					". " + sdkManagerFix + ", then rerun %[3]s" + skewConsequence}},
		{name: "a PATH symlink to the SDK adb counts once",
			setup: func(t *testing.T) (string, string) {
				if runtime.GOOS == "windows" {
					t.Skip("symlinks need a privilege on Windows")
				}
				isolateLocalAdb(t)
				sdkAdb := installFakeAdb(t, sdkPlatformTools(t, "ANDROID_HOME"), fakeAdbSpec{Output: adbOutput39})
				binDir := t.TempDir()
				require.NoError(t, os.Symlink(sdkAdb, filepath.Join(binDir, "adb")))
				t.Setenv("PATH", binDir)
				return filepath.Join(binDir, "adb"), sdkAdb
			},
			want: Result{Status: Fail,
				Summary: "local adb 1.0.39 differs from the remote adb server (1.0.41); every adb command fails",
				Hint: "the local adb is 1.0.39 and the remote adb server is 1.0.41, so every adb command fails: that adb tries to start its " +
					"own adb server on the daemon's port and cannot" + studioClause +
					". " + sdkManagerFix + ", then rerun %[3]s" + skewConsequence}},
		{name: "a broken SDK adb beside a matching PATH adb",
			setup: func(t *testing.T) (string, string) {
				return fakeAdbOnPath(t, fakeAdbSpec{Output: adbOutput41}), installFakeAdb(t, sdkPlatformTools(t, "ANDROID_HOME"), fakeAdbSpec{ExitCode: 3})
			},
			want: Result{Status: Warn, Summary: "'%[2]s version' failed: exit status 3",
				Hint: strings.NewReplacer("%[1]s", "'%[2]s version'", "%[2]s", "%[3]s", "%[3]d", "41").Replace(adbFailedHint)}},
		{name: "the default SDK location when neither variable is set",
			setup: func(t *testing.T) (string, string) {
				isolateLocalAdb(t)
				return "", installFakeAdb(t, defaultPlatformTools(), fakeAdbSpec{Output: adbOutput41})
			},
			want: Result{Status: Pass, Summary: "local adb 1.0.41 matches the remote adb server"}},
		{name: "a PATH adb reached only through the current directory beside a matching SDK adb",
			setup: func(t *testing.T) (string, string) {
				isolateLocalAdb(t)
				sdkAdb := installFakeAdb(t, sdkPlatformTools(t, "ANDROID_HOME"), fakeAdbSpec{Output: adbOutput41})
				dir := t.TempDir()
				hiddenAdb := installFakeAdb(t, dir, fakeAdbSpec{Output: adbOutput39})
				t.Chdir(dir)
				t.Setenv("PATH", ".")
				return hiddenAdb, sdkAdb
			},
			want: Result{Status: Warn,
				Summary: "local adb 1.0.41 matches the remote adb server, but an adb at '%[1]s' reached only through the current directory or a " +
					"relative PATH entry was not checked",
				Hint: "an adb exists at '%[1]s', reached only through the current directory or a relative PATH entry, so it was not run and its " +
					"version is unknown. Add that directory to PATH by its absolute path if you meant that adb, then rerun %[3]s" + noAdbConsequence}},
		{name: "the adb reached only through the current directory is the SDK adb that ran",
			setup: func(t *testing.T) (string, string) {
				isolateLocalAdb(t)
				platformTools := sdkPlatformTools(t, "ANDROID_HOME")
				adb := installFakeAdb(t, platformTools, fakeAdbSpec{Output: adbOutput41})
				t.Chdir(platformTools)
				t.Setenv("PATH", ".")
				return adb, adb
			},
			want: Result{Status: Pass, Summary: "local adb 1.0.41 matches the remote adb server"}},
		{name: "ANDROID_HOME hides the default SDK location",
			setup: func(t *testing.T) (string, string) {
				pathAdb := fakeAdbOnPath(t, fakeAdbSpec{Output: adbOutput41})
				installFakeAdb(t, defaultPlatformTools(), fakeAdbSpec{Output: adbOutput39})
				return pathAdb, sdkPlatformTools(t, "ANDROID_HOME") // an SDK with no platform-tools
			},
			want: Result{Status: Pass, Summary: "local adb 1.0.41 matches the remote adb server"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pathAdb, sdkAdb := tc.setup(t)
			inputs, state := versionMock(t, "OKAY00040029")

			result := runRemote(t, inputs, state, checkAdbVersion)

			fill := func(text string) string {
				if !strings.Contains(text, "%") {
					return text
				}
				return fmt.Sprintf(text, pathAdb, sdkAdb, doctorRerun(inputs))
			}
			assert.Equal(t, Result{Status: tc.want.Status, Summary: fill(tc.want.Summary), Hint: fill(tc.want.Hint)}, result)
		})
	}
}

func TestGivenHangingAdbsOnPathAndInTheSdkWhenTheVersionIsCheckedThenOneBoundCoversBoth(t *testing.T) {
	pathAdb := fakeAdbOnPath(t, fakeAdbSpec{Output: adbOutput41, HangFor: time.Minute})
	sdkAdb := installFakeAdb(t, sdkPlatformTools(t, "ANDROID_HOME"), fakeAdbSpec{Output: adbOutput41, HangFor: time.Minute})
	inputs, state := versionMock(t, "OKAY00040029")
	started := time.Now()

	result := runRemote(t, inputs, state, checkAdbVersion)

	assert.Less(t, time.Since(started), adbVersionTimeout+pipeDrainDelay+2*time.Second, "both adbs run under the one bound")
	assert.Equal(t, Result{Status: Warn,
		Summary: "'" + pathAdb + " version' did not finish in 5s; '" + sdkAdb + " version' did not finish in 5s",
		Hint:    fmt.Sprintf(adbFailedHint, "'"+pathAdb+" version'", doctorRerun(inputs), 41)}, result)
}

func TestGivenTheSdkVariablesWhenCandidatesAreListedThenTheOsDefaultFillsInOnlyWhenBothAreUnset(t *testing.T) {
	home, local, sdkHome, sdkRoot := t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir()
	tests := []struct {
		name string
		goos string
		env  map[string]string
		want []string
	}{
		{name: "Linux default", goos: "linux", want: []string{filepath.Join(home, "Android", "Sdk", "platform-tools", "adb")}},
		{name: "macOS default", goos: "darwin", want: []string{filepath.Join(home, "Library", "Android", "sdk", "platform-tools", "adb")}},
		{name: "Windows default", goos: "windows", want: []string{filepath.Join(local, "Android", "Sdk", "platform-tools", "adb.exe")}},
		{name: "Windows without LOCALAPPDATA", goos: "windows", env: map[string]string{"LOCALAPPDATA": ""}, want: nil},
		{name: "Linux without a home", goos: "linux", env: map[string]string{"HOME": "", "USERPROFILE": ""}, want: nil},
		{name: "ANDROID_HOME alone", goos: "linux", env: map[string]string{"ANDROID_HOME": sdkHome},
			want: []string{filepath.Join(sdkHome, "platform-tools", "adb")}},
		{name: "both variables, in order", goos: "windows", env: map[string]string{"ANDROID_HOME": sdkHome, "ANDROID_SDK_ROOT": sdkRoot},
			want: []string{filepath.Join(sdkHome, "platform-tools", "adb.exe"), filepath.Join(sdkRoot, "platform-tools", "adb.exe")}},
		{name: "a relative root is skipped and hides the default", goos: "linux", env: map[string]string{"ANDROID_SDK_ROOT": "sdk"}, want: nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// HOME serves Linux and macOS, USERPROFILE a Windows host's os.UserHomeDir.
			for variable, value := range map[string]string{"HOME": home, "USERPROFILE": home, "LOCALAPPDATA": local, "ANDROID_HOME": "", "ANDROID_SDK_ROOT": ""} {
				t.Setenv(variable, value)
			}
			for variable, value := range tc.env {
				t.Setenv(variable, value)
			}

			assert.Equal(t, tc.want, sdkAdbCandidates(tc.goos))
		})
	}
}

func TestGivenAnAdbInARelativePathEntryWhenTheVersionIsCheckedThenTheRowWarnsWithoutRunningIt(t *testing.T) {
	isolateLocalAdb(t)
	dir := t.TempDir()
	marker := filepath.Join(dir, "ran")
	adb := installFakeAdb(t, dir, fakeAdbSpec{Output: adbOutput39, Marker: marker})
	t.Chdir(dir)
	t.Setenv("PATH", ".")
	inputs, state := versionMock(t, "OKAY00040029")

	result := runRemote(t, inputs, state, checkAdbVersion)

	assert.Equal(t, Result{Status: Warn,
		Summary: "no adb found on PATH or in an Android SDK, so its version was not checked against the remote adb server (1.0.41)",
		Hint: noAdbCause + "; an adb exists at '" + adb + "', reached only through the current directory or a relative PATH entry, so it was " +
			"not run. Add that directory to PATH by its absolute path if you meant that adb, or install Android platform-tools whose adb is " +
			"1.0.41, such as platform-tools 37, then add its directory to PATH and rerun " + doctorRerun(inputs) + noAdbConsequence}, result)
	assert.NoFileExists(t, marker, "a binary found through the current dir must not run")
}

func TestGivenARemoteAdbServerThatRefusesHostVersionWhenTheVersionIsCheckedThenTheRowFailsWithTheSupportHint(t *testing.T) {
	isolateLocalAdb(t) // no adb: the verdict is the probe's alone
	inputs, state := versionMock(t, "FAIL000edevice offline")

	result := runRemote(t, inputs, state, checkAdbVersion)

	assert.Equal(t, Result{Status: Fail,
		Summary: "the remote adb server did not answer its version request: the device adb server refused host:version: device offline",
		Hint:    versionProbeHint}, result)
}

func TestGivenAnAdbThatLeavesAChildHoldingItsPipesWhenTheVersionIsCheckedThenItsOutputStillCountsInTime(t *testing.T) {
	// The child inherits stdout and outlives adb; only the pipe-drain bound ends the wait.
	fakeAdbOnPath(t, fakeAdbSpec{Output: adbOutput41, Child: true})
	inputs, state := versionMock(t, "OKAY00040029")
	started := time.Now()

	result := runRemote(t, inputs, state, checkAdbVersion)

	assert.Less(t, time.Since(started), pipeDrainDelay+2*time.Second, "the drain bound, not the child, ends the wait")
	assert.Equal(t, Result{Status: Pass, Summary: "local adb 1.0.41 matches the remote adb server"}, result)
}

func TestGivenABrokenAdbOnPathWhenTheVersionIsCheckedThenTheRowWarnsNamingIt(t *testing.T) {
	tests := []struct {
		name        string
		spec        fakeAdbSpec
		wantSummary string // the fake's path fills the %s
		wantHint    string
	}{
		{name: "adb exits non-zero", spec: fakeAdbSpec{ExitCode: 3}, wantSummary: "'%s version' failed: exit status 3", wantHint: adbFailedHint},
		{name: "adb prints no version line", spec: fakeAdbSpec{Output: "hello\n"},
			wantSummary: `'%s version' printed no 'version 1.0.N' line: "hello"`, wantHint: adbNoVersionHint},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			adb := fakeAdbOnPath(t, tc.spec)
			inputs, state := versionMock(t, "OKAY00040029")

			result := runRemote(t, inputs, state, checkAdbVersion)

			assert.Equal(t, Result{Status: Warn, Summary: fmt.Sprintf(tc.wantSummary, adb),
				Hint: fmt.Sprintf(tc.wantHint, "'"+adb+" version'", doctorRerun(inputs), 41)}, result)
		})
	}
}

func TestGivenAnSdkAdbThatCannotStartWhenTheVersionIsCheckedThenTheRowWarnsWithTheLaunchError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the launch error texts are Unix's")
	}
	tests := []struct {
		name      string
		sdkRoot   func(t *testing.T) string
		mode      os.FileMode
		wantError string
	}{
		{name: "no execute permission", sdkRoot: func(t *testing.T) string { return t.TempDir() }, mode: 0o644, wantError: "permission denied"},
		{name: "an empty file in a directory named with a newline", sdkRoot: func(t *testing.T) string { return filepath.Join(t.TempDir(), "sdk\nroot") },
			mode: 0o755, wantError: "exec format error"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fakeAdbOnPath(t, fakeAdbSpec{Output: adbOutput41})
			platformTools := filepath.Join(tc.sdkRoot(t), "platform-tools")
			require.NoError(t, os.MkdirAll(platformTools, 0o755))
			t.Setenv("ANDROID_HOME", filepath.Dir(platformTools))
			sdkAdb := filepath.Join(platformTools, "adb")
			require.NoError(t, os.WriteFile(sdkAdb, nil, tc.mode))
			inputs, state := versionMock(t, "OKAY00040029")

			result := runRemote(t, inputs, state, checkAdbVersion)

			shown := safeValue(sdkAdb)
			assert.Equal(t, Result{Status: Warn, Summary: fmt.Sprintf("'%[1]s version' failed: fork/exec %[1]s: %[2]s", shown, tc.wantError),
				Hint: fmt.Sprintf(adbFailedHint, "'"+shown+" version'", doctorRerun(inputs), 41)}, result)
			assert.NotContains(t, result.Summary+result.Hint, "\n", "a newline in the path is shown escaped")
		})
	}
}

func TestGivenAMismatchedAdbWhenTheHintIsBuiltThenItSaysEveryCommandFailsAndTheRemoteAdbServerIsNotAffected(t *testing.T) {
	inputs := Inputs{Config: config.Default(), SessionARN: testSessionARN}
	tests := []struct {
		name       string
		mismatched []localAdb
		named      bool
		wantParts  []string
	}{
		{name: "a lone PATH adb", mismatched: []localAdb{{path: "/usr/bin/adb", onPath: true, version: 39}},
			wantParts: []string{"so every adb command fails: that adb tries to start its own adb server on the daemon's port and cannot",
				"Until then no adb command reaches the device. The remote adb server is not affected."}},
		{name: "a named adb", mismatched: []localAdb{{path: "/opt/tools/adb", onPath: true, version: 39}}, named: true,
			wantParts: []string{"so every command from that adb fails", "Until then no command from that adb reaches the device."}},
		{name: "several adbs", named: true,
			mismatched: []localAdb{{path: "/usr/bin/adb", onPath: true, version: 39},
				{path: "/opt/sdk/platform-tools/adb", sdkPath: "/opt/sdk/platform-tools/adb", version: 40}},
			wantParts: []string{"so every command from any of them fails", "Until then no command from any of them reaches the device."}},
		{name: "the Android Studio clause on a lone PATH adb that is the SDK's",
			mismatched: []localAdb{{path: "/usr/bin/adb", onPath: true, sdkPath: "/home/dev/Android/Sdk/platform-tools/adb", version: 39}},
			wantParts:  []string{"so Studio lists no device while that adb is on another version"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			hint := skewHint(tc.mismatched, tc.named, 41, inputs)

			for _, part := range tc.wantParts {
				assert.Contains(t, hint, part)
			}
		})
	}
}

func TestGivenEachAdbVersionVerdictWhenRenderedThenNoTextSaysTheRemoteAdbServerStops(t *testing.T) {
	inputs := Inputs{Config: config.Default(), SessionARN: testSessionARN}
	tests := []struct {
		name      string
		adbs      []localAdb
		hiddenAdb string
		wantFix   string
	}{
		{name: "a mismatched adb", adbs: []localAdb{{path: "/usr/bin/adb", onPath: true, version: 39}},
			wantFix: "Install platform-tools whose adb is 1.0.41"},
		{name: "a broken adb", adbs: []localAdb{{path: "/usr/bin/adb", onPath: true, err: errors.New("'/usr/bin/adb version' failed: exit status 3")}},
			wantFix: "'/usr/bin/adb version'"},
		{name: "an adb reached only through a relative PATH entry", hiddenAdb: "/home/dev/tools/adb",
			adbs:    []localAdb{{path: "/usr/bin/adb", onPath: true, version: 41}},
			wantFix: "Add that directory to PATH"},
		{name: "no adb anywhere", wantFix: "Install Android platform-tools whose adb is 1.0.41"},
		{name: "a mismatched SDK adb",
			adbs:    []localAdb{{path: "/opt/sdk/platform-tools/adb", sdkPath: "/opt/sdk/platform-tools/adb", version: 39}},
			wantFix: "Update Android SDK Platform-Tools"},
		{name: "an adb that prints no version line",
			adbs:    []localAdb{{path: "/usr/bin/adb", onPath: true, err: fmt.Errorf("'/usr/bin/adb version' %w: %q", errNoAdbVersionLine, "hello")}},
			wantFix: "check that it is the platform-tools adb"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := adbVersionVerdict(tc.adbs, tc.hiddenAdb, 41, inputs)

			text := result.Summary + " " + result.Hint
			assert.NotContains(t, text, "stop")
			assert.NotContains(t, text, "ends the remote")
			assert.Contains(t, result.Hint, tc.wantFix, "the fix command stays")
		})
	}
}
