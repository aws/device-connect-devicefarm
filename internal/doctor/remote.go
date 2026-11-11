// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package doctor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aws/device-connect-devicefarm/internal/bridge"
	"github.com/aws/device-connect-devicefarm/internal/config"
	"github.com/aws/device-connect-devicefarm/internal/session"
)

const (
	checkCredentials  = "aws.credentials"
	checkSession      = "session.resolve"
	checkRelaySession = "relay.session"
	checkReattach     = "relay.reattach"
	checkReverse      = "relay.reverse"
	checkAdbVersion   = "relay.adb-version"
	// remoteTimeout bounds one AWS call or one websocket probe. Above the SDK's instance
	// metadata timeout, so an empty chain fails with its hint, not the runner's timeout text.
	remoteTimeout = 10 * time.Second
	// reattachTimeout gives the reattach probe more room than a fresh session: the server has to
	// find the session before it answers.
	reattachTimeout = 15 * time.Second
	// probeMargin is how long before the row's deadline a relay probe gives up, so a silent
	// relay yields the probe's error and the row's hint, not the runner's timeout text.
	probeMargin = time.Second
	// adbVersionTimeout bounds the local `adb version`, which starts no server.
	adbVersionTimeout = 5 * time.Second
	// pipeDrainDelay bounds the wait for `adb version`'s pipes once it exited or was killed.
	pipeDrainDelay = time.Second
)

// adbVersionPattern finds the protocol version in `adb version` output.
var adbVersionPattern = regexp.MustCompile(`version 1\.0\.(\d{1,5})\b`)

// errNoAdbVersionLine: adb ran but printed no version the pattern reads.
var errNoAdbVersionLine = errors.New("printed no 'version 1.0.N' line")

const (
	ssoLoginCommand = "aws sso login --profile <name>"
	// credentialsFailLead puts user words before the SDK's config or chain error.
	credentialsFailLead = "this shell has no usable AWS credentials: "
	// No period after the URL, so a copied line stays a working link.
	credentialsHint = "the AWS config did not load or its credential chain resolved nothing, so every Device Farm call fails. " +
		"Set 'AWS_PROFILE' to a profile with Device Farm access, run '" + ssoLoginCommand + "' for a profile that uses SSO, " +
		"or export 'AWS_ACCESS_KEY_ID' and 'AWS_SECRET_ACCESS_KEY'. For more information, see " +
		"https://docs.aws.amazon.com/cli/latest/userguide/cli-chap-authentication.html"
	proxyIgnoredNote = "'HTTPS_PROXY' is set, but the connection to the device does not use it, so no adb traffic gets " +
		"through when this machine reaches the internet only through that proxy."
	reverseOnSummary   = "the running bridge has adb reverse on for this session"
	reversePassSummary = "adb reverse is available on this session"
	// reverseHeldSummary replaces bridge.ErrNotifierSlotTaken's text, which names the mechanism.
	reverseHeldSummary = "another client, or an earlier run of this client, still uses adb reverse on this session"
	// adbHostVersionTail ends the broken-adb WARN hints.
	adbHostVersionTail = " The remote adb server is 1.0.%[3]d, and a local adb on another version cannot connect through " +
		"the bridge."
	// Broken-adb WARN format args: quoted `<adb> version` command, quoted doctor rerun, remote adb server's version.
	adbFailedHint = "the local %[1]s failed or did not finish, so that adb's version is unknown. Run %[1]s by hand and fix what it " +
		"reports, then rerun %[2]s." + adbHostVersionTail
	adbNoVersionHint = "the local %[1]s printed no 'version 1.0.N' line, so that adb's version is unknown. Run %[1]s by hand " +
		"and check that it is the platform-tools adb, then rerun %[2]s." + adbHostVersionTail
	// versionProbeFailLead puts user words before the probe error, the cause AWS Support needs.
	versionProbeFailLead = "the remote adb server did not answer its version request: "
	versionProbeHint     = "there is an error communicating with the remote adb server, so an adb connection through the bridge " +
		"would fail the same way. Try again later, or contact AWS Support for assistance."
	// noAdbSummary takes the remote adb server's version.
	noAdbSummary = "no adb found on PATH or in an Android SDK, so its version was not checked against the remote adb server (1.0.%d)"
	// sdkLocations: the SDK places searched, as the no-adb hint names them.
	sdkLocations = "'ANDROID_HOME', 'ANDROID_SDK_ROOT' or Android Studio's default SDK location"
)

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
		"Device Farm console or '%s' printed. The bridge reaches the device through that ARN and refuses any other.",
		safeValue(inputs.SessionARN), createSessionCommand)
}

func accessDeniedHint(inputs Inputs) string {
	return fmt.Sprintf("these credentials may not read session '%s'. Allow 'devicefarm:GetRemoteAccessSession' on it in the IAM "+
		"policy of this profile or role, or set 'AWS_PROFILE' to a profile that has it. The bridge makes the same call every "+
		"few minutes while the device is attached, so the permission must stay for that whole time.",
		safeValue(inputs.SessionARN))
}

func sessionGoneHint(inputs Inputs) string {
	return fmt.Sprintf("Device Farm has no session '%s' in this account and region. Check the ARN against "+
		"'aws devicefarm list-remote-access-sessions --arn <project ARN> --region us-west-2' and pass one whose status is "+
		"RUNNING. A mistyped ARN or a session of another account resolves to nothing.", safeValue(inputs.SessionARN))
}

func unsupportedDeviceHint(inputs Inputs) string {
	return fmt.Sprintf("session '%s' runs on an Android release older than this client supports, so the bridge cannot attach it. "+
		"Start a new remote access session on a device with a newer Android release (the summary names the oldest one "+
		"supported) %s. The bridge refuses the older device at attach time too.", safeValue(inputs.SessionARN), passArnTail)
}

// nonAndroidDeviceHint: the device is not Android. platform is UnsupportedDeviceError.Platform.
func nonAndroidDeviceHint(inputs Inputs, platform string) string {
	return fmt.Sprintf("session '%s' runs on an %s device. This client connects adb to Android devices only, so the bridge "+
		"cannot attach it. Start a new remote access session on an Android device in the Device Farm console or with '%s', %s. "+
		"The bridge refuses the %s device at attach time too.", safeValue(inputs.SessionARN), platform, createSessionCommand,
		passArnTail, platform)
}

func sessionEndedHint(inputs Inputs) string {
	return fmt.Sprintf("session '%s' has ended, so nothing can attach to it. Start a new remote access session in the Device Farm "+
		"console or with '%s', %s. A bridge attached to a session that ends drops the device within a few minutes.",
		safeValue(inputs.SessionARN), createSessionCommand, passArnTail)
}

func notRunningHint(inputs Inputs) string {
	return fmt.Sprintf("this session has no interactive endpoint yet (it is still starting, or not RUNNING), so the bridge "+
		"cannot attach it. Wait until 'aws devicefarm get-remote-access-session --arn %s --region us-west-2' reports status "+
		"RUNNING with an endpoint, then rerun %s.", safeValue(inputs.SessionARN), doctorRerun(inputs))
}

func requestRejectedHint(inputs Inputs) string {
	return fmt.Sprintf("Device Farm rejected the request for session '%s' (the summary names the code), so the request itself "+
		"is at fault: expired or wrong credentials, a bad signature or a clock that is off, not the network. Run "+
		"'aws sts get-caller-identity' with the same profile or keys and fix what it reports, or the system clock on a "+
		"signature error, then rerun %s. The bridge signs its calls the same way and would detach the device on the same answer.",
		safeValue(inputs.SessionARN), doctorRerun(inputs))
}

func sessionNetworkHint(inputs Inputs) string {
	return fmt.Sprintf("the call to Device Farm for session '%s' failed. Check the network path from this machine to "+
		"'devicefarm.us-west-2.amazonaws.com'; 'HTTPS_PROXY' applies to this call, so set or clear it to match your network. "+
		"The bridge makes the same call when it attaches and every few minutes after.", safeValue(inputs.SessionARN))
}

// sessionArnSkipSummary is the session row's SKIP text with no ARN and no device.
const sessionArnSkipSummary = "pass --session-arn to check the session, the device connection and adb versions"

// severalSessionsSkipSummary is the session row's SKIP text with no ARN and several devices.
// It takes the device count.
const severalSessionsSkipSummary = "the bridge holds %d devices; pass --session-arn to pick the session to check"

// sessionToCheck is the session the session and relay rows check: the trimmed --session-arn
// value, else the session of the one device the running daemon holds (fromDaemon true), else
// empty with attached the daemon's device count.
func sessionToCheck(inputs Inputs, state *runState) (sessionARN string, fromDaemon bool, attached int) {
	if flagARN := strings.TrimSpace(inputs.SessionARN); flagARN != "" {
		return flagARN, false, 0
	}
	reply, _ := state.daemon()
	if reply.OK && len(reply.Devices) == 1 {
		return reply.Devices[0].SessionARN, true, 1
	}
	return "", false, len(reply.Devices)
}

// withSessionToCheck returns inputs with SessionARN set to the session to check, so hints and
// the quoted rerun name the effective session.
func withSessionToCheck(inputs Inputs, state *runState) Inputs {
	inputs.SessionARN, _, _ = sessionToCheck(inputs, state)
	return inputs
}

// remoteChecks are the rows that leave this machine, in run order.
func remoteChecks(inputs Inputs, state *runState) []Check {
	return []Check{
		{ID: checkCredentials, Title: "AWS credentials", Category: CategoryAWS, Timeout: remoteTimeout,
			Run: func(ctx context.Context) Result { return checkCredentialsResolve(ctx, inputs) }},
		{ID: checkSession, Title: "remote access session", Category: CategoryAWS, Needs: []string{checkCredentials}, Timeout: remoteTimeout,
			Run: func(ctx context.Context) Result { return checkSessionRunning(ctx, inputs, state) }},
		{ID: checkRelaySession, Title: "adb session to the device", Category: CategoryNetwork, Needs: []string{checkSession}, Timeout: remoteTimeout,
			Run: func(ctx context.Context) Result { return checkRelaySessionOpens(ctx, inputs, state) }},
		{ID: checkReattach, Title: "adb session reconnect", Category: CategoryNetwork, Needs: []string{checkRelaySession}, Timeout: reattachTimeout,
			Run: func(ctx context.Context) Result { return checkReattachAcked(ctx, inputs, state) }},
		{ID: checkReverse, Title: "adb reverse", Category: CategoryNetwork, Needs: []string{checkSession, checkDaemon}, Timeout: remoteTimeout,
			Run: func(ctx context.Context) Result { return checkReverseNotifier(ctx, inputs, state) }},
		{ID: checkAdbVersion, Title: "adb versions", Category: CategoryDependencies, Needs: []string{checkRelaySession},
			Timeout: adbVersionTimeout + pipeDrainDelay + remoteTimeout,
			Run:     func(ctx context.Context) Result { return checkAdbVersionSkew(ctx, inputs, state) }},
	}
}

// checkCredentialsResolve walks the credential chain once and names its source, never a key.
// No expiry verdict: static keys report none, even temporary ones, and the SDK fetches new
// credentials when a reported expiry passes. Only a real call proves them: the session row's,
// when it has a session to check.
func checkCredentialsResolve(ctx context.Context, inputs Inputs) Result {
	summary, err := inputs.Credentials(ctx)
	if err != nil {
		return Result{Status: Fail, Summary: credentialsFailLead + err.Error(), Hint: credentialsHint}
	}
	return Result{Status: Pass, Summary: "credentials from " + summary.Source}
}

// sessionLookup is one GetSession answer. noClient: err came from building the client.
type sessionLookup struct {
	info     session.RemoteAccessInfo
	err      error
	noClient bool
}

// lookUpSession makes the run's one lookup of the session to check, under remoteTimeout, and
// keeps the answer. The first row that needs it makes the call; a later row reads the kept
// answer.
func (state *runState) lookUpSession(ctx context.Context, inputs Inputs) sessionLookup {
	state.lookupOnce.Do(func() {
		sessionARN, _, _ := sessionToCheck(inputs, state)
		lookupCtx, cancel := context.WithTimeout(ctx, remoteTimeout)
		defer cancel()
		resolver := inputs.Resolver
		if resolver == nil {
			var err error
			if resolver, err = session.NewRemoteAccessClient(lookupCtx, inputs.Logger); err != nil {
				state.lookup = sessionLookup{err: err, noClient: true}
				return
			}
		}
		info, err := resolver.GetSession(lookupCtx, sessionARN)
		state.lookup = sessionLookup{info: info, err: err}
	})
	return state.lookup
}

// checkSessionRunning reads the run's one GetSession call; one call proves the ARN, region,
// credentials, permission and a RUNNING session. The endpoint goes to the run state, never to
// the row. The run state also keeps the ARN the service returned.
// Without --session-arn it checks the one attached device's session and names it; with no
// daemon, no device or several devices it skips.
func checkSessionRunning(ctx context.Context, inputs Inputs, state *runState) Result {
	sessionARN, fromDaemon, attached := sessionToCheck(inputs, state)
	if sessionARN == "" {
		if attached >= 2 {
			return Result{Status: Skip, Summary: fmt.Sprintf(severalSessionsSkipSummary, attached)}
		}
		return Result{Status: Skip, Summary: sessionArnSkipSummary}
	}
	inputs.SessionARN = sessionARN
	lookup := state.lookUpSession(ctx, inputs)
	switch {
	case lookup.noClient:
		return Result{Status: Fail, Summary: lookup.err.Error(), Hint: credentialsHint}
	case lookup.err != nil:
		return Result{Status: Fail, Summary: lookup.err.Error(), Hint: sessionHint(lookup.err, inputs)}
	}
	info := lookup.info
	state.endpoint, state.returnedARN = info.Endpoint, info.ReturnedARN(sessionARN)
	if fromDaemon {
		return Result{Status: Pass, Summary: fmt.Sprintf("RUNNING: %s (the session the bridge holds: %s)", info.Label, safeValue(sessionARN))}
	}
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
	inputs = withSessionToCheck(inputs, state)
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
	inputs = withSessionToCheck(inputs, state)
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
	inputs = withSessionToCheck(inputs, state)
	daemon, _ := state.daemon()
	if daemon.OK && daemon.ReverseEnabled &&
		attachedIsAsked(strings.TrimSpace(daemon.ARN), strings.TrimSpace(inputs.SessionARN), state.returnedARN) {
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
	hint := fmt.Sprintf("Device Farm or the device did not complete the adb connection, so an adb connection through the bridge "+
		"would fail the same way. Rerun %s once; a second failure means the network path to Device Farm or the device itself "+
		"is at fault, so check the session in the Device Farm console.", doctorRerun(inputs))
	if os.Getenv("HTTPS_PROXY") != "" || os.Getenv("https_proxy") != "" {
		hint += " " + proxyIgnoredNote
	}
	return hint
}

func reattachHint(inputs Inputs) string {
	return fmt.Sprintf("Device Farm did not resume the adb session on a new connection, so a live adb connection would drop "+
		"when the bridge reconnects. Rerun %s once. A second failure means Device Farm does not keep adb sessions across "+
		"reconnects.", doctorRerun(inputs))
}

func slotHeldHint(inputs Inputs) string {
	return fmt.Sprintf("another client still uses adb reverse on this session, so Device Farm would refuse 'adb reverse' through "+
		"the bridge. Stop that client with '%s' on the machine where it runs, or wait for Device Farm to release it, then rerun "+
		"%s. Device Farm lets one client at a time use adb reverse on a device.", bridgeStopCommand, doctorRerun(inputs))
}

// checkAdbVersionSkew compares every local adb's protocol version with the remote adb server's.
// A local adb on another version gets its stop request refused, cannot start its own server on
// the daemon's port, and fails every command. The local step runs first under its own bound;
// the probe takes the rest of the row's budget.
func checkAdbVersionSkew(ctx context.Context, inputs Inputs, state *runState) Result {
	inputs = withSessionToCheck(inputs, state)
	adbs, hiddenAdb := inspectLocalAdbs(ctx, runtime.GOOS)
	probeCtx, cancel := withProbeBudget(ctx)
	defer cancel()
	hostVersion, err := inputs.Dialer.ProbeAdbServerVersion(probeCtx, state.endpoint)
	if err != nil {
		return Result{Status: Fail, Summary: versionProbeFailLead + err.Error(), Hint: versionProbeHint}
	}
	return adbVersionVerdict(adbs, hiddenAdb, hostVersion, inputs)
}

// adbVersionVerdict: FAIL when any adb differs from the remote adb server, else WARN when none
// was found, one is broken or one was found but not run, else PASS. A lone PATH adb is "local
// adb"; others are named by path.
func adbVersionVerdict(adbs []localAdb, hiddenAdb string, hostVersion int, inputs Inputs) Result {
	if len(adbs) == 0 {
		return Result{Status: Warn, Summary: fmt.Sprintf(noAdbSummary, hostVersion), Hint: noAdbHint(hiddenAdb, hostVersion, inputs)}
	}
	var mismatched, broken []localAdb
	for _, adb := range adbs {
		switch {
		case adb.err != nil:
			broken = append(broken, adb)
		case adb.version != hostVersion:
			mismatched = append(mismatched, adb)
		}
	}
	named := len(adbs) > 1 || !adbs[0].onPath
	switch {
	case len(mismatched) > 0:
		return Result{Status: Fail, Summary: skewSummary(mismatched, named, hostVersion), Hint: skewHint(mismatched, named, hostVersion, inputs)}
	case len(broken) > 0:
		problems := make([]string, len(broken))
		for index, adb := range broken {
			problems[index] = adb.err.Error()
		}
		return Result{Status: Warn, Summary: strings.Join(problems, "; "), Hint: brokenAdbHint(broken[0], hostVersion, inputs)}
	case hiddenAdb != "":
		return Result{Status: Warn, Summary: fmt.Sprintf("%s, but an adb at '%s' reached only through the current directory or a relative "+
			"PATH entry was not checked", matchSummary(adbs, hostVersion), safeValue(hiddenAdb)), Hint: hiddenAdbHint(hiddenAdb, inputs)}
	}
	return Result{Status: Pass, Summary: matchSummary(adbs, hostVersion)}
}

// matchSummary: the matching version, with the count when several adbs were found.
func matchSummary(adbs []localAdb, hostVersion int) string {
	if len(adbs) > 1 {
		return fmt.Sprintf("local adb 1.0.%d (%d found) matches the remote adb server", hostVersion, len(adbs))
	}
	return fmt.Sprintf("local adb 1.0.%d matches the remote adb server", hostVersion)
}

// hiddenAdbHint, for an adb the lookup found but did not run beside adbs it did: the PATH fix,
// the consequence.
func hiddenAdbHint(hiddenAdb string, inputs Inputs) string {
	return fmt.Sprintf("an adb exists at '%s', reached only through the current directory or a relative PATH entry, so it was not run "+
		"and its version is unknown. Add that directory to PATH by its absolute path if you meant that adb, then rerun %s. A local adb "+
		"on another version cannot connect through the bridge.", safeValue(hiddenAdb), doctorRerun(inputs))
}

// platformToolsExample: a leading-comma clause naming a platform-tools release whose adb speaks
// hostVersion; "" when none is known. Updated by hand when platform-tools moves adb's protocol version.
func platformToolsExample(hostVersion int) string {
	if hostVersion == 41 {
		return ", such as platform-tools 37"
	}
	return ""
}

// skewSummary names each mismatched adb, then the failing command family.
func skewSummary(mismatched []localAdb, named bool, hostVersion int) string {
	labels := make([]string, len(mismatched))
	for index, adb := range mismatched {
		labels[index] = fmt.Sprintf("local adb 1.0.%d%s", adb.version, adb.at(named))
	}
	verb := "differs"
	if len(mismatched) > 1 {
		verb = "differ"
	}
	return fmt.Sprintf("%s %s from the remote adb server (1.0.%d); every %s fails",
		joinAnd(labels), verb, hostVersion, failingCommand(mismatched, named))
}

// failingCommand is the command family a mismatch breaks: any adb command for a lone PATH adb,
// else a command from the named adb or adbs.
func failingCommand(mismatched []localAdb, named bool) string {
	switch {
	case !named:
		return "adb command"
	case len(mismatched) > 1:
		return "command from any of them"
	}
	return "command from that adb"
}

// skewHint: each version, an Android Studio clause when a lone PATH adb is also the SDK's, the
// platform-tools fix where the mismatched adbs live, the consequence until then.
func skewHint(mismatched []localAdb, named bool, hostVersion int, inputs Inputs) string {
	labels := make([]string, 0, len(mismatched)+1)
	sdkPath, pathOnly := "", false
	for _, adb := range mismatched {
		labels = append(labels, adb.hintLabel(named))
		if adb.sdkPath == "" {
			pathOnly = true
		} else if sdkPath == "" {
			sdkPath = adb.sdkPath
		}
	}
	labels = append(labels, fmt.Sprintf("the remote adb server is 1.0.%d", hostVersion))
	command := failingCommand(mismatched, named)
	cause := fmt.Sprintf("%s, so every %s fails: that adb tries to start its own adb server on the bridge's port and cannot",
		joinAnd(labels), command)
	fix := fmt.Sprintf("Install platform-tools whose adb is 1.0.%d%s, or put such an adb first on PATH", hostVersion,
		platformToolsExample(hostVersion))
	if sdkPath != "" {
		if !named {
			cause += fmt.Sprintf("; Android Studio runs the SDK's adb at '%s', so Studio lists no device while that adb is on "+
				"another version", safeValue(sdkPath))
		}
		alsoPath := ""
		if pathOnly {
			alsoPath = ", and put such an adb first on PATH"
		}
		fix = fmt.Sprintf("Update Android SDK Platform-Tools in Android Studio's SDK Manager to a release whose adb is 1.0.%d%s%s",
			hostVersion, platformToolsExample(hostVersion), alsoPath)
	}
	return fmt.Sprintf("%s. %s, then rerun %s. Until then no %s reaches the device. The remote adb server is not affected.",
		cause, fix, doctorRerun(inputs), command)
}

// brokenAdbHint: the failed command, its by-hand run, the rerun, the remote adb server's version.
func brokenAdbHint(adb localAdb, hostVersion int, inputs Inputs) string {
	format := adbFailedHint
	if errors.Is(adb.err, errNoAdbVersionLine) {
		format = adbNoVersionHint
	}
	return fmt.Sprintf(format, adb.versionCommand(), doctorRerun(inputs), hostVersion)
}

// noAdbHint: the cause (and any adb found only through a relative entry), the PATH or install fix,
// the consequence.
func noAdbHint(hiddenAdb string, hostVersion int, inputs Inputs) string {
	hidden, action := "", "Install"
	if hiddenAdb != "" {
		hidden = fmt.Sprintf("; an adb exists at '%s', reached only through the current directory or a relative PATH entry, so it "+
			"was not run", safeValue(hiddenAdb))
		action = "Add that directory to PATH by its absolute path if you meant that adb, or install"
	}
	return fmt.Sprintf("adb was not found on PATH or in an Android SDK (%s), so the doctor could not check that its version "+
		"matches the remote adb server (1.0.%d)%s. %s Android platform-tools whose adb is 1.0.%d%s, then add its directory to "+
		"PATH and rerun %s. A local adb on another version cannot connect through the bridge.", sdkLocations,
		hostVersion, hidden, action, hostVersion, platformToolsExample(hostVersion), doctorRerun(inputs))
}

// joinAnd lists items as "a", "a and b" or "a, b and c".
func joinAnd(items []string) string {
	if len(items) <= 1 {
		return strings.Join(items, "")
	}
	return strings.Join(items[:len(items)-1], ", ") + " and " + items[len(items)-1]
}

// localAdb is one adb binary on this machine: how the lookup reached it, then its protocol
// version or why none was read.
type localAdb struct {
	path    string // the first path that reached it
	onPath  bool   // PATH reached it
	sdkPath string // the SDK path that reached it, if any: the adb Android Studio runs
	version int
	err     error
}

// at renders " at '<path>'", or "" when not named.
func (adb localAdb) at(named bool) string {
	if !named {
		return ""
	}
	return fmt.Sprintf(" at '%s'", safeValue(adb.path))
}

// hintLabel names the adb and its version for a hint: a lone PATH adb is "the local adb", an SDK
// adb is Android Studio's, any other is named by path.
func (adb localAdb) hintLabel(named bool) string {
	switch {
	case !named:
		return fmt.Sprintf("the local adb is 1.0.%d", adb.version)
	case adb.sdkPath != "":
		return fmt.Sprintf("Android Studio's adb at '%s' is 1.0.%d", safeValue(adb.sdkPath), adb.version)
	}
	return fmt.Sprintf("the local adb at '%s' is 1.0.%d", safeValue(adb.path), adb.version)
}

// versionCommand quotes the `adb version` run on this adb.
func (adb localAdb) versionCommand() string {
	return fmt.Sprintf("'%s version'", safeValue(adb.path))
}

// inspectLocalAdbs runs `adb version` on every found adb in parallel: all share one bound.
func inspectLocalAdbs(ctx context.Context, goos string) ([]localAdb, string) {
	adbs, hiddenAdb := findLocalAdbs(goos)
	var pending sync.WaitGroup
	for index := range adbs {
		pending.Go(func() { adbs[index].version, adbs[index].err = localAdbVersion(ctx, adbs[index]) })
	}
	pending.Wait()
	return adbs, hiddenAdb
}

// findLocalAdbs lists local adbs, one entry per binary: the PATH adb, then sdkAdbCandidates.
// hiddenAdb is an adb reached only through the current directory or a relative PATH entry: never
// run, its absolute path kept for the hint.
func findLocalAdbs(goos string) (adbs []localAdb, hiddenAdb string) {
	indexByBinary := map[string]int{}
	add := func(path string, fromSDK bool) {
		info, err := os.Stat(path)
		if err != nil || info.IsDir() {
			return
		}
		binary, err := filepath.EvalSymlinks(path)
		if err != nil {
			return
		}
		index, seen := indexByBinary[binary]
		if !seen {
			index = len(adbs)
			indexByBinary[binary] = index
			adbs = append(adbs, localAdb{path: path, onPath: !fromSDK})
		}
		if fromSDK && adbs[index].sdkPath == "" {
			adbs[index].sdkPath = path
		}
	}
	pathAdb, err := exec.LookPath("adb")
	switch {
	case errors.Is(err, exec.ErrDot):
		hiddenAdb = pathAdb
		if absolute, absErr := filepath.Abs(pathAdb); absErr == nil {
			hiddenAdb = absolute
		}
	case err == nil:
		add(pathAdb, false)
	}
	for _, candidate := range sdkAdbCandidates(goos) {
		add(candidate, true)
	}
	if binary, err := filepath.EvalSymlinks(hiddenAdb); err == nil {
		if _, ran := indexByBinary[binary]; ran {
			hiddenAdb = "" // an SDK path reached the same binary, so it ran after all
		}
	}
	return adbs, hiddenAdb
}

// sdkAdbCandidates: platform-tools/adb under ANDROID_HOME and ANDROID_SDK_ROOT, or with neither
// set under Android Studio's default SDK for this OS. A relative root is skipped: it would name
// an adb by the current directory. Candidates need not exist; findLocalAdbs stats them.
func sdkAdbCandidates(goos string) []string {
	roots := []string{os.Getenv("ANDROID_HOME"), os.Getenv("ANDROID_SDK_ROOT")}
	if roots[0] == "" && roots[1] == "" {
		roots = []string{defaultSDKRoot(goos)}
	}
	binary := "adb"
	if goos == "windows" {
		binary = "adb.exe"
	}
	var candidates []string
	for _, root := range roots {
		if filepath.IsAbs(root) {
			candidates = append(candidates, filepath.Join(root, "platform-tools", binary))
		}
	}
	return candidates
}

// defaultSDKRoot is Android Studio's default SDK path; "" when the base directory is unknown.
func defaultSDKRoot(goos string) string {
	if goos == "windows" {
		if local := os.Getenv("LOCALAPPDATA"); local != "" {
			return filepath.Join(local, "Android", "Sdk")
		}
		return ""
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	if goos == "darwin" {
		return filepath.Join(home, "Library", "Android", "sdk")
	}
	return filepath.Join(home, "Android", "Sdk")
}

// localAdbVersion runs `adb version` on adb under its own bound and reads the protocol version.
func localAdbVersion(ctx context.Context, adb localAdb) (int, error) {
	versionCtx, cancel := context.WithTimeout(ctx, adbVersionTimeout)
	defer cancel()
	command := exec.CommandContext(versionCtx, adb.path, "version")
	command.WaitDelay = pipeDrainDelay // a child adb left holding the pipes must not hold the row
	output, err := command.Output()
	switch {
	case errors.Is(err, exec.ErrWaitDelay):
		// adb exited and a child kept its pipes; the output is whole, whatever the clock says.
	case versionCtx.Err() != nil:
		return 0, fmt.Errorf("%s did not finish in %s", adb.versionCommand(), adbVersionTimeout)
	case err != nil:
		return 0, fmt.Errorf("%s failed: %s", adb.versionCommand(), safeValue(err.Error())) // the launch error repeats the path
	}
	match := adbVersionPattern.FindSubmatch(output)
	if match == nil {
		return 0, noVersionLine(adb, output)
	}
	version, err := strconv.Atoi(string(match[1]))
	if err != nil {
		return 0, noVersionLine(adb, output)
	}
	return version, nil
}

// noVersionLine: adb printed nothing the version pattern reads; its first line is quoted.
func noVersionLine(adb localAdb, output []byte) error {
	summaryLine, _, _ := strings.Cut(string(output), "\n")
	return fmt.Errorf("%s %w: %q", adb.versionCommand(), errNoAdbVersionLine, summaryLine)
}
