package doctor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/aws/device-connect-devicefarm/internal/bridge"
	"github.com/aws/device-connect-devicefarm/internal/config"
	"github.com/aws/device-connect-devicefarm/internal/control"
	"github.com/aws/device-connect-devicefarm/internal/session"
)

const (
	checkCredentials  = "aws.credentials"
	checkSession      = "session.resolve"
	checkRelaySession = "relay.session"
	checkReattach     = "relay.reattach"
	checkReverse      = "relay.reverse"
	// remoteTimeout bounds one AWS call or one websocket probe. Above the SDK's instance
	// metadata timeout, so an empty chain fails with its hint, not the runner's timeout text.
	remoteTimeout = 10 * time.Second
	// reattachTimeout gives the reattach probe more room than a fresh session: the server has to
	// find the session before it answers.
	reattachTimeout = 15 * time.Second
	// probeMargin is how long before the row's deadline a relay probe gives up, so a silent
	// relay yields the probe's error and the row's hint, not the runner's timeout text.
	probeMargin = time.Second
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
	proxyIgnoredNote = "'HTTPS_PROXY' is set, but the connection to the device does not use it, so no adb traffic gets " +
		"through when this machine reaches the internet only through that proxy."
	reverseOnSummary   = "the running daemon has adb reverse on for this session"
	reversePassSummary = "adb reverse is available on this session"
	// reverseHeldSummary replaces bridge.ErrNotifierSlotTaken's text, which names the mechanism.
	reverseHeldSummary = "another client, or an earlier run of this client, still uses adb reverse on this session"
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

// remoteChecks are the rows that leave this machine, in run order.
func remoteChecks(inputs Inputs, state *runState) []Check {
	return []Check{
		{ID: checkCredentials, Title: "AWS credentials", Category: CategoryAWS, Timeout: remoteTimeout,
			Run: func(ctx context.Context) Result { return checkCredentialsResolve(ctx, inputs, state) }},
		{ID: checkSession, Title: "remote access session", Category: CategoryAWS, Needs: []string{checkCredentials}, Timeout: remoteTimeout,
			Run: func(ctx context.Context) Result { return checkSessionRunning(ctx, inputs, state) }},
		{ID: checkRelaySession, Title: "adb session to the device", Category: CategoryNetwork, Needs: []string{checkSession}, Timeout: remoteTimeout,
			Run: func(ctx context.Context) Result { return checkRelaySessionOpens(ctx, inputs, state) }},
		{ID: checkReattach, Title: "adb session reconnect", Category: CategoryNetwork, Needs: []string{checkRelaySession}, Timeout: reattachTimeout,
			Run: func(ctx context.Context) Result { return checkReattachAcked(ctx, inputs, state) }},
		{ID: checkReverse, Title: "adb reverse", Category: CategoryNetwork, Needs: []string{checkSession, checkDaemon}, Timeout: remoteTimeout,
			Run: func(ctx context.Context) Result { return checkReverseNotifier(ctx, inputs, state) }},
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

// checkRelaySessionOpens opens one adb session over the relay and selects the device transport,
// the first exchange of every proxied connection.
func checkRelaySessionOpens(ctx context.Context, inputs Inputs, state *runState) Result {
	probeCtx, cancel := withProbeBudget(ctx)
	defer cancel()
	sessionUUID, err := inputs.Dialer.ProbeSession(probeCtx, state.endpoint)
	if err != nil {
		return Result{Status: Fail, Summary: "could not open an adb session to the device: " + err.Error(), Hint: relayHint(inputs)}
	}
	state.sessionUUID = sessionUUID
	return Result{Status: Pass, Summary: fmt.Sprintf("adb session %s opened to the device", sessionUUID)}
}

// checkReattachAcked dials again with the probed UUID; the server must resume the session, as
// every websocket swap needs.
func checkReattachAcked(ctx context.Context, inputs Inputs, state *runState) Result {
	probeCtx, cancel := withProbeBudget(ctx)
	defer cancel()
	if err := inputs.Dialer.ProbeReattach(probeCtx, state.endpoint, state.sessionUUID); err != nil {
		return Result{Status: Fail, Summary: fmt.Sprintf("could not reconnect to adb session %s: %v", state.sessionUUID, err),
			Hint: reattachHint(inputs)}
	}
	return Result{Status: Pass, Summary: "reconnected to adb session " + state.sessionUUID}
}

// checkReverseNotifier registers a reverse notifier and releases it. The server allows one
// notifier per device and refuses a second hello, so a probe cannot tell the running daemon's
// slot from another client's. With the running daemon on this session with reverse on, the row
// passes on that setting and runs no probe.
func checkReverseNotifier(ctx context.Context, inputs Inputs, state *runState) Result {
	daemon, _ := state.daemon()
	if daemon.OK && daemon.ReverseEnabled && strings.TrimSpace(daemon.ARN) == strings.TrimSpace(inputs.SessionARN) {
		return Result{Status: Pass, Summary: reverseOnSummary}
	}
	probeCtx, cancel := withProbeBudget(ctx)
	defer cancel()
	err := inputs.Dialer.ProbeNotifier(probeCtx, state.endpoint)
	switch {
	case errors.Is(err, bridge.ErrNotifierSlotTaken):
		return Result{Status: Fail, Summary: reverseHeldSummary, Hint: slotHeldHint(inputs)}
	case err != nil:
		return Result{Status: Fail, Summary: "adb reverse is not available on this session: " + err.Error(), Hint: relayHint(inputs)}
	}
	return Result{Status: Pass, Summary: reversePassSummary}
}

// withProbeBudget ends a probe probeMargin before the row's deadline. The probes bound each
// read by the context deadline, so without the margin a silent relay and the runner's clock
// would expire together and the row would lose its hint.
func withProbeBudget(ctx context.Context) (context.Context, context.CancelFunc) {
	deadline, ok := ctx.Deadline()
	if !ok {
		return context.WithCancel(ctx)
	}
	return context.WithDeadline(ctx, deadline.Add(-probeMargin))
}

// relayHint is the one hint for a failed relay probe; the summary names the failure. With a
// proxy set, it adds that the websocket connection ignores it.
func relayHint(inputs Inputs) string {
	hint := fmt.Sprintf("Device Farm or the device did not complete the adb connection, so an adb connection through the daemon "+
		"would fail the same way. Rerun %s once; a second failure means the network path to Device Farm or the device itself "+
		"is at fault, so check the session in the Device Farm console.", doctorRerun(inputs))
	if os.Getenv("HTTPS_PROXY") != "" || os.Getenv("https_proxy") != "" {
		hint += " " + proxyIgnoredNote
	}
	return hint
}

func reattachHint(inputs Inputs) string {
	return fmt.Sprintf("Device Farm did not resume the adb session on a new connection, so a live adb connection would drop "+
		"when the daemon reconnects. Rerun %s once. A second failure means Device Farm does not keep adb sessions across "+
		"reconnects.", doctorRerun(inputs))
}

func slotHeldHint(inputs Inputs) string {
	return fmt.Sprintf("another client still uses adb reverse on this session, so Device Farm would refuse 'adb reverse' through "+
		"the daemon. Stop that client with '%s' on the machine where it runs, or wait for Device Farm to release it, then rerun "+
		"%s. Device Farm lets one client at a time use adb reverse on a device.", bridgeStopCommand, doctorRerun(inputs))
}
