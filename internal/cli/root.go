package cli

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/spf13/cobra"

	"github.com/aws/device-connect-devicefarm/internal/config"
	"github.com/aws/device-connect-devicefarm/internal/control"
	"github.com/aws/device-connect-devicefarm/internal/daemon"
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
}

// Execute builds the command tree and runs it. It is the single entrypoint
// called from main.
func Execute() error {
	return newRootCommand().Execute()
}

// newRootCommand assembles the command tree over a fresh default configuration.
func newRootCommand() *cobra.Command {
	return newRootCommandWithState(&rootState{cfg: config.Default()})
}

// newRootCommandWithState assembles the cobra command tree over state: a root that
// owns the persistent flags plus the "bridge" (start/stop/status) and "device"
// (add/remove) command groups and the hidden "daemon" body. A test passes state to
// read what the flags parsed.
func newRootCommandWithState(state *rootState) *cobra.Command {
	root := &cobra.Command{
		Use:     config.BinaryName,
		Version: version.String(), // root only; cobra answers it before PersistentPreRunE
		Short:   fmt.Sprintf("Bridge a local adb client to an %s %s remote access device", awsName, productName),
		Long: fmt.Sprintf("%s presents itself to local adb clients as the adb host. It listens\n"+
			"on a local TCP port and pipes each connection over the websocket of an %s\n"+
			"%s remote access session to the device's adb server. 'device add' attaches a\n"+
			"device and starts the background daemon when none runs; 'device remove' detaches\n"+
			"it. 'bridge start' starts the daemon with custom options; 'bridge stop' stops it.",
			config.BinaryName, awsName, productName),
		SilenceUsage:  true,
		SilenceErrors: true,
		// The generated `completion` command is noise for this tool's small,
		// fixed command set, so it is hidden from help and not registered.
		CompletionOptions: cobra.CompletionOptions{DisableDefaultCmd: true},
		// Initialize logging and resolve runtime paths once, before any
		// subcommand runs.
		PersistentPreRunE: func(_ *cobra.Command, _ []string) error {
			initLogging(state.cfg.Debug)
			paths, err := daemon.DefaultPaths()
			if err != nil {
				return err
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
				return err
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
// advertised on every command's help. The connection, dial, keep-alive, and stop
// durations are intentionally not exposed: they are fixed runtime constants (see
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

// initLogging installs the global slog logger, writing human-readable text to
// stderr at debug level when debug is set and info otherwise. Stderr (not stdout)
// is used so the detached daemon's logs land in the redirected logfile and never
// intermix with proxied data.
func initLogging(debug bool) {
	level := slog.LevelInfo
	if debug {
		level = slog.LevelDebug
	}
	handler := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})
	slog.SetDefault(slog.New(handler))
}

// newManager builds a daemon.DaemonManager for the commands that start, stop or probe
// the daemon: bridge start/stop/status and the `device add` auto-start. The
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
