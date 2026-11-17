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
	"github.com/aws/device-connect-devicefarm/internal/daemon"
	"github.com/aws/device-connect-devicefarm/internal/session"
	"github.com/aws/device-connect-devicefarm/internal/wording"
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
var errNoAdbVersionLine = errors.New(wording.DoctorDetailAdbNoVersionLine)

// doctorRerun quotes the doctor rerun with the flags this run used.
func doctorRerun(inputs Inputs) string {
	return "'" + daemon.DoctorCommand(safeValue(inputs.Config.ListenAddr), inputs.Config.ListenPort, safeValue(inputs.SessionARN)) + "'"
}

// Session hints. Each quotes the passed ARN.

func invalidArnHint(inputs Inputs) string {
	return fmt.Sprintf(wording.DoctorHintARNInvalidFormat, safeValue(inputs.SessionARN))
}

func accessDeniedHint(inputs Inputs) string {
	return fmt.Sprintf(wording.DoctorHintSessionAccessDeniedFormat, safeValue(inputs.SessionARN))
}

func sessionGoneHint(inputs Inputs) string {
	return fmt.Sprintf(wording.DoctorHintSessionNotFoundFormat, safeValue(inputs.SessionARN))
}

func unsupportedDeviceHint(inputs Inputs) string {
	return fmt.Sprintf(wording.DoctorHintAndroidTooOldFormat, safeValue(inputs.SessionARN))
}

// nonAndroidDeviceHint: the device is not Android. platform is UnsupportedDeviceError.Platform.
func nonAndroidDeviceHint(inputs Inputs, platform string) string {
	return fmt.Sprintf(wording.DoctorHintNotAndroidFormat, safeValue(inputs.SessionARN), platform, platform)
}

func sessionEndedHint(inputs Inputs) string {
	return fmt.Sprintf(wording.DoctorHintSessionEndedFormat, safeValue(inputs.SessionARN))
}

// notRunningHint: the session is not RUNNING yet, so a wait helps. status is NoEndpointError.Status.
func notRunningHint(inputs Inputs, status string) string {
	return fmt.Sprintf(wording.DoctorHintSessionNotReadyFormat, status, safeValue(inputs.SessionARN), doctorRerun(inputs))
}

// noEndpointHint: the service says RUNNING but gives no endpoint, so a wait does not help.
func noEndpointHint(inputs Inputs) string {
	return fmt.Sprintf(wording.DoctorHintSessionNoEndpointFormat, safeValue(inputs.SessionARN), doctorRerun(inputs))
}

func requestRejectedHint(inputs Inputs) string {
	return fmt.Sprintf(wording.DoctorHintRequestRejectedFormat, safeValue(inputs.SessionARN), doctorRerun(inputs))
}

func sessionNetworkHint(inputs Inputs) string {
	return fmt.Sprintf(wording.DoctorHintSessionCallFailedFormat, safeValue(inputs.SessionARN))
}

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
		{ID: checkCredentials, Title: wording.DoctorTitleAWSCredentials, Category: CategoryAWS, Timeout: remoteTimeout,
			Run: func(ctx context.Context) Result { return checkCredentialsResolve(ctx, inputs) }},
		{ID: checkSession, Title: wording.DoctorTitleRemoteAccessSession, Category: CategoryAWS, Needs: []string{checkCredentials}, Timeout: remoteTimeout,
			Run: func(ctx context.Context) Result { return checkSessionRunning(ctx, inputs, state) }},
		{ID: checkRelaySession, Title: wording.DoctorTitleAdbSession, Category: CategoryNetwork, Needs: []string{checkSession}, Timeout: remoteTimeout,
			Run: func(ctx context.Context) Result { return checkRelaySessionOpens(ctx, inputs, state) }},
		{ID: checkReattach, Title: wording.DoctorTitleAdbSessionReconnect, Category: CategoryNetwork, Needs: []string{checkRelaySession}, Timeout: reattachTimeout,
			Run: func(ctx context.Context) Result { return checkReattachAcked(ctx, inputs, state) }},
		{ID: checkReverse, Title: wording.DoctorTitleAdbReverse, Category: CategoryNetwork, Needs: []string{checkSession, checkDaemon}, Timeout: remoteTimeout,
			Run: func(ctx context.Context) Result { return checkReverseNotifier(ctx, inputs, state) }},
		{ID: checkAdbVersion, Title: wording.DoctorTitleAdbVersions, Category: CategoryDependencies, Needs: []string{checkRelaySession},
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
		return Result{Status: Fail, Summary: wording.DoctorDetailNoCredentialsLead + err.Error(), Hint: wording.DoctorHintNoCredentials}
	}
	return Result{Status: Pass, Summary: fmt.Sprintf(wording.DoctorDetailCredentialsSourceFormat, summary.Source)}
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
			return Result{Status: Skip, Summary: fmt.Sprintf(wording.DoctorDetailSeveralDevicesFormat, attached)}
		}
		return Result{Status: Skip, Summary: wording.DoctorDetailSessionARNMissing}
	}
	inputs.SessionARN = sessionARN
	lookup := state.lookUpSession(ctx, inputs)
	switch {
	case lookup.noClient:
		return Result{Status: Fail, Summary: lookup.err.Error(), Hint: wording.DoctorHintNoCredentials}
	case lookup.err != nil:
		return Result{Status: Fail, Summary: lookup.err.Error(), Hint: sessionHint(lookup.err, inputs)}
	}
	info := lookup.info
	state.endpoint, state.returnedARN = info.Endpoint, info.ReturnedARN(sessionARN)
	if fromDaemon {
		return Result{Status: Pass, Summary: fmt.Sprintf(wording.DoctorDetailDaemonSessionRunningFormat, info.Label, safeValue(sessionARN))}
	}
	return Result{Status: Pass, Summary: fmt.Sprintf(wording.DoctorDetailSessionRunningFormat, info.Label)}
}

// sessionHint picks the fix by the error's class.
func sessionHint(err error, inputs Inputs) string {
	var refusal session.UnsupportedDeviceError
	var noEndpoint *session.NoEndpointError
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
	case errors.As(err, &noEndpoint):
		if noEndpoint.Running() {
			return noEndpointHint(inputs)
		}
		return notRunningHint(inputs, noEndpoint.Status)
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
		return Result{Status: Fail, Summary: wording.DoctorDetailAdbSessionFailedLead + err.Error(), Hint: relayHint(inputs, err)}
	}
	state.sessionUUID = sessionUUID
	return Result{Status: Pass, Summary: fmt.Sprintf(wording.DoctorDetailAdbSessionOpenedFormat, sessionUUID)}
}

// checkReattachAcked dials again with the probed UUID; the server must resume the session, as
// every websocket swap needs.
func checkReattachAcked(ctx context.Context, inputs Inputs, state *runState) Result {
	inputs = withSessionToCheck(inputs, state)
	probeCtx, cancel := withProbeBudget(ctx)
	defer cancel()
	if err := inputs.Dialer.ProbeReattach(probeCtx, state.endpoint, state.sessionUUID); err != nil {
		return Result{Status: Fail, Summary: fmt.Sprintf(wording.DoctorDetailReconnectFailedFormat, state.sessionUUID, err),
			Hint: reattachHint(inputs)}
	}
	return Result{Status: Pass, Summary: wording.DoctorDetailReconnectedLead + state.sessionUUID}
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
		return Result{Status: Pass, Summary: wording.DoctorDetailReverseOnInDaemon}
	}
	probeCtx, cancel := withProbeBudget(ctx)
	defer cancel()
	err := inputs.Dialer.ProbeNotifier(probeCtx, state.endpoint)
	switch {
	case errors.Is(err, bridge.ErrNotifierSlotTaken):
		return Result{Status: Fail, Summary: wording.DoctorDetailReverseInUse, Hint: slotHeldHint(inputs)}
	case err != nil:
		return Result{Status: Fail, Summary: wording.DoctorDetailReverseUnavailableLead + err.Error(), Hint: relayHint(inputs, err)}
	}
	return Result{Status: Pass, Summary: wording.DoctorDetailReverseAvailable}
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

// relayHint is the hint for a failed relay probe; the summary names the failure. For a failed
// dial it names the host and the port to allow on the network. For any other failure it says to
// rerun once. With a proxy set, it adds that the websocket connection ignores it.
func relayHint(inputs Inputs, probeErr error) string {
	rerun := doctorRerun(inputs)
	hint := fmt.Sprintf(wording.DoctorHintAdbConnectionFailedFormat, rerun)
	if host, port := bridge.SortConnectionError(probeErr, inputs.SessionARN).DialedHostPort(); host != "" {
		hint = fmt.Sprintf(wording.DoctorHintWebSocketFailedFormat, safeValue(host), port, rerun)
	}
	if os.Getenv("HTTPS_PROXY") != "" || os.Getenv("https_proxy") != "" {
		hint += " " + wording.DoctorHintProxyIgnored
	}
	return hint
}

func reattachHint(inputs Inputs) string {
	return fmt.Sprintf(wording.DoctorHintReconnectFailedFormat, doctorRerun(inputs))
}

func slotHeldHint(inputs Inputs) string {
	return fmt.Sprintf(wording.DoctorHintReverseInUseFormat, doctorRerun(inputs))
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
		return Result{Status: Fail, Summary: wording.DoctorDetailRemoteAdbSilentLead + err.Error(), Hint: wording.DoctorHintRemoteAdbSilent}
	}
	return adbVersionVerdict(adbs, hiddenAdb, hostVersion, inputs)
}

// adbVersionVerdict: FAIL when any adb differs from the remote adb server, else WARN when none
// was found, one is broken or one was found but not run, else PASS. A lone PATH adb is "local
// adb"; others are named by path.
func adbVersionVerdict(adbs []localAdb, hiddenAdb string, hostVersion int, inputs Inputs) Result {
	if len(adbs) == 0 {
		return Result{Status: Warn, Summary: fmt.Sprintf(wording.DoctorDetailNoAdbFormat, hostVersion), Hint: noAdbHint(hiddenAdb, hostVersion, inputs)}
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
		return Result{Status: Warn, Summary: fmt.Sprintf(wording.DoctorDetailAdbNotRunFormat, matchSummary(adbs, hostVersion), safeValue(hiddenAdb)),
			Hint: hiddenAdbHint(hiddenAdb, inputs)}
	}
	return Result{Status: Pass, Summary: matchSummary(adbs, hostVersion)}
}

// matchSummary: the matching version, with the count when several adbs were found.
func matchSummary(adbs []localAdb, hostVersion int) string {
	if len(adbs) > 1 {
		return fmt.Sprintf(wording.DoctorDetailSeveralAdbsMatchFormat, hostVersion, len(adbs))
	}
	return fmt.Sprintf(wording.DoctorDetailAdbMatchesFormat, hostVersion)
}

// hiddenAdbHint, for an adb the lookup found but did not run beside adbs it did: the PATH fix,
// the consequence.
func hiddenAdbHint(hiddenAdb string, inputs Inputs) string {
	return fmt.Sprintf(wording.DoctorHintAdbNotRunFormat, safeValue(hiddenAdb), doctorRerun(inputs))
}

// platformToolsExample: a leading-comma clause naming a platform-tools release whose adb speaks
// hostVersion; "" when none is known. Updated by hand when platform-tools moves adb's protocol version.
func platformToolsExample(hostVersion int) string {
	if hostVersion == 41 {
		return wording.DoctorHintPlatformToolsClause
	}
	return ""
}

// skewSummary names each mismatched adb, then the failing command family.
func skewSummary(mismatched []localAdb, named bool, hostVersion int) string {
	labels := make([]string, len(mismatched))
	for index, adb := range mismatched {
		labels[index] = adb.summaryLabel(named)
	}
	verb := wording.DoctorDetailMismatchDiffers
	if len(mismatched) > 1 {
		verb = wording.DoctorDetailMismatchDiffer
	}
	return fmt.Sprintf(wording.DoctorDetailMismatchFormat, joinAnd(labels), verb, hostVersion, failingCommand(mismatched, named))
}

// failingCommand is the command family a mismatch breaks: any adb command for a lone PATH adb,
// else a command from the named adb or adbs.
func failingCommand(mismatched []localAdb, named bool) string {
	switch {
	case !named:
		return wording.DoctorMismatchAdbCommandClause
	case len(mismatched) > 1:
		return wording.DoctorMismatchFromAnyOfThemClause
	}
	return wording.DoctorMismatchFromThatAdbClause
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
	labels = append(labels, fmt.Sprintf(wording.DoctorHintMismatchRemoteAdbFormat, hostVersion))
	command := failingCommand(mismatched, named)
	cause := fmt.Sprintf(wording.DoctorHintMismatchCauseFormat, joinAnd(labels), command)
	fix := fmt.Sprintf(wording.DoctorHintMismatchInstallFormat, hostVersion, platformToolsExample(hostVersion))
	if sdkPath != "" {
		if !named {
			cause += fmt.Sprintf(wording.DoctorHintMismatchStudioClauseFormat, safeValue(sdkPath))
		}
		alsoPath := ""
		if pathOnly {
			alsoPath = wording.DoctorHintMismatchPathClause
		}
		fix = fmt.Sprintf(wording.DoctorHintMismatchUpdateSDKFormat, hostVersion, platformToolsExample(hostVersion), alsoPath)
	}
	return fmt.Sprintf(wording.DoctorHintMismatchFormat, cause, fix, doctorRerun(inputs), command)
}

// brokenAdbHint: the failed command, its by-hand run, the rerun, the remote adb server's version.
func brokenAdbHint(adb localAdb, hostVersion int, inputs Inputs) string {
	format := wording.DoctorHintAdbFailedFormat
	if errors.Is(adb.err, errNoAdbVersionLine) {
		format = wording.DoctorHintAdbNoVersionFormat
	}
	return fmt.Sprintf(format, adb.versionCommand(), doctorRerun(inputs), hostVersion)
}

// noAdbHint: the cause (and any adb found only through a relative entry), the PATH or install fix,
// the consequence.
func noAdbHint(hiddenAdb string, hostVersion int, inputs Inputs) string {
	hidden, action := "", wording.DoctorHintNoAdbInstallLead
	if hiddenAdb != "" {
		hidden = fmt.Sprintf(wording.DoctorHintNoAdbNotRunClauseFormat, safeValue(hiddenAdb))
		action = wording.DoctorHintNoAdbAddOrInstallLead
	}
	return fmt.Sprintf(wording.DoctorHintNoAdbFormat,
		hostVersion, hidden, action, hostVersion, platformToolsExample(hostVersion), doctorRerun(inputs))
}

// joinAnd lists items as "a", "a and b", or "a, b, and c".
func joinAnd(items []string) string {
	if len(items) <= 2 {
		return strings.Join(items, wording.DoctorMismatchListAnd)
	}
	return strings.Join(items[:len(items)-1], ", ") + wording.DoctorMismatchListSerialAnd + items[len(items)-1]
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

// summaryLabel names the adb and its version for a summary: by path when named.
func (adb localAdb) summaryLabel(named bool) string {
	if !named {
		return fmt.Sprintf(wording.DoctorDetailMismatchLocalAdbFormat, adb.version)
	}
	return fmt.Sprintf(wording.DoctorDetailMismatchLocalAdbAtPathFormat, adb.version, safeValue(adb.path))
}

// hintLabel names the adb and its version for a hint: a lone PATH adb is "the local adb", an SDK
// adb is Android Studio's, any other is named by path.
func (adb localAdb) hintLabel(named bool) string {
	switch {
	case !named:
		return fmt.Sprintf(wording.DoctorHintMismatchLocalAdbFormat, adb.version)
	case adb.sdkPath != "":
		return fmt.Sprintf(wording.DoctorHintMismatchStudioAdbFormat, safeValue(adb.sdkPath), adb.version)
	}
	return fmt.Sprintf(wording.DoctorHintMismatchLocalAdbAtPathFormat, safeValue(adb.path), adb.version)
}

// versionCommand quotes the `adb version` run on this adb.
func (adb localAdb) versionCommand() string {
	return fmt.Sprintf(wording.DoctorAdbVersionCommandFormat, safeValue(adb.path))
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
		return 0, fmt.Errorf(wording.DoctorDetailAdbTimedOutFormat, adb.versionCommand(), adbVersionTimeout)
	case err != nil:
		return 0, fmt.Errorf(wording.DoctorDetailAdbFailedFormat, adb.versionCommand(), safeValue(err.Error())) // the launch error repeats the path
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
