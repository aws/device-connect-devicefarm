// Package session resolves an AWS Device Farm remote access session ARN into the
// websocket endpoint and human-readable device label the proxy needs. It is the
// only package that talks to AWS. It runs inside the detached daemon, which
// resolves the ARN on attach and on a periodic refresh, because the interactive
// endpoint is time-limited and must be renewed while a device stays attached.
//
// Public surface: RemoteAccessClient (the application-layer interface the daemon
// depends on), RemoteAccessInfo (its result), and NewRemoteAccessClient (the real,
// AWS-backed implementation). Everything else - the SDK-shaped getter, the ARN
// helper, the config loading - is internal to this package, so no aws-sdk-go-v2
// type crosses the package boundary.
package session

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/arn"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/devicefarm"
	"github.com/aws/aws-sdk-go-v2/service/devicefarm/types"
)

// maxRetryAttempts caps how many times the SDK tries GetRemoteAccessSession
// before returning an error. The daemon resolves synchronously on attach (a user
// is waiting on the control channel) and again on every refresh tick, so the
// budget is kept small: two attempts smooths a single transient blip without
// letting a persistent outage stall the attach for long. Any error the SDK
// surfaces after this budget causes the daemon to detach the device.
const maxRetryAttempts = 2

// clientRegion is the region the Device Farm client is built for. Device Farm's
// remote access control plane is only offered in us-west-2 today, so every
// session ARN this tool sees names that region and the client can be built once,
// eagerly, instead of per request. GetSession still rejects an ARN from another
// region rather than silently calling the wrong endpoint, so if Device Farm adds
// regions this fails loudly instead of misbehaving.
const clientRegion = "us-west-2"

// minimumAndroidMajor is the oldest Android release this client serves. The adb
// relay behind a remote access session does not work below it, so GetSession
// refuses an older Android device.
const minimumAndroidMajor = 9

// RemoteAccessInfo is the outcome of resolving a session ARN: the websocket URL
// to dial (the interactive endpoint verbatim, path-less; each dial site names its
// route through bridge.Route) and the device label to show the user.
type RemoteAccessInfo struct {
	Endpoint string
	Label    string
}

// RemoteAccessClient looks up a Device Farm remote access session by ARN and
// returns the websocket endpoint and device label the proxy needs. It is the
// application-layer abstraction the daemon depends on: the daemon holds one for
// its lifetime and calls GetSession on attach and on each refresh tick. The real
// implementation talks to AWS (NewRemoteAccessClient); tests supply a fake, so no
// AWS SDK type leaks into the daemon or its tests.
type RemoteAccessClient interface {
	GetSession(ctx context.Context, sessionARN string) (RemoteAccessInfo, error)
}

// remoteAccessGetter is the subset of the generated Device Farm client the
// resolver needs. The generated *devicefarm.Client satisfies it. Keeping it
// unexported confines the SDK shape to this package.
type remoteAccessGetter interface {
	GetRemoteAccessSession(
		ctx context.Context,
		params *devicefarm.GetRemoteAccessSessionInput,
		optFns ...func(*devicefarm.Options),
	) (*devicefarm.GetRemoteAccessSessionOutput, error)
}

// awsRemoteAccessClient is the real RemoteAccessClient. It holds one Device Farm
// client for its lifetime: building one loads the AWS config, which walks the
// credential chain in order, so it is not cheap enough to redo on every call. The
// client is region-bound and Device Farm remote access is single-region today (see
// clientRegion), so one instance serves every session and the daemon reuses it
// across refresh ticks and device swaps.
type awsRemoteAccessClient struct {
	getter remoteAccessGetter
	logger *slog.Logger
}

// NewRemoteAccessClient builds the real, AWS-backed RemoteAccessClient the daemon
// injects into its manager. It loads the AWS config and constructs the Device Farm
// client eagerly, so the cost is paid once at daemon startup rather than on the
// attach path where a user is waiting. Credentials themselves resolve lazily on
// first use, so an absent credential chain does not fail here; a malformed profile
// or AWS_* value still does. A nil logger defaults to slog.Default.
func NewRemoteAccessClient(ctx context.Context, logger *slog.Logger) (RemoteAccessClient, error) {
	if logger == nil {
		logger = slog.Default()
	}
	cfg, err := config.LoadDefaultConfig(
		ctx,
		config.WithRegion(clientRegion),
		config.WithRetryMode(aws.RetryModeAdaptive),
		config.WithRetryMaxAttempts(maxRetryAttempts),
	)
	if err != nil {
		return nil, fmt.Errorf("load AWS config: %w", err)
	}
	return awsRemoteAccessClient{getter: devicefarm.NewFromConfig(cfg), logger: logger}, nil
}

// GetSession looks up the remote access session named by sessionARN and maps the
// response to a RemoteAccessInfo (the interactive endpoint verbatim, plus the
// device label). Validating the ARN is the explicit first step,
// so a malformed ARN, a non-Device-Farm ARN, or one from a region this client was
// not built for fails fast, before any network call. An Android device below
// minimumAndroidMajor is refused after the call (only the response names the
// device) and before the endpoint check, so a session still PREPARING on such a
// device is refused at once, not after a wait and a retry.
func (c awsRemoteAccessClient) GetSession(ctx context.Context, sessionARN string) (RemoteAccessInfo, error) {
	if err := validateSessionARN(sessionARN); err != nil {
		return RemoteAccessInfo{}, err
	}

	out, err := c.getter.GetRemoteAccessSession(ctx, &devicefarm.GetRemoteAccessSessionInput{
		Arn: aws.String(sessionARN),
	})
	if err != nil {
		return RemoteAccessInfo{}, fmt.Errorf("GetRemoteAccessSession %q: %w", sessionARN, err)
	}

	remoteAccessSession := out.RemoteAccessSession
	if remoteAccessSession == nil {
		return RemoteAccessInfo{}, fmt.Errorf("remote access session %q not found", sessionARN)
	}
	if err := refuseUnsupportedAndroid(sessionARN, remoteAccessSession.Device, c.logger); err != nil {
		return RemoteAccessInfo{}, err
	}
	if remoteAccessSession.Endpoints == nil || aws.ToString(remoteAccessSession.Endpoints.InteractiveEndpoint) == "" {
		return RemoteAccessInfo{}, fmt.Errorf(
			"remote access session %q has no interactive endpoint yet (status %s); is it RUNNING?",
			sessionARN, remoteAccessSession.Status,
		)
	}

	return RemoteAccessInfo{
		Endpoint: aws.ToString(remoteAccessSession.Endpoints.InteractiveEndpoint),
		Label:    deviceLabel(remoteAccessSession.Device),
	}, nil
}

// validateSessionARN rejects a session ARN the client cannot serve: one that is
// malformed, not a Device Farm ARN, or in a region other than the one the client
// was built for. A typo fails fast (surfaced as the daemon's add error) rather
// than as an opaque AWS error, and an ARN from a region Device Farm may add later
// fails loudly rather than being sent to the wrong regional endpoint (the SDK
// would otherwise synthesize a plausible-looking endpoint for it).
func validateSessionARN(sessionARN string) error {
	parsed, err := arn.Parse(sessionARN)
	if err != nil {
		return fmt.Errorf("parse session ARN %q: %w", sessionARN, err)
	}
	if parsed.Service != "devicefarm" {
		return fmt.Errorf("ARN %q is not a Device Farm ARN (service %q)", sessionARN, parsed.Service)
	}
	if parsed.Region != clientRegion {
		return fmt.Errorf(
			"session ARN %q is in region %q, but Device Farm remote access is only supported in %s",
			sessionARN, parsed.Region, clientRegion,
		)
	}
	return nil
}

// refuseUnsupportedAndroid rejects a session on an Android device below
// minimumAndroidMajor. iOS and a missing device pass: refusing them is a separate
// decision. An OS version that does not parse passes with a warning: the floor is
// a compatibility rule, not a security bound, so an odd format must not lock out a
// working device.
func refuseUnsupportedAndroid(sessionARN string, device *types.Device, logger *slog.Logger) error {
	if device == nil || device.Platform != types.DevicePlatformAndroid {
		return nil
	}
	osVersion := aws.ToString(device.Os)
	major, ok := androidMajor(osVersion)
	if !ok {
		logger.Warn("device OS version not parsed; allowing the session", "os", osVersion, "arn", sessionARN)
		return nil
	}
	if major < minimumAndroidMajor {
		return fmt.Errorf(
			"remote access session %q is on an Android %s device, but this client only supports Android %d and above",
			sessionARN, osVersion, minimumAndroidMajor,
		)
	}
	return nil
}

// androidMajor reads the integer before the first dot of the OS version Device
// Farm reports: "9" is 9, "8.0.0" is 8; "", "Q" and "Android 9" do not parse.
func androidMajor(osVersion string) (int, bool) {
	head, _, _ := strings.Cut(strings.TrimSpace(osVersion), ".")
	major, err := strconv.Atoi(head)
	if err != nil {
		return 0, false
	}
	return major, true
}

// deviceLabel renders the human-readable device description shown to the user,
// e.g. "Google Pixel 9 (OS 16)". Missing fields degrade to placeholders rather
// than failing resolution: the label is cosmetic, not load-bearing.
func deviceLabel(device *types.Device) string {
	name := "unknown device"
	osVersion := "unknown"
	if device != nil {
		if n := aws.ToString(device.Name); n != "" {
			name = n
		}
		if o := aws.ToString(device.Os); o != "" {
			osVersion = o
		}
	}
	return fmt.Sprintf("%s (OS %s)", name, osVersion)
}
