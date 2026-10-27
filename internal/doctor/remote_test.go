package doctor

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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

func TestGivenACredentialSummaryWhenRenderedThenTheRowReflectsItsExpiry(t *testing.T) {
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	refresh := config.DefaultEndpointRefreshInterval
	tests := []struct {
		name    string
		summary session.CredentialSummary
		want    Result
	}{
		{name: "no expiry", summary: session.CredentialSummary{Source: "EnvConfigCredentials"},
			want: Result{Status: Pass, Summary: "credentials from EnvConfigCredentials, no expiry"}},
		{name: "far expiry", summary: session.CredentialSummary{Source: "SSOProvider", CanExpire: true, Expires: now.Add(time.Hour)},
			want: Result{Status: Pass, Summary: "credentials from SSOProvider, expire in 1h0m0s"}},
		{name: "exactly two refreshes", summary: session.CredentialSummary{Source: "SSOProvider", CanExpire: true, Expires: now.Add(2 * refresh)},
			want: Result{Status: Pass, Summary: "credentials from SSOProvider, expire in 8m0s"}},
		{name: "expiry inside two refreshes", summary: session.CredentialSummary{Source: "SSOProvider", CanExpire: true, Expires: now.Add(2*refresh - time.Second)},
			want: Result{Status: Warn, Summary: "credentials from SSOProvider, expire in 7m59s", Hint: refreshCredentialsHint(expiringCause, control.Response{})}},
		{name: "under a second left", summary: session.CredentialSummary{Source: "SSOProvider", CanExpire: true, Expires: now.Add(500 * time.Millisecond)},
			want: Result{Status: Warn, Summary: "credentials from SSOProvider, expire in 0s", Hint: refreshCredentialsHint(expiringCause, control.Response{})}},
		{name: "already expired", summary: session.CredentialSummary{Source: "SSOProvider", CanExpire: true, Expires: now.Add(-time.Minute)},
			want: Result{Status: Fail, Summary: "credentials from SSOProvider, expired 1m0s ago", Hint: refreshCredentialsHint(expiredCause, control.Response{})}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, credentialsResult(tc.summary, now, refresh, control.Response{}))
		})
	}
}

func TestGivenExpiringCredentialsWhenTheHintIsBuiltThenItQuotesTheStartThatFitsTheDaemonState(t *testing.T) {
	tests := []struct {
		name       string
		daemon     control.Response
		wantQuoted []string
		wantAbsent []string
	}{
		{name: "no daemon", wantQuoted: []string{"'" + ssoLoginCommand + "'", "'" + bridgeStartCommand + "'"},
			wantAbsent: []string{restartDaemonHint, reverseCaveat}},
		{name: "a daemon with reverse off", daemon: control.Response{OK: true, Port: 6000, ARN: "arn:1"},
			wantQuoted: []string{"'" + ssoLoginCommand + "'", "'" + restartDaemonHint + " --port 6000 --session-arn arn:1'"},
			wantAbsent: []string{reverseCaveat}},
		{name: "a daemon with reverse on", daemon: control.Response{OK: true, Port: 6000, ReverseEnabled: true},
			wantQuoted: []string{"'" + restartDaemonHint + " --port 6000 --allow-adb-reverse'", reverseCaveat}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			hint := refreshCredentialsHint(expiredCause, tc.daemon)

			assert.True(t, strings.HasPrefix(hint, expiredCause+" "), hint)
			for _, quoted := range tc.wantQuoted {
				assert.Contains(t, hint, quoted)
			}
			for _, absent := range tc.wantAbsent {
				assert.NotContains(t, hint, absent)
			}
		})
	}
}

func TestGivenACredentialsSeamWhenTheCheckRunsThenItsOutcomeIsTheRow(t *testing.T) {
	// A minute left is inside two refresh intervals of the default config, so the row warns:
	// this proves the check feeds the config's interval and the clock into the verdict.
	inputs := Inputs{Config: config.Default(), Credentials: func(context.Context) (session.CredentialSummary, error) {
		return session.CredentialSummary{Source: "SSOProvider", CanExpire: true, Expires: time.Now().Add(time.Minute)}, nil
	}}
	warning := runRemote(t, inputs, &runState{}, checkCredentials)
	assert.Equal(t, Warn, warning.Status)
	assert.Contains(t, warning.Summary, "credentials from SSOProvider, expire in")
	assert.Equal(t, refreshCredentialsHint(expiringCause, control.Response{}), warning.Hint)
	assert.Contains(t, warning.Hint, "'"+bridgeStartCommand+"'", "with no daemon the hint names the first start")

	inputs.Credentials = func(context.Context) (session.CredentialSummary, error) {
		return session.CredentialSummary{}, errors.New("no EC2 IMDS role found")
	}
	assert.Equal(t, Result{Status: Fail, Summary: "this shell has no usable AWS credentials: no EC2 IMDS role found", Hint: credentialsHint},
		runRemote(t, inputs, &runState{}, checkCredentials))
}

func TestGivenNoCredentialsSeamWhenTheCheckRunsThenTheRealChainIsWalked(t *testing.T) {
	testsupport.HermeticAWSEnv(t)

	result := runRemote(t, Inputs{Config: config.Default()}, &runState{}, checkCredentials)

	assert.Equal(t, Result{Status: Pass, Summary: "credentials from EnvConfigCredentials, no expiry"}, result)
}

func TestGivenNoSessionArnWhenTheSessionCheckRunsThenItSkipsNamingTheFlag(t *testing.T) {
	resolver := &fakeResolver{}

	result := runRemote(t, Inputs{Resolver: resolver}, &runState{}, checkSession)

	assert.Equal(t, Result{Status: Skip, Summary: "pass --session-arn to check the session, the device connection and adb versions"}, result)
	assert.NotRegexp(t, `(local|aws|session|relay)\.[a-z-]+`, result.Summary, "no check id in a user row")
	assert.Empty(t, resolver.arns, "no ARN, no call")
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
