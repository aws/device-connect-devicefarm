// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

// Package wording holds the text this program shows a person: help, command
// output, failures, doctor rows and the replies the adb command line prints.
// Constants only, so every package can import it. A name ending in Format
// holds fmt verbs; any other name is plain text. A verb takes a runtime value
// only: fixed wording is joined into its sentence here. A fragment that starts
// a sentence ends in Lead, one that ends it in Tail, one from the middle in
// Clause.
package wording

// The program's name.
const (
	BinaryName = "devicefarm-connect"
)

// Commands and flags quoted inside fixes and hints.
const (
	BridgeStartCommand = BinaryName + " bridge start"
	BridgeStopCommand  = BinaryName + " bridge stop"
	DoctorCommand      = BinaryName + " doctor"
	// Flags RestartCommand appends. The reverse flag needs DoctorHintReverseCaveat beside it.
	RestartReverseFlag     = " --allow-adb-reverse"
	RestartSessionFlagLead = " --session-arn "
	// The restart, quoted, for a text that cannot read the bridge's flags.
	StartWithYourOptionsClause = "'" + BridgeStartCommand + "' with the options that you started it with"
	// The address follows. A quoted command names it only off the default address.
	ListenAddrFlagLead = " --listen-addr "
)

// Help text: the root, then each command.
const (
	HelpRoot     = "Bridge a local adb client to an AWS Device Farm remote access device"
	HelpRootLong = BinaryName + " connects a local adb client to a device in an\n" +
		"AWS Device Farm remote access session. A background bridge listens on a\n" +
		"local TCP port in place of the adb server and carries each adb connection\n" +
		"over the session's WebSocket to the device.\n" +
		"\n" +
		"  device add      Attach a device. Starts the bridge when none runs.\n" +
		"  device list     Show the attached device. Starts the bridge when none runs.\n" +
		"  device remove   Detach the attached device.\n" +
		"  bridge start    Start the bridge with custom options, such as --port.\n" +
		"  bridge stop     Stop the bridge.\n" +
		"  doctor          Check this machine, the AWS credentials, and a session."
	HelpUsage = "Help about any command"

	HelpBridge       = "Manage the background bridge"
	HelpBridgeStart  = "Start the bridge, optionally attaching a Device Farm remote access device"
	HelpBridgeStop   = "Stop the running bridge"
	HelpBridgeStatus = "Report whether the bridge is running and how many devices are attached"

	HelpDevice         = "Attach, list, or detach Device Farm remote access devices"
	HelpDeviceAdd      = "Attach a Device Farm remote access device to the bridge"
	HelpDeviceAddLong  = HelpDeviceAdd + ".\n\n" + HelpBridgeAutoStartNote
	HelpDeviceList     = "List the Device Farm remote access devices attached to the bridge"
	HelpDeviceListLong = HelpDeviceList + ".\n\nOne table row per device: the device name, the adb serial, how long it has been\n" +
		"attached, when its session started, and how long until the session expires. The\n" +
		"table fits the terminal width, or COLUMNS when stdout is not a terminal. --list\n" +
		"prints one tab-separated line per device instead, the session ARN first; --json\n" +
		"prints one JSON object.\n\n" + HelpBridgeAutoStartNote
	HelpDeviceRemove     = "Detach the attached Device Farm remote access device from the bridge"
	HelpDeviceRemoveLong = HelpDeviceRemove + ".\n\nThis command needs a running bridge."
	// Ends the long help of each command that starts the bridge.
	HelpBridgeAutoStartNote = "If the bridge is not running, this command starts it with default\n" +
		"settings, as 'adb' starts its own server. To pick a port or other options,\n" +
		"run 'bridge start' first."

	HelpDoctor     = "Check this machine, the AWS credentials, and a session before 'bridge start'"
	HelpDoctorLong = `Run readiness checks for 'bridge start', 'device add', an adb connection, a
reconnect to the device, 'adb forward', and 'adb reverse', and print one table
row per category:

  Local machine  the working directory, the bridge, the ports it needs
  AWS            this shell's AWS credentials and the remote access session
  Network        the device's adb connection, its reconnect, and adb reverse
  Dependencies   the adb on this machine against the device's adb server

A row is FAIL when a check in it fails, WARN when one works with a caveat, SKIP
when none run, else PASS. The Details column names each check that did not pass
and why. A "What to do" list under the table gives the fix for each WARN or
FAIL, with the command to run. The table fits the terminal width, or COLUMNS
when stdout is not a terminal. --verbose prints one line per check instead;
--json prints one compact JSON object. Exit code 0 unless a check is FAIL.
Without --session-arn the doctor checks the session of the one device the
running bridge holds; with no bridge, no device, or several devices, the
session, the connection to the device, and the adb versions are not checked.
Run the doctor from the shell that will run 'bridge start': the bridge
inherits that shell's environment. A running bridge is reported, not changed.

Side effects:
  creates the client's working directory if absent; forgets a bridge that no
  longer runs
  binds and frees the listen address when no bridge holds it, and one port on
  127.0.0.1 for 'adb forward'
  asks whatever holds the listen address for its adb version
  makes one GetRemoteAccessSession call
  opens one short connection to the AWS Device Farm device per Network and
  Dependencies check
  runs 'adb version' on each adb it finds on PATH, in ANDROID_HOME or
  ANDROID_SDK_ROOT, or in Android Studio's default SDK location; none starts an
  adb server
Starts no bridge; installs no forward or reverse rule. Each check has its own
timeout; a run in which every check times out ends in under two minutes.`

	HelpDaemon = "Run the bridge in the foreground (internal; used by 'bridge start')"
)

// Flag usage.
const (
	FlagDebugUsage           = "print this command's log records on stderr; a bridge it starts also logs at debug level"
	FlagListenAddrUsage      = "address to listen on"
	FlagPortUsage            = "local TCP port to listen on for adb clients"
	FlagAllowAdbReverseUsage = "allow device-originated reverse connections ('adb reverse'); off by default"
	FlagLogUsage             = "bridge log file path (defaults to the per-user runtime directory)"

	FlagSessionARNUsage       = "AWS Device Farm remote access session ARN"
	FlagSessionARNStartUsage  = FlagSessionARNUsage + " (optional; attaches the device on start)"
	FlagSessionARNRemoveUsage = FlagSessionARNUsage + " (optional; without it, the attached device is detached)"
	FlagSessionARNDoctorUsage = FlagSessionARNUsage + " (optional; defaults to the one attached session; also checks the session, the connection to the device, and the adb versions)"

	FlagBridgeStatusJSONUsage = "print one JSON object instead of the report line"
	FlagListUsage             = "print one tab-separated line per device, the session ARN first, instead of the table"
	FlagDeviceListJSONUsage   = "print one JSON object instead of the table"
	FlagVerboseUsage          = "print one line per check, with its ID, instead of the table"
	FlagDoctorJSONUsage       = "print one JSON object instead of the table; same exit code"
)

// What bridge and device commands print on success.
const (
	OutputBridgeStarting = "* bridge not running; starting it now"
	// bridge start over a running bridge: "* a bridge was running (PID 42) and was stopped; its attached devices, ...".
	OutputRestartStoppedFormat = "* a bridge was running (PID %d) and was stopped"
	OutputRestartKilledFormat  = "* a bridge was running (PID %d), did not stop cleanly, and was killed"
	// The device keeps a reverse rule until the next attach with reverse on clears it; it reaches no bridge meanwhile.
	OutputRestartDroppedTail = "; its attached devices and forward rules were dropped, and its reverse rules no longer reach this machine"
	// Verb: the address of the adb server that the start stopped.
	OutputAdbServerStoppedFormat = "* stopped the adb server on %s (via 'adb kill-server')"
	// Verbs: PID, log file path.
	OutputBridgeStartedFormat       = "* bridge started (PID %d); log file: %s"
	OutputListeningNoDeviceFormat   = BinaryName + " listening on port %d with no device attached; use 'device add' to connect one"
	OutputListeningWithDeviceFormat = BinaryName + " listening on port %d with connected device: %s"
	// After an add: the service returned another ARN than the typed one. Verb: that ARN.
	OutputServiceARNFormat = "* the service names this session %s; use that ARN with 'device remove'"
	OutputStopKilledFormat = "* the bridge (PID %d) did not stop cleanly and was killed"
	OutputStopped          = BinaryName + " stopped"
	// bridge stop with no daemon.
	OutputNotListening = BinaryName + " not listening"
	// bridge status with no daemon.
	OutputDaemonNotRunning = BinaryName + " bridge is not running"
	// "... (PID 42), no device attached; the last device detached 3 minutes ago: <reason>"
	OutputDaemonRunningNoDeviceFormat = BinaryName + " bridge is running (PID %d), no device attached"
	OutputLastDetachFormat            = "the last device detached %s ago: %s"
	// "... (PID 42), 1 device attached; run 'device list' for details"
	OutputDaemonRunningDevicesFormat = BinaryName + " bridge is running (PID %d), %s attached; run 'device list' for details"
	OutputDeviceNoun                 = "device"
	OutputNoDeviceAttached           = "no device attached; use 'device add' to connect one"
	OutputRemovedFormat              = "Removed %s from the bridge"
	// Age units, here and in the device list. Plural adds "s".
	OutputUnitSecond = "second"
	OutputUnitMinute = "minute"
	OutputUnitHour   = "hour"
)

// Device list: table columns, cell words, and the words of the unlabeled --list values.
const (
	OutputColumnDevice   = "Device"
	OutputColumnSerial   = "Serial"
	OutputColumnAttached = "Attached"
	OutputColumnSession  = "Session"
	OutputUnknownSerial  = "unknown"
	// "started 25 minutes ago", "started unknown"
	OutputStartedLead = "started "
	OutputAgoTail     = " ago"
	OutputUnknownTime = "unknown"
	// "expires in 2 hours 5 minutes"
	OutputExpiresInLead = "expires in "
	OutputExpired       = "expired"
	OutputExpiryUnknown = "expiry unknown"
	// The --list expiry value: "in 2 hours 5 minutes"
	OutputExpiryInLead = "in "
)

// The failure block, in print order.
const (
	ProblemLineFormat = "%s: error: %s\n"
	CauseLineFormat   = "  cause: %s\n"
	FixLineFormat     = "  fix:   %s\n"
)

// Command problems, fixes and causes.
const (
	ProblemWorkingDirectory         = "cannot use the working directory"
	ProblemStartFailed              = "could not start the bridge"
	ProblemStopFailed               = "could not stop the bridge"
	ProblemStopRunningFailed        = "could not stop the running bridge"
	ProblemBridgeNotListening       = "the bridge is not listening"
	ProblemBridgeNoAnswer           = "the bridge did not answer"
	ProblemStartedButNotAttached    = "the bridge started but could not attach the device"
	ProblemAttachFailed             = "could not attach the device"
	ProblemListFailed               = "could not list the devices"
	ProblemDetachFailed             = "could not detach the device"
	ProblemSessionARNRequired       = "--session-arn is required"
	ProblemSessionARNBlank          = "--session-arn is blank"
	ProblemSessionARNBlankRemove    = ProblemSessionARNBlank + "; omit it to detach the attached device"
	ProblemListAndJSON              = "--list and --json cannot be used together"
	ProblemDoctorOneCheckFailed     = "doctor: 1 check failed"
	ProblemDoctorChecksFailedFormat = "doctor: %d checks failed"
	FixRunHelpFormat                = "run '%s --help' for usage"
	CauseRuntimeDirOwnerFormat      = "working directory %q is owned by UID %d, not %d"
	FixRuntimeDirEnvVarFormat       = "set %s to a private directory"
)

// Control refusals: the bridge's reply, printed as the command's cause.
const (
	RefusalMalformedRequest      = "malformed request"
	RefusalUnknownOpFormat       = "unknown control operation %q"
	RefusalDifferentDeviceFormat = "a different device is already attached (%s)"
	RefusalNoDevice              = "no device is attached"
	RefusalDeviceMismatch        = "the attached device does not match the requested session"
)

// What failed on the connection to a device: the cause of a refused add. adb prints the close
// texts after "error: ". A reason is the service's own text.
const (
	// The service refused the session before any reply. Verb: its reason.
	RefusalSessionRefusedFormat = "Device Farm refused the connection to the device: %s"
	// A close with a reason. Verbs: the reason, the close code.
	RefusalServiceRefusedFormat = RefusalSessionRefusedFormat + " (close code %d)"
	// A close with the catch-all code (1011), then a close with no reason.
	RefusalServiceClosedFormat         = "Device Farm closed the connection to the device: %s (close code %d)"
	RefusalServiceClosedNoReasonFormat = "Device Farm closed the connection to the device (close code %d)"
	RefusalConnectionDropped           = "the connection to Device Farm ended before the device answered"
	// Verb: how long the bridge waits for one reply.
	RefusalReplyTimeoutFormat = "the device did not answer within %s"
	RefusalAttachDeadline     = "the attach ran out of time before the device answered"
	// Verbs: the host, the system's error text.
	RefusalUnreachableFormat = "cannot connect to Device Farm at %s: %s"
	// The service answered the connection request with an HTTP error. Verb: the host.
	RefusalUpgradeRefusedAtHostFormat = "Device Farm at %s rejected the connection request"
	// The same refusal with no host of its own; adb and the doctor print it. The status is not
	// known, so the text names the possible causes.
	RefusalUpgradeRefused = "Device Farm rejected the connection request: the session may have ended, " +
		"its address may have expired, or the session ARN may belong to another account"
	RefusalNoDeviceSerial = "the remote adb server lists no device"
	// Verb: the scheme of the session's address.
	RefusalEndpointNotWSSFormat = "only wss endpoints are supported for connections to devices: %q"
	// A control frame on another wire version. Verbs: its version byte, this program's.
	RefusalOtherProtocolFormat = "first websocket message is not a control frame this client understands: " +
		"wire protocol 0x%02x, this client speaks 0x%02x"
)

// Fixes the bridge sends with a refused request, one each: whole sentences that name the command
// to run. The restart verb: the quoted start command, then FixOtherOptionsClause for a bridge with
// reverse on.
const (
	// Verb: the refused session's ARN.
	FixRemoveOtherDeviceFormat = "Remove that device with '" + BinaryName + " device remove', then " + DetachReattachLead + " %s' again."
	FixPassRunningSessionARN   = "Pass the ARN of a running remote access session. To list your sessions, run " +
		"'aws devicefarm list-remote-access-sessions --arn <project ARN> --region us-west-2'."
	FixOtherOptionsClause = " with any other option the bridge started with"
	// Verb: the restart.
	FixBridgeCredentialsFormat = "The bridge uses the AWS credentials of the shell that started it. From a shell with working " +
		"AWS credentials that allow devicefarm:GetRemoteAccessSession, run %s."
	// Verbs: the doctor command, the restart.
	FixDoctorThenRestartFormat = "Run '%s'. If its AWS and Network rows pass, restart the bridge from this shell with %s."
	// A session that is gone, ended or stopping. Shared with DetachNewSessionFix.
	FixNewSessionClause = "a new remote access session, then " + DetachReattachLead + " <new ARN>'"
	FixNewSession       = "Start " + FixNewSessionClause + "."
	// Verb: the refused session's ARN.
	FixWaitForSessionFormat = "Wait for the session to begin running, then " + DetachReattachLead + " %s' again."
	// Verb: the doctor command.
	FixDoctorThenSupportFormat = "Run '%s'. If the doctor does not find the cause, contact AWS Support."
	// Verb: the oldest Android release this program serves.
	FixSupportedDeviceFormat = "Start a remote access session on an Android %d or later device, then " + DetachReattachLead + " <new ARN>'."
	FixOtherDevice           = "Start a remote access session on another device, then " + DetachReattachLead + " <new ARN>'. " +
		"If the error remains, contact AWS Support."
	// Verbs: the session ARN, the doctor command.
	FixRetryThenDoctorFormat = "Run " + DoctorDeviceAddCommandFormat + " again. If the error remains, run '%s'. " +
		"If the doctor does not find the cause, contact AWS Support."
	// Follows a sentence that names the host and the port.
	FixAskNetworkAdministratorLead = "If a firewall or a proxy on your network blocks the connection, ask your network administrator " +
		"to allow direct outbound WebSocket connections (wss) to that host and port"
	// Verbs: the host and port of the session's address, the session ARN, the doctor command.
	FixCheckReachFormat = "Check that this machine can reach %s on TCP port %s. " + FixAskNetworkAdministratorLead + ". Then " +
		DetachReattachLead + " %s' again. '%s' tests the connection."
	// Verb: the restart.
	FixInstallLatestFormat = "Install the latest " + BinaryName + ", then run %s."
	FixMalformedRequest    = "Restart the bridge: run " + StartWithYourOptionsClause + ". If the error remains, install the latest " + BinaryName + "."
	// Also the command's own fix for a refusal with no fix, when the bridge reports another version
	// of this program or no version.
	FixOtherVersionBridge = "The running bridge is another version of " + BinaryName + " than this command. Restart it: run " +
		StartWithYourOptionsClause + ". If the error remains, install the latest " + BinaryName + ", then run '" + DoctorCommand +
		"'. If the doctor does not find the cause, contact AWS Support."
	FixSeeDeviceList  = "Run '" + BinaryName + " device list' to see what the bridge holds and why the last device detached."
	FixSeeAttachedARN = "Run '" + BinaryName + " device list --list' to see the ARN of the attached session. " +
		"To detach that device, run '" + BinaryName + " device remove'."
)

// Detach reasons, as bridge status, device list and adb show them.
const (
	// Open quote; each format closes it: "run 'devicefarm-connect device add --session-arn <new ARN>'".
	DetachReattachLead = "run '" + BinaryName + " device add --session-arn"
	// Fix for a session that is gone, ended or stopping. A detach reason joins it after "; ".
	DetachNewSessionFix      = "start " + FixNewSessionClause
	DetachSessionGoneFormat  = "remote access session %s no longer exists; " + DetachNewSessionFix
	DetachSessionEndedFormat = "remote access session %s %s; " + DetachNewSessionFix
	// The websocket relay's session-stopping close: the cause of a refused add. DetachSessionEndedFormat is its detach reason.
	DetachSessionStoppingFormat = "remote access session %s " + DetachStoppingClause
	// Verbs: the session ARN, the restart as the fixes quote it.
	DetachCredentialsExpiredFormat = "the AWS credentials the bridge started with have expired, so it can no longer read remote access " +
		"session %s; refresh the credentials, then from that shell run %s"
	// A session the bridge can no longer read. Verbs: the session ARN, the failure's class. Each format adds its class's advice.
	DetachUnreadableLeadFormat    = "the bridge can no longer read remote access session %s: %s; "
	DetachSessionUnreadableFormat = DetachUnreadableLeadFormat + "restart the bridge from a shell with working AWS credentials"
	DetachLookupFailedFormat      = "the bridge could not look up remote access session %s: %v; " + DetachReattachLead + " %s' to attach it again"
	DetachInvalidARNFormat        = DetachUnreadableLeadFormat + DetachNewSessionFix
	// Last verb: the oldest Android release this program serves.
	DetachUnsupportedDeviceFormat = DetachUnreadableLeadFormat + "start a remote access session on an Android %d or later device, then " +
		DetachReattachLead + " <new ARN>'"
	// Last verb: the doctor command.
	DetachNoEndpointFormat = DetachUnreadableLeadFormat + "run '%s'; if the doctor does not find the cause, contact AWS Support"
	// Fills DetachSessionEndedFormat, as SessionEnded does, and ends DetachSessionStoppingFormat:
	// "remote access session <ARN> is stopping; start ...".
	DetachStoppingClause              = "is stopping"
	DetachEndedWithStatusClauseFormat = SessionEnded + " (%s)"
)

// AWS session and credential errors. A short phrase reads inside a format: "remote access session <ARN> not found".
const (
	SessionInvalidARN              = "invalid session ARN"
	SessionAccessDenied            = "access denied"
	SessionNotFound                = "not found"
	SessionUnsupportedDevice       = "unsupported device"
	SessionEnded                   = "has ended"
	SessionNoEndpointYet           = "has no interactive endpoint yet"
	SessionRequestRejected         = "request rejected"
	SessionCredentialProcessFailed = "the credential_process helper failed or returned unusable output"

	// ARN refusals. First verb: the invalid-ARN error, so the text starts "invalid session ARN: ".
	SessionParseARNFormat         = "%w: parse session ARN %q: %w"
	SessionNotAWSARNFormat        = "%w: ARN %q is not an AWS ARN (partition %q)"
	SessionNotDeviceFarmARNFormat = "%w: ARN %q is not a Device Farm ARN (service %q)"
	SessionWrongRegionFormat      = "%w: session ARN %q is in region %q, but Device Farm remote access is only supported in %s"
	// Last verb: the region a session ARN names.
	SessionNotSessionARNFormat = "%w: ARN %q is not a remote access session ARN (resource %q); a session ARN looks like " +
		"arn:aws:devicefarm:%s:<account>:session:<ids>"
	SessionCallFailedFormat      = "GetRemoteAccessSession %q: %w"
	SessionNotFoundCauseFormat   = "session %w: %w"
	SessionNotFoundFormat        = "remote access session %q %w"
	SessionEndedWithStatusFormat = "remote access session %q " + SessionEnded + " (status %s)"
	SessionNotRunningFormat      = "remote access session %q is not running yet (status %s)"
	SessionNoEndpointFormat      = "remote access session %q is running, but Device Farm returned no interactive endpoint for it"
	SessionNotAndroidFormat      = "remote access session %q is on an %s device, but this client only supports Android devices"
	SessionAndroidTooOldFormat   = "remote access session %q is on an Android %s device, but this client only supports Android %d and above"
	// "Google Pixel 9 (OS 16)"
	SessionDeviceLabelFormat = "%s (OS %s)"
	SessionUnknownDeviceName = "unknown device"
	SessionUnknownOSVersion  = "unknown"
)

// Flag validation.
const (
	ConfigListenAddrEmpty           = "--listen-addr must not be empty"
	ConfigListenAddrMalformedFormat = "--listen-addr must be an IP address or hostname with no port or brackets, got %q"
	ConfigSetPortWithFlag           = "set the port with --port"
	ConfigPortOutOfRangeFormat      = "--port must be from 1 to 65535, got %d"
)

// bridge start and bridge stop failures.
const (
	StartExitedFormat        = "the bridge exited while starting: %s. Fix what it reports, then run '%s'. Its log is \"%s\"."
	StartExitedNoErrorFormat = "the bridge exited while starting and logged no error. Check \"%s\", then run '%s'."
	StartNotReadyFormat      = "the bridge did not report ready within %s and logged no error, so it was stopped. Check \"%s\", then run '%s'."
	StopFromElevatedShell    = "stop it from an elevated shell"
	StopSurvivedKillFormat   = "bridge (PID %d) survived kill after %s; PID file left in place"
)

// Listen port failures and their fixes.
const (
	PortStartedMeanwhile = "another command started the bridge on this port meanwhile"
	// "devicefarm-connect bridge start --port 5037"; a non-default address adds " --listen-addr <address>".
	PortStartCommandFormat  = BridgeStartCommand + " --port %d"
	PortDoctorCommandFormat = DoctorCommand + " --port %d"
	PointAdbAtPortClause    = "point adb at it with 'adb -P <n>'"
	// Follows "the bridge" or "a bridge".
	PortOnOtherPortClause = "on another port with '" + BridgeStartCommand + " --port <n>' and " + PointAdbAtPortClause
	// Follows "start" or "Start".
	PortOtherPortClause  = "the bridge " + PortOnOtherPortClause
	PortHeldFormat       = "another program is listening on %s, so the bridge cannot bind it. Start " + PortOtherPortClause + ", or stop that program and run '%s'. The bridge listens on exactly the port you pass."
	PortUnbindableFormat = "the bridge cannot bind %s: %v. Run '%s' for the fix, or start " + PortOtherPortClause + ". The bridge listens on exactly the port you pass."
	PortReleasedFormat   = "another program held %s while the bridge started and has released it. Run '%s'. The bridge binds the port on that start."
	// Another bridge on the port, which answered with its PID. Verbs: PID, address, PID, the start command.
	PortHeldByBridgeFormat = "another " + BinaryName + " bridge (PID %d) is listening on %s, so a new bridge cannot bind it. That bridge " +
		"was started from another working directory or by another user, so this shell cannot stop it. Run '" + BridgeStopCommand +
		"' in the shell that started it, or check that process %d is " + BinaryName + " and end it, then run '%s'. Or start a " +
		"bridge " + PortOnOtherPortClause + "."
	// An adb server on the port. Its stop answer fills PortAdbServerDidNotStopFormat: "... and did not (it answered FAIL). <fix>"
	PortStopNoAnswerFormat        = "it answered nothing within %s"
	PortStopBadReplyFormat        = "it answered %q, not OKAY or FAIL"
	PortStopFailed                = "it answered FAIL"
	PortStopFailedLead            = PortStopFailed + ": "
	PortStopStillHeldFormat       = "it answered OKAY but port %d is still held after %s; a program that restarts adb, such as Android Studio, can take it back at once"
	PortAdbServerDidNotStopFormat = "an adb server on %s was asked to stop (via 'adb kill-server') and did not (%v). %s"
	PortAdbServerTookPortFormat   = "an adb server took %s before the bridge could bind it. Programs that restart adb automatically, such as Android Studio, are known to do this. %s"
	// Fix for an adb server that keeps the port, on a non-default port.
	PortStartOnOtherPortTail   = "start " + PortOtherPortClause + "."
	PortStartOnOtherPortFormat = "Start " + PortOtherPortClause + ". That adb server keeps port %d and the bridge serves adb on the port you pass."
	// On the default port. Verbs: the start command, the port.
	PortProgramThenUsesBridgeTailFormat = "it then uses the bridge as its adb server on port %d."
	PortQuitThatProgramFormat           = "Quit the program that restarted adb, run '%s', then open it again: " + PortProgramThenUsesBridgeTailFormat
	PortQuitThatProgramTailFormat       = "quit that program, run '%s' again, then open it: " + PortProgramThenUsesBridgeTailFormat
	PortQuitProgramThatStartedAdbFormat = "Quit the program that started adb, such as Android Studio, run '%s', then open that program again: " + PortProgramThenUsesBridgeTailFormat
)

// The bridge's own FAIL texts; adb prints them after "error: ".
const (
	// Names the flag that turns reverse on.
	AdbFailReverseOff      = "adb reverse is off for this client; restart it with --allow-adb-reverse"
	AdbFailReverseNotReady = "the device's reverse rules are not ready yet; run the command again"
	// A reverse install onto the bridge's own listen port. Verb: that port.
	AdbFailReverseOntoListenPortFormat = "cannot reverse to tcp:%d: the bridge listens on that port"
	// The server's refusal does not say which holder, so the text names both.
	AdbFailReverseHeld               = "adb reverse on this device is held by another adb client, or by this client's previous connection for a short while; stop the other client or retry later"
	AdbFailTooManyConnectionsFormat  = "too many adb connections (limit %d)"
	AdbFailTooManyForwardRulesFormat = "too many forward rules (limit %d)"
	AdbFailTooManyReverseRulesFormat = "too many reverse rules (limit %d)"
	// The server's reason or the dial's cause follows.
	AdbFailRefusedLead     = "the device connection was refused: "
	AdbFailUnreachableLead = "the bridge could not reach the device: "
)

// Doctor report: table headings, the list heading and the count lines.
const (
	DoctorHeadingCategory = "Category"
	DoctorHeadingStatus   = "Status"
	DoctorHeadingDetails  = "Details"
	DoctorHeadingWhatToDo = "What to do"

	// "doctor: 1 of 4 categories passed, 1 warning, 1 failed, 1 not checked"
	DoctorCountCategoriesPassedFormat = "doctor: %d of %d categories passed"
	DoctorCountOneWarning             = "1 warning"
	DoctorCountWarningsFormat         = "%d warnings"
	DoctorCountFailedFormat           = "%d failed"
	DoctorCountNotCheckedFormat       = "%d not checked"

	// Ends the --verbose output. Counts checks, not categories.
	DoctorCountChecksOneWarningFormat = "doctor: %d passed, 1 warning, %d failed, %d skipped"
	DoctorCountChecksFormat           = "doctor: %d passed, %d warnings, %d failed, %d skipped"
)

// Doctor category titles.
const (
	DoctorCategoryLocalMachine = "Local machine"
	DoctorCategoryAWS          = "AWS"
	DoctorCategoryNetwork      = "Network"
	DoctorCategoryDependencies = "Dependencies"
)

// Doctor rows: what the report writes around a check's own text.
const (
	// "listen port: <summary>", then "also passed: working directory, adb forward"
	DoctorRowAlsoPassedLead = "also passed: "

	// "adb versions not checked: needs remote access session", "not checked: AWS credentials failed"
	DoctorRowNotCheckedTail = " not checked"
	DoctorRowFailedTail     = " failed"
	// A check title follows in the table, a check id in --verbose and --json.
	DoctorRowNeedsLead = "needs "

	DoctorRowTimedOutFormat = "timed out after %s"

	// --verbose row: "[FAIL] local.listen-port: <summary>; fix: <hint>"
	DoctorRowFixTailFormat = "; fix: %s"
)

// Doctor check: working directory.
const (
	DoctorTitleWorkingDirectory = "working directory"

	DoctorDetailDirectoryWritableFormat       = "working directory %s is writable"
	DoctorDetailDirectoryUnusableLead         = ProblemWorkingDirectory + ": "
	DoctorDetailDirectoryOneByteTooLongFormat = "working directory %s is 1 byte too long for this system"
	DoctorDetailDirectoryTooLongFormat        = "working directory %s is %d bytes too long for this system"
	DoctorDetailDirectoryNotWritableFormat    = "cannot write a file in the working directory %s: %v"

	// Verb: the environment variable that sets the directory's parent.
	DoctorHintDirectoryUnusableFormat = "the client cannot use this working directory, so '" + BridgeStartCommand + "' cannot launch the bridge. " +
		"Set '%s' to a directory with a short path that only this user can write, then rerun '" + DoctorCommand +
		"'. Every " + BinaryName + " command reads that variable, so 'bridge start' and the bridge it launches use the new directory too."
)

// The reverse flag's caveat.
const (
	// Follows every mention of --allow-adb-reverse.
	DoctorHintReverseCaveat = "'--allow-adb-reverse' lets the device open connections to this machine, so use it only with a " +
		"device you trust; a public physical device can run software you do not control."
)

// Doctor check: bridge.
const (
	DoctorTitleBridge = "bridge"

	DoctorDetailDaemonNotRunning = "not running; bridge start, device add, or device list launches it"

	// "running (PID 4242) on port 5037; device Google Pixel 8 (OS 15), session <ARN>; reverse on"
	// Verbs: PID, port, one of the device clauses, one of the reverse clauses.
	DoctorDetailDaemonRunningFormat      = "running (PID %d) on port %d; %s; %s"
	DoctorDetailDaemonNoDeviceClause     = "no device"
	DoctorDetailDaemonDeviceClauseFormat = "device %s, session %s"
	DoctorDetailDaemonReverseOffClause   = "reverse off"
	DoctorDetailDaemonReverseOnClause    = "reverse on"
	// Follow the running text. Verb: the session --session-arn asked for.
	DoctorDetailDaemonDetachedClause          = "; the bridge detached the last device"
	DoctorDetailDaemonNotAttachedClauseFormat = "; session %s is not attached"
	// Follow the running text, before the other clauses. Verbs: the bridge's version, then the
	// command's; Earlier, for a bridge that reports no version, has only the command's.
	DoctorDetailDaemonOtherProgramVersionClauseFormat   = "; the bridge runs another version of " + BinaryName + " (%s) than this command (%s)"
	DoctorDetailDaemonEarlierProgramVersionClauseFormat = "; the bridge runs an earlier version of " + BinaryName + " than this command (%s)"

	DoctorDetailDaemonSilentFormat        = "a bridge (PID %d) is registered but does not answer: %s"
	DoctorDetailDaemonNoStatusFormat      = "a bridge (PID %d) is registered but did not report its status: %s"
	DoctorDetailDaemonStatusRefusedFormat = "a bridge (PID %d) is registered but refused to report its status: %s"

	DoctorHintDaemonNoStatus = "a bridge is registered but did not report its status. Run " + StartWithYourOptionsClause +
		". The start ends that bridge and launches a new one."
	// Verb: the restart with the bridge's own port, session and reverse setting.
	DoctorHintDaemonOtherProgramVersionFormat = "the bridge runs another version of " + BinaryName + " than this command, so a command can " +
		"fail. Run '%s' to start it again with this command's version of " + BinaryName + ". The restart drops an attached device with its " +
		"forward rules and open adb connections, and its reverse rules no longer reach this machine; the device comes back when the " +
		"command names its session."
	// Verb: the restart with the reverse flag.
	DoctorHintDaemonReverseOffFormat = "the bridge runs with adb reverse off, so every 'adb reverse' fails. Run '%s' to start it again " +
		"with reverse on."
	// Verb: the bridge's detach reason, which names the fix.
	DoctorHintDaemonDetachedFormat = "the bridge detached the device, so adb finds no device: %s."

	// The asked session is not attached. Verbs: session ARN, quoted add command. Third verb of the
	// last format: the attached device, which the remove detaches.
	DoctorDeviceAddCommandFormat                  = "'" + BinaryName + " device add --session-arn %s'"
	DoctorHintSessionNotAttachedFormat            = "session %s is not attached to the bridge, so adb cannot reach its device. Run %s."
	DoctorHintSessionNotAttachedRemoveFirstFormat = "session %s is not attached to the bridge, so adb cannot reach its device. Run '" + BinaryName +
		" device remove', then %s. The remove detaches %s."
)

// Doctor check: listen port.
const (
	DoctorTitleListenPort = "listen port"

	// "127.0.0.1:5037 is free"
	DoctorDetailPortFreeTail            = " is free"
	DoctorDetailPortHeldByDaemonFormat  = "%s is held by the running bridge (PID %d)"
	DoctorDetailDaemonOnOtherPortFormat = "the running bridge (PID %d) listens on port %d, not %d"
	DoctorDetailAdbServerOnPortFormat   = "an adb server (version %d) is listening on %s"
	DoctorDetailOtherBridgeOnPortFormat = "another " + BinaryName + " bridge (PID %d) is listening on %s"
	DoctorDetailPortHeldByProgramFormat = "cannot bind %s because another program is listening on it"

	DoctorHintPortInvalid = "'--port' does not name a port the bridge can bind. Pass '--port' a number from 1 to 65535, as in '" + BridgeStartCommand +
		" --port 5037'. The bridge listens on exactly that port, so point adb at the same port."
	DoctorHintListenFlagsInvalid = "the listen flags do not name an address the bridge can bind. Pass '--port' a number from 1 to 65535 and " +
		"'--listen-addr' a non-empty address with no port or brackets, as in '" + BridgeStartCommand + " --port 5037 --listen-addr 127.0.0.1'. The bridge " +
		"binds exactly that address, so point adb at the same port."

	// Verbs: bridge's port, asked port, restart, asked port, bridge's port.
	DoctorHintDaemonOnOtherPortFormat = "the running bridge listens on port %d, not %d. " +
		"Run '%s' to move it to port %d, or point adb at it with 'adb -P %d'."

	// Verbs: address, its start command, the fix when a program takes the port back. That fix ends the sentence.
	DoctorHintAdbServerOnPortFormat = "an adb server is listening on %s, so the bridge cannot bind it until that server stops. Run '%s': it stops that " +
		"server first, as 'adb kill-server' would. If a program that restarts adb, such as Android Studio, takes the port back, %s"

	DoctorHintPortNeedsAdminFormat = "this user cannot bind port %d; ports below 1024 need administrator rights on most systems. " +
		"Run '" + BridgeStartCommand + " --port <n>' with a port above 1023 and " + PointAdbAtPortClause + ". The bridge then listens on that port instead of %d."
	DoctorHintPortReservedFormat = "Windows refuses to bind port %d: the port sits in a range that Hyper-V, WSL, or Docker reserved, or another " +
		"program holds it exclusively. See the reserved ranges with 'netsh int ipv4 show excludedportrange protocol=tcp', then run " +
		"'" + BridgeStartCommand + " --port <n>' with a port outside them and " + PointAdbAtPortClause + ". The bridge then listens on that port instead of %d."

	DoctorHintAddressNotLocalFormat = "'%s' is not an address of this machine, so nothing can bind it. Pass '--listen-addr' an address this " +
		"machine owns, as in '" + BridgeStartCommand + " --listen-addr 127.0.0.1 --port %d'. The bridge binds exactly that address, so adb must " +
		"reach it there."
)

// Doctor check: adb forward.
const (
	DoctorTitleAdbForward = "adb forward"

	DoctorDetailForwardListensFormat     = "adb forward can listen on 127.0.0.1 (tried port %d)"
	DoctorDetailForwardCannotListenLead  = "adb forward cannot listen on 127.0.0.1: "
	DoctorDetailForwardUnreachableFormat = "cannot reach the port adb forward opened on 127.0.0.1: %v"

	DoctorHintForwardBlocked = "'adb forward' could not open or reach a port on 127.0.0.1, so every 'adb forward' would fail. Allow '" +
		BinaryName + "' to listen on 127.0.0.1 in the sandbox, container, or firewall rules, then rerun '" + DoctorCommand +
		"'. Each 'adb forward' rule opens one port on 127.0.0.1, and every forwarded connection enters through it."
)

// Doctor check: AWS credentials.
const (
	DoctorTitleAWSCredentials = "AWS credentials"

	// "credentials from SSOProvider". Verb: the credential source.
	DoctorDetailCredentialsSourceFormat = "credentials from %s"
	// Puts user words before the SDK's config or chain error.
	DoctorDetailNoCredentialsLead = "this shell has no usable AWS credentials: "

	DoctorSSOLoginCommand = "aws sso login --profile <name>"

	// Also the remote access session check's hint. No period after the URL, so a copied line stays a working link.
	DoctorHintNoCredentials = "the AWS config did not load or its credential chain resolved nothing, so every Device Farm call fails. " +
		"Set 'AWS_PROFILE' to a profile with Device Farm access, run '" + DoctorSSOLoginCommand + "' for a profile that uses SSO, " +
		"or export 'AWS_ACCESS_KEY_ID' and 'AWS_SECRET_ACCESS_KEY'. For more information, see " +
		"https://docs.aws.amazon.com/cli/latest/userguide/cli-chap-authentication.html"
)

// Doctor rerun that the hints below quote. ListenAddrFlagLead and the address follow off the default address.
const (
	DoctorRerunCommandFormat = DoctorCommand + " --session-arn %s --port %d"
)

// Doctor check: remote access session.
const (
	DoctorTitleRemoteAccessSession = "remote access session"

	// SKIP text with no ARN and no device.
	DoctorDetailSessionARNMissing = "pass --session-arn to check the session, the device connection, and adb versions"
	// SKIP text with no ARN and several devices. Verb: the device count.
	DoctorDetailSeveralDevicesFormat = "the bridge holds %d devices; pass --session-arn to pick the session to check"

	// Verbs: device label, then the ARN.
	DoctorDetailSessionRunningFormat       = "RUNNING: %s"
	DoctorDetailDaemonSessionRunningFormat = DoctorDetailSessionRunningFormat + " (the session the bridge holds: %s)"

	DoctorCreateSessionCommand = "aws devicefarm create-remote-access-session --project-arn <project ARN> --device-arn <device ARN> " +
		"--region us-west-2"
	// Ends the new-session step of hints below.
	DoctorHintPassNewARNClause = "and pass its ARN with '--session-arn'"

	// Session hints. First verb: the ARN.

	DoctorHintARNInvalidFormat = "'%s' is not a Device Farm remote access session ARN in us-west-2. Pass '--session-arn' the ARN that the " +
		"Device Farm console or '" + DoctorCreateSessionCommand + "' printed. The bridge reaches the device through that ARN and refuses any other."
	DoctorHintSessionAccessDeniedFormat = "these credentials cannot read session '%s'. Allow 'devicefarm:GetRemoteAccessSession' on it in the IAM " +
		"policy of this profile or role, or set 'AWS_PROFILE' to a profile that has it. The bridge makes the same call every " +
		"few minutes while the device is attached, so the permission must stay for that whole time."
	DoctorHintSessionNotFoundFormat = "Device Farm has no session '%s' in this account and region. Check the ARN against " +
		"'aws devicefarm list-remote-access-sessions --arn <project ARN> --region us-west-2' and pass one whose status is " +
		"RUNNING. A mistyped ARN or a session of another account resolves to nothing."
	DoctorHintAndroidTooOldFormat = "session '%s' runs on an Android release older than this client supports, so the bridge cannot attach it. " +
		"Start a new remote access session on a device with a newer Android release (the summary names the oldest one " +
		"supported) " + DoctorHintPassNewARNClause + ". The bridge refuses the older device at attach time too."
	// Verbs after the ARN: the device's platform, both times.
	DoctorHintNotAndroidFormat = "session '%s' runs on an %s device. This client connects adb to Android devices only, so the bridge " +
		"cannot attach it. Start a new remote access session on an Android device in the Device Farm console or with '" + DoctorCreateSessionCommand + "', " + DoctorHintPassNewARNClause + ". " +
		"The bridge refuses the %s device at attach time too."
	DoctorHintSessionEndedFormat = "session '%s' has ended, so nothing can attach to it. Start a new remote access session in the Device Farm " +
		"console or with '" + DoctorCreateSessionCommand + "', " + DoctorHintPassNewARNClause + ". A bridge attached to a session that ends drops the device within a few minutes."
	// Second verb: the quoted rerun.
	DoctorHintSessionNotReadyFormat = "this session has no interactive endpoint yet (it is still starting, or not RUNNING), so the bridge " +
		"cannot attach it. Wait until 'aws devicefarm get-remote-access-session --arn %s --region us-west-2' reports status " +
		"RUNNING with an endpoint, then rerun %s."
	// Second verb: the quoted rerun.
	DoctorHintSessionNoEndpointFormat = "Device Farm reports session '%s' RUNNING but returned no interactive endpoint for it, so the " +
		"bridge cannot attach it. Rerun %s once. If the endpoint stays missing, contact AWS Support."
	// Second verb: the quoted rerun.
	DoctorHintRequestRejectedFormat = "Device Farm rejected the request for session '%s' (the summary names the code), so the request itself " +
		"is at fault: expired or wrong credentials, a bad signature, or a clock that is off, not the network. Run " +
		"'aws sts get-caller-identity' with the same profile or keys and fix what it reports, or the system clock on a " +
		"signature error, then rerun %s. The bridge signs its calls the same way and would detach the device on the same answer."
	DoctorHintSessionCallFailedFormat = "the call to Device Farm for session '%s' failed. Check the network path from this machine to " +
		"'devicefarm.us-west-2.amazonaws.com'; 'HTTPS_PROXY' applies to this call, so set or clear it to match your network. " +
		"The bridge makes the same call when it attaches and every few minutes after."
)

// Doctor check: adb session to the device.
const (
	DoctorTitleAdbSession = "adb session to the device"

	DoctorDetailAdbSessionOpenedFormat = "adb session %s opened to the device"
	DoctorDetailAdbSessionFailedLead   = "could not open an adb session to the device: "

	// A failed dial. Also the adb reverse check's hint. Verbs: the host and port of the session's address, the quoted rerun.
	DoctorHintWebSocketFailedFormat = "this machine could not open a WebSocket connection (wss) to %s on TCP port %s, so adb cannot " +
		"reach the device through the bridge. " + FixAskNetworkAdministratorLead + ", then rerun %s. If your network allows that " +
		"connection and the error remains, contact AWS Support."
	// Any other failure. Also the adb reverse check's hint. Verb: the quoted rerun.
	DoctorHintAdbConnectionFailedFormat = "Device Farm or the device did not complete the adb connection, so an adb connection through the bridge " +
		"would fail the same way. Rerun %s once; a second failure means the network path to Device Farm or the device itself " +
		"is at fault, so check the session in the Device Farm console."
	// Follows either hint when a proxy is set.
	DoctorHintProxyIgnored = "'HTTPS_PROXY' is set, but the connection to the device does not use it, so no adb traffic gets " +
		"through when this machine reaches the internet only through that proxy."
)

// Doctor check: adb session reconnect.
const (
	DoctorTitleAdbSessionReconnect = "adb session reconnect"

	DoctorDetailReconnectedLead       = "reconnected to adb session "
	DoctorDetailReconnectFailedFormat = "could not reconnect to adb session %s: %v"

	// Verb: the quoted rerun.
	DoctorHintReconnectFailedFormat = "Device Farm did not resume the adb session on a new connection, so a live adb connection would drop " +
		"when the bridge reconnects. Rerun %s once. A second failure means Device Farm does not keep adb sessions across " +
		"reconnects."
)

// Doctor check: adb reverse.
const (
	DoctorTitleAdbReverse = "adb reverse"

	DoctorDetailReverseOnInDaemon = "the running bridge has adb reverse on for this session"
	DoctorDetailReverseAvailable  = "adb reverse is available on this session"
	// Replaces the refusal's own text, which names the mechanism.
	DoctorDetailReverseInUse           = "another client, or an earlier run of this client, still uses adb reverse on this session"
	DoctorDetailReverseUnavailableLead = "adb reverse is not available on this session: "

	// Verb: the quoted rerun.
	DoctorHintReverseInUseFormat = "another client still uses adb reverse on this session, so Device Farm would refuse 'adb reverse' through " +
		"the bridge. Stop that client with '" + BridgeStopCommand + "' on the machine where it runs, or wait for Device Farm to release it, then rerun " +
		"%s. Device Farm lets one client at a time use adb reverse on a device."
)

// Doctor check: adb versions.
const (
	DoctorTitleAdbVersions = "adb versions"

	// Every local adb matches. Verbs of the last format: one of the texts above it, the path of an adb that was not run.
	DoctorDetailAdbMatchesFormat       = "local adb 1.0.%d matches the remote adb server"
	DoctorDetailSeveralAdbsMatchFormat = "local adb 1.0.%d (%d found) matches the remote adb server"
	DoctorDetailAdbNotRunFormat        = "%s, but an adb at '%s' reached only through the current directory or a relative " +
		"PATH entry was not checked"

	// Puts user words before the probe error, the cause AWS Support needs.
	DoctorDetailRemoteAdbSilentLead = "the remote adb server did not answer its version request: "

	// Verb: the remote adb server's version.
	DoctorDetailNoAdbFormat = "no adb found on PATH or in an Android SDK, so its version was not checked against the remote adb server (1.0.%d)"

	// A local adb that gave no version: "'/usr/bin/adb version' failed: exit status 1",
	// "'/usr/bin/adb version' printed no 'version 1.0.N' line: <its first line>"
	DoctorAdbVersionCommandFormat = "'%s version'"
	DoctorDetailAdbTimedOutFormat = "%s did not finish in %s"
	DoctorDetailAdbFailedFormat   = "%s failed: %s"
	DoctorDetailAdbNoVersionLine  = "printed no 'version 1.0.N' line"

	// Shared by the mismatch detail and hint: the commands a mismatch breaks, and the joins before a list's last item.
	DoctorMismatchAdbCommandClause    = "adb command"
	DoctorMismatchFromAnyOfThemClause = "command from any of them"
	DoctorMismatchFromThatAdbClause   = "command from that adb"
	DoctorMismatchListAnd             = " and "
	DoctorMismatchListSerialAnd       = "," + DoctorMismatchListAnd

	// "local adb 1.0.39 at '/usr/bin/adb' differs from the remote adb server (1.0.41); every command from that adb fails"
	DoctorDetailMismatchFormat               = "%s %s from the remote adb server (1.0.%d); every %s fails"
	DoctorDetailMismatchLocalAdbFormat       = "local adb 1.0.%d"
	DoctorDetailMismatchLocalAdbAtPathFormat = DoctorDetailMismatchLocalAdbFormat + " at '%s'"
	DoctorDetailMismatchDiffers              = "differs"
	DoctorDetailMismatchDiffer               = "differ"

	// Second verb: the quoted rerun.
	DoctorHintAdbNotRunFormat = "an adb exists at '%s', reached only through the current directory or a relative PATH entry, so it was not run " +
		"and its version is unknown. Add that directory to PATH by its absolute path if you meant that adb, then rerun %s. A local adb " +
		"on another version cannot connect through the bridge."

	DoctorHintRemoteAdbSilent = "there is an error communicating with the remote adb server, so an adb connection through the bridge " +
		"would fail the same way. Try again later, or contact AWS Support for assistance."

	// Follows a version only when this release's adb speaks it. Change it together with the version comparison in the doctor package.
	DoctorHintPlatformToolsClause = ", such as platform-tools 37"

	// No local adb. Verbs: remote version, not-run clause or nothing, one of the leads, remote version,
	// platform-tools clause or nothing, quoted rerun.
	DoctorHintNoAdbFormat = "adb was not found on PATH or in an Android SDK ('ANDROID_HOME', 'ANDROID_SDK_ROOT', or Android Studio's default SDK location), so the doctor could not check that its version " +
		"matches the remote adb server (1.0.%d)%s. %s Android platform-tools whose adb is 1.0.%d%s, then add its directory to " +
		"PATH and rerun %s. A local adb on another version cannot connect through the bridge."
	DoctorHintNoAdbNotRunClauseFormat = "; an adb exists at '%s', reached only through the current directory or a relative PATH entry, so it " +
		"was not run"
	DoctorHintNoAdbInstallLead      = "Install"
	DoctorHintNoAdbAddOrInstallLead = "Add that directory to PATH by its absolute path if you meant that adb, or install"

	// Local adb gave no version. Verbs: quoted '<adb> version' command, quoted rerun, remote version.
	DoctorHintAdbFailedFormat = "the local %[1]s failed or did not finish, so that adb's version is unknown. Run %[1]s by hand and fix what it " +
		"reports, then rerun %[2]s." + DoctorHintRemoteVersionTailFormat
	DoctorHintAdbNoVersionFormat = "the local %[1]s " + DoctorDetailAdbNoVersionLine + ", so that adb's version is unknown. Run %[1]s by hand " +
		"and check that it is the platform-tools adb, then rerun %[2]s." + DoctorHintRemoteVersionTailFormat
	DoctorHintRemoteVersionTailFormat = " The remote adb server is 1.0.%[3]d, and a local adb on another version cannot connect through " +
		"the bridge."

	// Verbs: cause, fix, quoted rerun, the commands the mismatch breaks.
	DoctorHintMismatchFormat = "%s. %s, then rerun %s. Until then no %s reaches the device. The remote adb server is not affected."
	// Cause: "the local adb is 1.0.39 and the remote adb server is 1.0.41, so every adb command fails: that adb tries ...".
	// The Studio clause follows for a lone PATH adb that is also the SDK's.
	DoctorHintMismatchCauseFormat          = "%s, so every %s fails: that adb tries to start its own adb server on the bridge's port and cannot"
	DoctorHintMismatchLocalAdbFormat       = "the local adb is 1.0.%d"
	DoctorHintMismatchLocalAdbAtPathFormat = "the local adb at '%s' is 1.0.%d"
	DoctorHintMismatchStudioAdbFormat      = "Android Studio's adb at '%s' is 1.0.%d"
	DoctorHintMismatchRemoteAdbFormat      = "the remote adb server is 1.0.%d"
	DoctorHintMismatchStudioClauseFormat   = "; Android Studio runs the SDK's adb at '%s', so Studio lists no device while that adb is on " +
		"another version"
	// Fix. Second verb: platform-tools clause or nothing. Third verb of the update: PATH clause or nothing.
	DoctorHintMismatchInstallFormat   = "Install platform-tools whose adb is 1.0.%d%s, or put such an adb first on PATH"
	DoctorHintMismatchUpdateSDKFormat = "Update Android SDK Platform-Tools in Android Studio's SDK Manager to a release whose adb is 1.0.%d%s%s"
	DoctorHintMismatchPathClause      = ", and put such an adb first on PATH"
)
