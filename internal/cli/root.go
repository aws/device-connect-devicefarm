package cli

import (
	"context"
	"fmt"
	"io"
	"log/slog"

	"github.com/spf13/cobra"

	"github.com/aws/device-connect-devicefarm/internal/config"
	"github.com/aws/device-connect-devicefarm/internal/control"
	"github.com/aws/device-connect-devicefarm/internal/daemon"
	"github.com/aws/device-connect-devicefarm/internal/failure"
	"github.com/aws/device-connect-devicefarm/internal/session"
	"github.com/aws/device-connect-devicefarm/internal/version"
)

// rootState holds the flag-bound configuration and the resolved runtime paths
// shared across subcommands. It is populated by the persistent flags on the root
// command and finalized in PersistentPreRunE. sessionARN is a per-command
// argument (the device to add or remove), not part of the daemon's config.
type rootState struct {
	cfg        config.ProxyConfig
	sessionARN string
	paths      daemon.Paths
	// commandLineAccepted flips in PersistentPreRunE, which cobra runs after it
	// has parsed the flags and checked the arguments. A failure before that is a
	// bad command, flag or argument.
	commandLineAccepted bool
}

// Execute runs the command tree on the process arguments and returns the exit
// code for main.
func Execute() int {
	state := &rootState{cfg: config.Default()}
	return execute(newRootCommandWithState(state), state)
}

// execute runs root and writes its failure to the root's error stream as
// problem, causes and fix. A failure before PersistentPreRunE (a bad command,
// flag or argument) gets the command's help as its fix. The daemon body's
// stderr is its logfile, so its failure is a timestamped log line instead.
func execute(root *cobra.Command, state *rootState) int {
	cmd, err := root.ExecuteC()
	if err == nil {
		return 0
	}
	if !state.commandLineAccepted {
		err = usageError(cmd, err)
	}
	if cmd.Name() == daemon.DaemonSubcommand {
		slog.Error(config.BinaryName+" daemon failed", "error", err)
	} else {
		failure.Write(root.ErrOrStderr(), config.BinaryName, err)
	}
	return 1
}

// usageError attaches the command's help to a command-line mistake. A hidden
// command's name stays hidden: its fix names the root.
func usageError(cmd *cobra.Command, err error) error {
	if cmd.Hidden {
		cmd = cmd.Root()
	}
	return failure.WithFix(err, fmt.Sprintf("run '%s --help' for usage", cmd.CommandPath()))
}

// newRootCommandWithState assembles the cobra command tree over state: a root that
// owns the persistent flags plus the "bridge" (start/stop/status) and "device"
// (add/list/remove) command groups and the hidden "daemon" body. A test passes state to
// read what the flags parsed.
func newRootCommandWithState(state *rootState) *cobra.Command {
	root := &cobra.Command{
		Use:     config.BinaryName,
		Version: version.String(), // root only; cobra answers it before PersistentPreRunE
		Short:   fmt.Sprintf("Bridge a local adb client to an %s %s remote access device", awsName, productName),
		Long: fmt.Sprintf("%s connects a local adb client to a device in an\n"+
			"%s %s remote access session. A background bridge daemon listens on a\n"+
			"local TCP port in place of the adb server and carries each adb connection\n"+
			"over the session's websocket to the device.\n"+
			"\n"+
			"  device add      Attach a device. Starts the daemon when none runs.\n"+
			"  device list     Show the attached device. Starts the daemon when none runs.\n"+
			"  device remove   Detach the attached device.\n"+
			"  bridge start    Start the daemon with custom options, such as --port.\n"+
			"  bridge stop     Stop the daemon.",
			config.BinaryName, awsName, productName),
		SilenceUsage:  true,
		SilenceErrors: true,
		// The generated `completion` command is noise for this tool's small,
		// fixed command set, so it is hidden from help and not registered.
		CompletionOptions: cobra.CompletionOptions{DisableDefaultCmd: true},
		// Initialize logging and resolve runtime paths once, before any
		// subcommand runs.
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			state.commandLineAccepted = true
			initLogging(cmd.ErrOrStderr(), state.cfg.Debug)
			paths, err := daemon.DefaultPaths()
			if err != nil {
				return &failure.Explained{Problem: "cannot use the working directory", Cause: err}
			}
			// Resolve the effective logfile once: an explicit --log overrides the
			// default runtime-dir logfile. Done here so newManager stays a pure
			// constructor with no shared-state side effects.
			if state.cfg.LogPath != "" {
				paths.LogFile = state.cfg.LogPath
			}
			state.paths = paths
			return nil
		},
	}

	bindPersistentFlags(root, &state.cfg)
	root.SetHelpCommand(hiddenHelpCommand(root))
	root.AddCommand(
		newBridgeCommand(state),
		newDeviceCommand(state),
		newDaemonCommand(state),
	)
	return root
}

// hiddenHelpCommand replaces cobra's help command with a hidden one. cobra's command
// list shows any subcommand named "help", hidden or not, so this one is named "usage"
// and keeps "help" as an alias: 'help [command]' still works.
func hiddenHelpCommand(root *cobra.Command) *cobra.Command {
	return &cobra.Command{
		Use:     "usage [command]",
		Aliases: []string{"help"},
		Short:   "Help about any command",
		Hidden:  true,
		RunE: func(_ *cobra.Command, args []string) error {
			target, _, err := root.Find(args)
			if err != nil {
				return usageError(root, err)
			}
			// cobra adds --help and --version lazily; its own help command does the same.
			target.InitDefaultHelpFlag()
			target.InitDefaultVersionFlag()
			return target.Help()
		},
	}
}

// bindPersistentFlags wires the truly global flags to the given config. Only
// --debug is genuinely cross-cutting: every command initializes logging from it,
// and it is forwarded to the detached daemon. The listener flags (--port,
// --listen-addr) and --log are NOT global: they matter only to the command that
// starts the daemon, so they are bound there (see bindListenerFlags) rather than
// advertised on every command's help. The connection, dial, and stop durations
// are intentionally not exposed: they are fixed runtime constants (see
// config.Default) to keep the command surface small.
func bindPersistentFlags(cmd *cobra.Command, cfg *config.ProxyConfig) {
	cmd.PersistentFlags().BoolVar(&cfg.Debug, "debug", cfg.Debug, "enable debug-level logging (default is info)")
}

// listenAddrUsage is hard-wrapped: pflag indents each line to the usage column.
// TODO: link the public security docs here once they exist.
const listenAddrUsage = "address to listen on\n" +
	"WARNING: changing it is not recommended. If it is\n" +
	"not loopback, any machine that can reach that\n" +
	"address can drive the attached device, with no\n" +
	"password or token. With --allow-adb-reverse, it\n" +
	"can also reach ports open only to this machine.\n" +
	"Keep it on loopback"

// bindListenerFlags wires the daemon's listener flags (--port, --listen-addr) to
// cfg. They are needed only by the two commands that stand up the daemon: `bridge
// start` (which spawns it) and the hidden `daemon` body (which is re-execed with
// them via config.AsDaemonArgs). Every other command finds the running daemon
// through its per-user control socket, so the port and address are irrelevant to
// them and are deliberately kept off their help output.
func bindListenerFlags(cmd *cobra.Command, cfg *config.ProxyConfig) {
	flags := cmd.Flags()
	flags.StringVar(&cfg.ListenAddr, "listen-addr", cfg.ListenAddr, listenAddrUsage)
	flags.IntVar(&cfg.ListenPort, "port", cfg.ListenPort, "local TCP port to listen on for adb clients")
}

// bindReverseFlag binds --allow-adb-reverse to cfg. Scoped like the listener flags, for
// the same reason: every other command talks to a daemon that already decided.
func bindReverseFlag(cmd *cobra.Command, cfg *config.ProxyConfig) {
	cmd.Flags().BoolVar(&cfg.ReverseEnabled, "allow-adb-reverse", cfg.ReverseEnabled,
		"allow device-originated reverse connections ('adb reverse'); off by default")
}

// initLogging installs the global slog text handler on errOut at info level,
// debug with --debug. errOut is the command's stderr, so logs never mix with
// command output on stdout; a test passes a buffer.
func initLogging(errOut io.Writer, debug bool) {
	level := slog.LevelInfo
	if debug {
		level = slog.LevelDebug
	}
	handler := slog.NewTextHandler(errOut, &slog.HandlerOptions{Level: level})
	slog.SetDefault(slog.New(handler))
}

// newManager builds a daemon.DaemonManager for the commands that start, stop or probe
// the daemon: bridge start/stop/status and the `device add`/`device list` auto-start. The
// effective logfile path in s.paths is already resolved in
// PersistentPreRunE, so this is a pure constructor. It passes no remote-access
// client: Start, Stop, and Status never resolve an ARN (they signal and inspect a
// process), so these commands must not pay the AWS config load or fail when no
// credentials are configured. Only the daemon body resolves, and it builds its own
// client (see newDaemonManager).
func (s *rootState) newManager() *daemon.DaemonManager {
	return daemon.NewManager(s.cfg, s.paths, nil, slog.Default())
}

// newDaemonManager builds the DaemonManager for the re-execed daemon body, the one
// component that actually resolves session ARNs. It constructs the AWS-backed
// remote-access client eagerly and synchronously at daemon launch, so the config
// load and credential-chain walk are paid once here rather than on every attach and
// refresh tick.
func (s *rootState) newDaemonManager(ctx context.Context) (*daemon.DaemonManager, error) {
	logger := slog.Default()
	remoteAccessClient, err := session.NewRemoteAccessClient(ctx, logger)
	if err != nil {
		return nil, err
	}
	return daemon.NewManager(s.cfg, s.paths, remoteAccessClient, logger), nil
}

// controlClient builds a control-channel client targeting the daemon's socket.
func (s *rootState) controlClient() *control.DaemonClient {
	return control.NewDaemonClient(s.paths.ControlSocket)
}
