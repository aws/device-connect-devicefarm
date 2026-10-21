package cli

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/aws/device-connect-devicefarm/internal/config"
	"github.com/aws/device-connect-devicefarm/internal/control"
	"github.com/aws/device-connect-devicefarm/internal/daemon"
)

// runRoot executes the command tree with the given args in a fresh temp runtime
// dir, so the daemon's runtime files never touch the real per-user location. Use
// it for single-command tests.
func runRoot(t *testing.T, args ...string) (*bytes.Buffer, error) {
	t.Helper()
	isolateRuntimeDir(t, t.TempDir())
	return execRoot(t, args...)
}

// isolateRuntimeDir makes the daemon put its runtime files under dir on every OS.
// Unix reads XDG_RUNTIME_DIR; Windows reads LocalAppData (through os.UserCacheDir).
func isolateRuntimeDir(t *testing.T, dir string) {
	t.Helper()
	t.Setenv("XDG_RUNTIME_DIR", dir)
	t.Setenv("LocalAppData", dir)
}

// execRoot executes the command tree with the given args against the current
// runtime dir, capturing stdout/stderr into the returned buffer. Tests that run
// several commands against one daemon runtime dir call isolateRuntimeDir once
// themselves, then execRoot for each command.
func execRoot(t *testing.T, args ...string) (*bytes.Buffer, error) {
	t.Helper()
	root := newRootCommand()
	out := &bytes.Buffer{}
	root.SetOut(out)
	root.SetErr(out)
	root.SetArgs(args)
	return out, root.Execute()
}

func TestGivenCommandTreeWhenBuiltThenHasExpectedCommandGroups(t *testing.T) {
	root := newRootCommand()

	// The top level is the two command groups plus the hidden daemon body.
	top := childNames(root)
	for _, want := range []string{"proxy", "device", "daemon"} {
		assert.True(t, top[want], "missing top-level command %q", want)
	}

	// proxy owns start/stop/status; device owns add/remove.
	proxy := childNames(findChild(t, root, "proxy"))
	for _, want := range []string{"start", "stop", "status"} {
		assert.True(t, proxy[want], "missing proxy subcommand %q", want)
	}
	device := childNames(findChild(t, root, "device"))
	for _, want := range []string{"add", "remove"} {
		assert.True(t, device[want], "missing device subcommand %q", want)
	}
}

// childNames returns the set of immediate subcommand names of cmd.
func childNames(cmd *cobra.Command) map[string]bool {
	names := make(map[string]bool)
	for _, sub := range cmd.Commands() {
		names[sub.Name()] = true
	}
	return names
}

// findChild returns the immediate subcommand of parent with the given name,
// failing the test if it is absent.
func findChild(t *testing.T, parent *cobra.Command, name string) *cobra.Command {
	t.Helper()
	for _, sub := range parent.Commands() {
		if sub.Name() == name {
			return sub
		}
	}
	t.Fatalf("command %q has no subcommand %q", parent.Name(), name)
	return nil
}

func TestGivenParentCommandWhenInvokedWithNoArgsThenPrintsHelpExitZero(t *testing.T) {
	for _, parent := range []string{"proxy", "device"} {
		t.Run(parent, func(t *testing.T) {
			out, err := runRoot(t, parent)

			require.NoError(t, err, "a bare parent command must print help and exit 0")
			assert.Contains(t, out.String(), "Usage:")
		})
	}
}

func TestGivenParentCommandWhenInvokedWithUnknownSubcommandThenErrors(t *testing.T) {
	for _, parent := range []string{"proxy", "device"} {
		t.Run(parent, func(t *testing.T) {
			_, err := runRoot(t, parent, "bogus")

			// Literal on purpose: a changed BinaryName must fail here.
			wantErr := `unknown command "bogus" for "devicefarm-connect ` + parent + `"`
			require.EqualError(t, err, wantErr, "an unknown subcommand under %q must fail with cobra's text", parent)
		})
	}
}

func TestGivenCommandTreeWhenBuiltThenCompletionCommandNotRegistered(t *testing.T) {
	root := newRootCommand()

	for _, cmd := range root.Commands() {
		assert.NotEqual(t, "completion", cmd.Name(), "the generated completion command must be disabled")
	}
}

func TestGivenDaemonSubcommandWhenInspectedThenIsHidden(t *testing.T) {
	root := newRootCommand()

	for _, cmd := range root.Commands() {
		if cmd.Name() == "daemon" {
			assert.True(t, cmd.Hidden, "daemon subcommand must be hidden")
			return
		}
	}
	t.Fatal("daemon subcommand not found")
}

func TestGivenNoRunningDaemonWhenStatusInvokedThenReportsNotRunning(t *testing.T) {
	out, err := runRoot(t, "proxy", "status")

	require.NoError(t, err)
	assert.Contains(t, out.String(), "not running")
}

func TestGivenNoRunningDaemonWhenProxyStopInvokedThenReportsNotListening(t *testing.T) {
	out, err := runRoot(t, "proxy", "stop")

	require.NoError(t, err)
	assert.Contains(t, out.String(), "not listening")
}

func TestGivenDeviceAddWithoutARNWhenInvokedThenFailsRequestingARN(t *testing.T) {
	_, err := runRoot(t, "device", "add")

	require.Error(t, err)
}

func TestGivenDeviceRemoveWithoutRunningDaemonWhenInvokedThenFailsNotListening(t *testing.T) {
	// With a session ARN provided, the command gets past the required-flag check
	// and fails because no daemon is listening.
	_, err := runRoot(t, "device", "remove", "--session-arn", "arn:aws:devicefarm:us-west-2:1:session:a/b/0")

	require.Error(t, err)
}

func TestGivenListenerFlagsAfterProxyStartPathWhenInvokedThenParsed(t *testing.T) {
	// The integration harness (kept outside this repository) invokes the
	// binary as `devicefarm-connect proxy start --port N --log P --session-arn ...`, i.e.
	// the listener/log flags follow the two-level subcommand path (they are flags of
	// `proxy start`, not global). This guards that contract with a fast unit test
	// rather than only the slow integration suite. fakeDaemon writes a live pidfile,
	// so Manager.Start short-circuits (a daemon is "already running") rather than
	// forking a real child; reaching the add request over the control socket proves
	// the flags parsed and `proxy start` ran to completion.
	addSeen := false
	fakeDaemon(t, func(req control.Request) control.Response {
		if req.Op == control.OpAdd {
			addSeen = true
			assert.Equal(t, "arn:aws:devicefarm:us-west-2:1:session:a/b/0", req.ARN)
		}
		return control.Response{OK: true, DeviceLabel: "Pixel", Port: 5599}
	})

	_, err := execRoot(t, "proxy", "start", "--port", "5599", "--log", t.TempDir()+"/d.log",
		"--session-arn", "arn:aws:devicefarm:us-west-2:1:session:a/b/0")

	require.NoError(t, err, "proxy start must parse its flags and attach through the control channel")
	assert.True(t, addSeen, "proxy start must have parsed its flags and forwarded the add to the daemon")
}

func TestGivenListenerFlagOnScopedCommandWhenInvokedThenRejected(t *testing.T) {
	// --port/--listen-addr/--log are scoped to `proxy start` (and the hidden daemon
	// body), not global. A command that does not stand up the daemon must reject
	// them, which is what makes the pruned "Global Flags" help honest.
	for _, flag := range []string{"--port", "--listen-addr", "--log"} {
		t.Run(flag, func(t *testing.T) {
			value := "9999"
			if flag != "--port" {
				value = "x"
			}
			_, err := runRoot(t, "proxy", "status", flag, value)
			require.Error(t, err, "%s must not be accepted on 'proxy status'", flag)
		})
	}
}

func TestGivenAllowAdbReverseWhenParsedThenOnlyDaemonStandingCommandsAcceptIt(t *testing.T) {
	for _, path := range [][]string{{"proxy", "start"}, {"daemon"}} {
		t.Run(strings.Join(path, " "), func(t *testing.T) {
			state := &rootState{cfg: config.Default()}
			cmd := newRootCommandWithState(state)
			for _, name := range path {
				cmd = findChild(t, cmd, name)
			}
			require.False(t, state.cfg.ReverseEnabled, "reverse is off until the flag sets it")

			// Parsed from argv, not set on the flag: this is the token AsDaemonArgs writes.
			require.NoError(t, cmd.ParseFlags([]string{"--allow-adb-reverse"}), "%v must bind --allow-adb-reverse", path)

			assert.True(t, state.cfg.ReverseEnabled, "--allow-adb-reverse must set the config's reverse setting")
		})
	}
	for _, path := range [][]string{{"proxy", "status"}, {"device", "add"}} {
		t.Run(strings.Join(path, " ")+" refuses it", func(t *testing.T) {
			// Scoped like the listener flags: the running daemon already decided, so the flag would mislead.
			_, err := runRoot(t, path[0], path[1], "--allow-adb-reverse")

			require.Error(t, err, "--allow-adb-reverse must not be accepted on %v", path)
		})
	}
}

func TestGivenAnOptedInConfigWhenItsDaemonArgsReachTheDaemonCommandThenReverseStaysOn(t *testing.T) {
	// The re-exec round trip: the parent's argv must turn reverse on in the child, or a customer
	// who opted in gets the reverse-off FAIL.
	parent := config.Default()
	parent.ReverseEnabled = true
	state := &rootState{cfg: config.Default()}
	daemonCommand := findChild(t, newRootCommandWithState(state), "daemon")

	require.NoError(t, daemonCommand.ParseFlags(parent.AsDaemonArgs()))

	assert.True(t, state.cfg.ReverseEnabled, "the child must inherit the opt-in from the parent's argv")
}

func TestGivenDebugFlagWhenStatusInvokedThenAcceptedAndRuns(t *testing.T) {
	// --debug is a boolean flag (no value); it must parse and run through to the
	// status command rather than being rejected.
	out, err := runRoot(t, "proxy", "status", "--debug")

	require.NoError(t, err)
	assert.Contains(t, out.String(), "not running")
}

// Literal on purpose: the release steps read this exact line, so a changed
// BinaryName or VERSION must fail here.
const wantVersionLine = "devicefarm-connect version v1.0.0\n"

func TestGivenVersionFlagWhenRootRunsThenPrintsNameAndEmbeddedVersion(t *testing.T) {
	runtimeBase := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", runtimeBase)

	out, err := execRoot(t, "--version")

	require.NoError(t, err)
	assert.Equal(t, wantVersionLine, out.String())
	assert.NoDirExists(t, filepath.Join(runtimeBase, config.BinaryName), "--version must not create the runtime dir")
}

func TestGivenShortVersionFlagWhenRootRunsThenPrintsTheSameLine(t *testing.T) {
	out, err := runRoot(t, "-v")

	require.NoError(t, err)
	assert.Equal(t, wantVersionLine, out.String())
}

func TestGivenUnknownFlagWhenInvokedThenFails(t *testing.T) {
	// The removed --log-level must no longer be accepted.
	_, err := runRoot(t, "proxy", "status", "--log-level", "debug")

	require.Error(t, err)
}

func TestGivenInvalidPortWhenProxyStartInvokedThenValidationFails(t *testing.T) {
	_, err := runRoot(t, "proxy", "start", "--port", "0", "--session-arn", "arn:aws:devicefarm:us-west-2:1:session:a/b/0")

	require.Error(t, err)
}

// newTestState builds a rootState with default config and real runtime paths under
// a temp runtime dir, so the manager constructors can be exercised directly.
func newTestState(t *testing.T) *rootState {
	t.Helper()
	isolateRuntimeDir(t, t.TempDir())
	paths, err := daemon.DefaultPaths()
	require.NoError(t, err)
	return &rootState{cfg: config.Default(), paths: paths}
}

func TestGivenLifecycleManagerWhenRunForegroundThenRefusesWithoutRemoteAccessClient(t *testing.T) {
	// proxy start/stop/status only signal and inspect a process, so newManager
	// deliberately builds no AWS client: doing so would make every lifecycle command
	// load AWS config and fail on a host with none configured. The daemon body uses
	// newDaemonManager instead, so a lifecycle manager reaching RunForeground is a
	// wiring mistake that must fail loudly rather than nil-panic on the first add.
	state := newTestState(t)

	err := state.newManager().RunForeground(context.Background())

	require.ErrorContains(t, err, "remote access client")
}

func TestGivenPinnedEnvWhenDaemonManagerBuiltThenBuildsTheAwsClientWithoutError(t *testing.T) {
	// newDaemonManager builds the AWS client eagerly, so the daemon body starts with
	// one already in hand rather than loading config on the attach path. The env is
	// pinned so the config load behaves the same on any host (a stray AWS_PROFILE
	// would otherwise make the SDK demand that profile exist).
	state := newTestState(t)
	state.cfg.ListenPort = 0 // fails config validation, so RunForeground stops early
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	t.Setenv("AWS_PROFILE", "")
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(t.TempDir(), "no-config"))
	t.Setenv("AWS_DEFAULTS_MODE", "legacy")

	manager, err := state.newDaemonManager(context.Background())

	require.NoError(t, err)
	// The daemon body must now get PAST the missing-client guard that a lifecycle
	// manager trips, failing instead on the invalid port seeded above. That
	// discriminates "a client was wired in" from "it errored for any reason".
	require.ErrorContains(t, manager.RunForeground(context.Background()), "configuration")
}
