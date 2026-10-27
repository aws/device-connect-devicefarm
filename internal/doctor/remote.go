package doctor

import (
	"context"
	"fmt"
	"time"

	"github.com/aws/device-connect-devicefarm/internal/control"
	"github.com/aws/device-connect-devicefarm/internal/session"
)

const (
	checkCredentials = "aws.credentials"
	// remoteTimeout bounds one AWS call. Above the SDK's instance metadata timeout, so an empty
	// chain fails with its hint, not the runner's timeout text.
	remoteTimeout = 10 * time.Second
)

const (
	ssoLoginCommand = "aws sso login --profile <name>"
	// credentialsFailLead puts user words before the SDK's config or chain error.
	credentialsFailLead = "this shell has no usable AWS credentials: "
	// No period after the URL, so a copied line stays a working link.
	credentialsHint = "the AWS config did not load or its credential chain resolved nothing, so every Device Farm call fails. " +
		"Set 'AWS_PROFILE' to a profile with Device Farm access, run '" + ssoLoginCommand + "' for a profile that uses SSO, " +
		"or export 'AWS_ACCESS_KEY_ID' and 'AWS_SECRET_ACCESS_KEY'. For more information, see " +
		"https://docs.aws.amazon.com/cli/latest/userguide/cli-chap-authentication.html"
	expiredCause  = "these credentials have expired, so every Device Farm call fails."
	expiringCause = "these credentials expire soon, so a daemon started from this shell would lose the device a few minutes " +
		"after they expire."
)

// refreshCredentialsHint: the cause, then the login or the fresh keys and the daemon start that
// inherits them. A running daemon keeps its port, reverse setting and session in the restart.
func refreshCredentialsHint(cause string, daemon control.Response) string {
	if !daemon.OK {
		return fmt.Sprintf("%s Run '%s' for an SSO profile, or export fresh keys, then start the daemon from this shell with '%s'. "+
			"The daemon inherits the credentials of the shell that starts it.", cause, ssoLoginCommand, bridgeStartCommand)
	}
	return caveatIf(fmt.Sprintf("%s Run '%s' for an SSO profile, or export fresh keys and run '%s', since exported keys reach a "+
		"running daemon only through that restart.", cause, ssoLoginCommand, restartWith(daemon, daemon.Port, daemon.ReverseEnabled)),
		daemon.ReverseEnabled)
}

// remoteChecks are the rows that reach AWS, in run order.
func remoteChecks(inputs Inputs, state *runState) []Check {
	return []Check{
		{ID: checkCredentials, Title: "AWS credentials", Category: CategoryAWS, Timeout: remoteTimeout,
			Run: func(ctx context.Context) Result { return checkCredentialsResolve(ctx, inputs, state) }},
	}
}

// checkCredentialsResolve walks the credential chain once and names its source, never a key.
func checkCredentialsResolve(ctx context.Context, inputs Inputs, state *runState) Result {
	summary, err := inputs.Credentials(ctx)
	if err != nil {
		return Result{Status: Fail, Summary: credentialsFailLead + err.Error(), Hint: credentialsHint}
	}
	daemonReply, _ := state.daemon()
	return credentialsResult(summary, time.Now(), inputs.Config.EndpointRefreshInterval, daemonReply)
}

// credentialsResult renders the row. Expiry inside two endpoint refreshes warns: the daemon
// detaches on its first failed refresh. daemon is the running daemon's status reply, if any.
func credentialsResult(summary session.CredentialSummary, now time.Time, refreshInterval time.Duration, daemon control.Response) Result {
	text := "credentials from " + summary.Source
	if !summary.CanExpire {
		return Result{Status: Pass, Summary: text + ", no expiry"}
	}
	remaining := summary.Expires.Sub(now)
	shown := remaining.Truncate(time.Second) // the verdict uses the full value
	switch {
	case remaining <= 0:
		return Result{Status: Fail, Summary: fmt.Sprintf("%s, expired %s ago", text, -shown), Hint: refreshCredentialsHint(expiredCause, daemon)}
	case remaining < 2*refreshInterval:
		return Result{Status: Warn, Summary: fmt.Sprintf("%s, expire in %s", text, shown), Hint: refreshCredentialsHint(expiringCause, daemon)}
	}
	return Result{Status: Pass, Summary: fmt.Sprintf("%s, expire in %s", text, shown)}
}
