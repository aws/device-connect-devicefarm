package doctor

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/aws/device-connect-devicefarm/internal/config"
	"github.com/aws/device-connect-devicefarm/internal/control"
	"github.com/aws/device-connect-devicefarm/internal/session"
	"github.com/aws/device-connect-devicefarm/internal/testsupport"
)

// runRemote runs the check named by id from the full table, defaults applied.
func runRemote(t *testing.T, inputs Inputs, id string) Result {
	t.Helper()
	return checkByID(t, Checks(inputs), id).Run(context.Background())
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
	warning := runRemote(t, inputs, checkCredentials)
	assert.Equal(t, Warn, warning.Status)
	assert.Contains(t, warning.Summary, "credentials from SSOProvider, expire in")
	assert.Equal(t, refreshCredentialsHint(expiringCause, control.Response{}), warning.Hint)
	assert.Contains(t, warning.Hint, "'"+bridgeStartCommand+"'", "with no daemon the hint names the first start")

	inputs.Credentials = func(context.Context) (session.CredentialSummary, error) {
		return session.CredentialSummary{}, errors.New("no EC2 IMDS role found")
	}
	assert.Equal(t, Result{Status: Fail, Summary: "this shell has no usable AWS credentials: no EC2 IMDS role found", Hint: credentialsHint},
		runRemote(t, inputs, checkCredentials))
}

func TestGivenNoCredentialsSeamWhenTheCheckRunsThenTheRealChainIsWalked(t *testing.T) {
	testsupport.HermeticAWSEnv(t)

	result := runRemote(t, Inputs{Config: config.Default()}, checkCredentials)

	assert.Equal(t, Result{Status: Pass, Summary: "credentials from EnvConfigCredentials, no expiry"}, result)
}
