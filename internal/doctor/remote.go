package doctor

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/aws/device-connect-devicefarm/internal/config"
	"github.com/aws/device-connect-devicefarm/internal/control"
	"github.com/aws/device-connect-devicefarm/internal/session"
)

const (
	checkCredentials = "aws.credentials"
	checkSession     = "session.resolve"
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

// doctorRerun quotes the doctor rerun with the flags this run used.
func doctorRerun(inputs Inputs) string {
	command := fmt.Sprintf("%s --session-arn %s --port %d", doctorCommand, safeValue(inputs.SessionARN), inputs.Config.ListenPort)
	if inputs.Config.ListenAddr != config.DefaultListenAddr {
		command += " --listen-addr " + safeValue(inputs.Config.ListenAddr)
	}
	return "'" + command + "'"
}

const (
	createSessionCommand = "aws devicefarm create-remote-access-session --project-arn <project ARN> --device-arn <device ARN> " +
		"--region us-west-2"
	passArnTail = "and pass its ARN with '--session-arn'"
)

// Session hints. Each quotes the passed ARN.

func invalidArnHint(inputs Inputs) string {
	return fmt.Sprintf("'%s' is not a Device Farm remote access session ARN in us-west-2. Pass '--session-arn' the ARN that the "+
		"Device Farm console or '%s' printed. The daemon reaches the device through that ARN and refuses any other.",
		safeValue(inputs.SessionARN), createSessionCommand)
}

func accessDeniedHint(inputs Inputs) string {
	return fmt.Sprintf("these credentials may not read session '%s'. Allow 'devicefarm:GetRemoteAccessSession' on it in the IAM "+
		"policy of this profile or role, or set 'AWS_PROFILE' to a profile that has it. The daemon makes the same call every "+
		"few minutes while the device is attached, so the permission must stay for that whole time.",
		safeValue(inputs.SessionARN))
}

func sessionGoneHint(inputs Inputs) string {
	return fmt.Sprintf("Device Farm has no session '%s' in this account and region. Check the ARN against "+
		"'aws devicefarm list-remote-access-sessions --arn <project ARN> --region us-west-2' and pass one whose status is "+
		"RUNNING. A mistyped ARN or a session of another account resolves to nothing.", safeValue(inputs.SessionARN))
}

func unsupportedDeviceHint(inputs Inputs) string {
	return fmt.Sprintf("session '%s' runs on an Android release older than this client supports, so the daemon cannot attach it. "+
		"Start a new remote access session on a device with a newer Android release (the summary names the oldest one "+
		"supported) %s. The daemon refuses the older device at attach time too.", safeValue(inputs.SessionARN), passArnTail)
}

// nonAndroidDeviceHint: the device is not Android. platform is UnsupportedDeviceError.Platform.
func nonAndroidDeviceHint(inputs Inputs, platform string) string {
	return fmt.Sprintf("session '%s' runs on an %s device. This client connects adb to Android devices only, so the daemon "+
		"cannot attach it. Start a new remote access session on an Android device in the Device Farm console or with '%s', %s. "+
		"The daemon refuses the %s device at attach time too.", safeValue(inputs.SessionARN), platform, createSessionCommand,
		passArnTail, platform)
}

func sessionEndedHint(inputs Inputs) string {
	return fmt.Sprintf("session '%s' has ended, so nothing can attach to it. Start a new remote access session in the Device Farm "+
		"console or with '%s', %s. A daemon attached to a session that ends drops the device within a few minutes.",
		safeValue(inputs.SessionARN), createSessionCommand, passArnTail)
}

func notRunningHint(inputs Inputs) string {
	return fmt.Sprintf("this session has no interactive endpoint yet (it is still starting, or not RUNNING), so the daemon "+
		"cannot attach it. Wait until 'aws devicefarm get-remote-access-session --arn %s --region us-west-2' reports status "+
		"RUNNING with an endpoint, then rerun %s.", safeValue(inputs.SessionARN), doctorRerun(inputs))
}

func requestRejectedHint(inputs Inputs) string {
	return fmt.Sprintf("Device Farm rejected the request for session '%s' (the summary names the code), so the request itself "+
		"is at fault: expired or wrong credentials, a bad signature or a clock that is off, not the network. Run "+
		"'aws sts get-caller-identity' with the same profile or keys and fix what it reports, or the system clock on a "+
		"signature error, then rerun %s. The daemon signs its calls the same way and would detach the device on the same answer.",
		safeValue(inputs.SessionARN), doctorRerun(inputs))
}

func sessionNetworkHint(inputs Inputs) string {
	return fmt.Sprintf("the call to Device Farm for session '%s' failed. Check the network path from this machine to "+
		"'devicefarm.us-west-2.amazonaws.com'; 'HTTPS_PROXY' applies to this call, so set or clear it to match your network. "+
		"The daemon makes the same call when it attaches and every few minutes after.", safeValue(inputs.SessionARN))
}

// sessionArnSkipSummary is the session row's SKIP text when no ARN was passed.
const sessionArnSkipSummary = "pass --session-arn to check the session, the device connection and adb versions"

// remoteChecks are the rows that reach AWS, in run order.
func remoteChecks(inputs Inputs, state *runState) []Check {
	return []Check{
		{ID: checkCredentials, Title: "AWS credentials", Category: CategoryAWS, Timeout: remoteTimeout,
			Run: func(ctx context.Context) Result { return checkCredentialsResolve(ctx, inputs, state) }},
		{ID: checkSession, Title: "remote access session", Category: CategoryAWS, Needs: []string{checkCredentials}, Timeout: remoteTimeout,
			Run: func(ctx context.Context) Result { return checkSessionRunning(ctx, inputs, state) }},
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

// checkSessionRunning calls GetSession once; one call proves the ARN, region, credentials,
// permission and a RUNNING session. The endpoint goes to the run state, never to the row.
func checkSessionRunning(ctx context.Context, inputs Inputs, state *runState) Result {
	if inputs.SessionARN == "" {
		return Result{Status: Skip, Summary: sessionArnSkipSummary}
	}
	resolver := inputs.Resolver
	if resolver == nil {
		var err error
		if resolver, err = session.NewRemoteAccessClient(ctx, inputs.Logger); err != nil {
			return Result{Status: Fail, Summary: err.Error(), Hint: credentialsHint}
		}
	}
	info, err := resolver.GetSession(ctx, inputs.SessionARN)
	if err != nil {
		return Result{Status: Fail, Summary: err.Error(), Hint: sessionHint(err, inputs)}
	}
	state.endpoint = info.Endpoint
	return Result{Status: Pass, Summary: "RUNNING: " + info.Label}
}

// sessionHint picks the fix by the error's class.
func sessionHint(err error, inputs Inputs) string {
	var refusal session.UnsupportedDeviceError
	switch {
	case errors.Is(err, session.ErrInvalidARN):
		return invalidArnHint(inputs)
	case errors.Is(err, session.ErrAccessDenied):
		return accessDeniedHint(inputs)
	case errors.Is(err, session.ErrSessionNotFound):
		return sessionGoneHint(inputs)
	case errors.As(err, &refusal) && !refusal.BelowAndroidFloor():
		return nonAndroidDeviceHint(inputs, refusal.Platform)
	case errors.Is(err, session.ErrUnsupportedDevice):
		return unsupportedDeviceHint(inputs)
	case errors.Is(err, session.ErrSessionEnded):
		return sessionEndedHint(inputs)
	case errors.Is(err, session.ErrNoEndpointYet):
		return notRunningHint(inputs)
	case errors.Is(err, session.ErrRequestRejected):
		return requestRejectedHint(inputs)
	}
	return sessionNetworkHint(inputs)
}
