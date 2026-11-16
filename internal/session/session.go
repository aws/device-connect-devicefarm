// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

// Package session resolves an AWS Device Farm remote access session ARN into the
// websocket endpoint and human-readable device label the proxy needs. It is the
// only package that talks to AWS. It runs inside the detached daemon, which
// resolves the ARN on attach and on a periodic refresh, because the interactive
// endpoint is time-limited and must be renewed while a device stays attached.
//
// Public surface: RemoteAccessClient (the application-layer interface the daemon
// depends on), RemoteAccessInfo (its result), NewRemoteAccessClient (the real,
// AWS-backed implementation), the Err sentinels a GetSession failure carries,
// with SessionEndedError for the status word, RequestRejectedError for the
// service's error code and UnsupportedDeviceError for the refused device,
// ValidateSessionARN (bridge start and the add handler run it first),
// and ResolveCredentials with its CredentialSummary (the credential chain's
// source, for the doctor). Everything else - the
// SDK-shaped getter, the config loading - is internal to this
// package, so no aws-sdk-go-v2 type crosses the package boundary.
package session

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/arn"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/devicefarm"
	"github.com/aws/aws-sdk-go-v2/service/devicefarm/types"
	"github.com/aws/smithy-go/logging"

	"github.com/aws/device-connect-devicefarm/internal/wording"
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

// MaxSessionLength is the service's session limit, per the Device Farm developer guide, "Using a
// remote access session"
// (https://docs.aws.amazon.com/devicefarm/latest/developerguide/how-to-use-session.html). The
// API reports no expiry; the daemon reads the time left from the session's status route at the
// add and derives the start as the expiry minus this.
const MaxSessionLength = 150 * time.Minute

// Sentinels for errors.Is. GetSession wraps them into its texts, where their words read in
// place; the SDK's own text stays inside the wrap.
var (
	// ErrInvalidARN: not a Device Farm session ARN this client can serve.
	ErrInvalidARN = errors.New(wording.SessionInvalidARN)
	// ErrAccessDenied: the credentials lack devicefarm:GetRemoteAccessSession on the session.
	ErrAccessDenied = errors.New(wording.SessionAccessDenied)
	// ErrSessionNotFound: no session has this ARN in this account and region.
	ErrSessionNotFound = errors.New(wording.SessionNotFound)
	// ErrUnsupportedDevice: the session's device is one this client does not serve (a non-Android
	// platform, or an Android release below minimumAndroidMajor). UnsupportedDeviceError matches it
	// and keeps its own text.
	ErrUnsupportedDevice = errors.New(wording.SessionUnsupportedDevice)
	// ErrSessionEnded: the session is COMPLETED or STOPPING. SessionEndedError carries which.
	ErrSessionEnded = errors.New(wording.SessionEnded)
	// ErrNoEndpointYet: the session is not RUNNING, so it has no endpoint to dial.
	ErrNoEndpointYet = errors.New(wording.SessionNoEndpointYet)
	// ErrRequestRejected: the service answered with a 4xx that is not a throttle: the request
	// itself is bad (credentials, signature, token, ARN shape). RequestRejectedError carries the
	// code. Throttles, 5xx and transport failures carry no sentinel.
	ErrRequestRejected = errors.New(wording.SessionRequestRejected)
	// ErrNoCredentials: the credential chain gave no credentials (none configured, or a provider
	// such as SSO that can no longer refresh). The provider's text stays; this adds no words.
	ErrNoCredentials = errors.New("no usable AWS credentials")
)

// RequestRejectedError is the ErrRequestRejected failure with the service's error code, for a
// caller that tells the user which.
type RequestRejectedError struct {
	Code  string
	Cause error
}

func (rejected *RequestRejectedError) Error() string {
	return fmt.Sprintf("%v (%s): %v", ErrRequestRejected, rejected.Code, rejected.Cause)
}

func (rejected *RequestRejectedError) Unwrap() error { return rejected.Cause }

func (*RequestRejectedError) Is(target error) bool { return target == ErrRequestRejected }

// expiredCredentialCodes are the service's codes for expired credentials.
var expiredCredentialCodes = map[string]bool{"ExpiredTokenException": true, "ExpiredToken": true}

// CredentialsExpired reports whether the code names expired credentials.
func (rejected *RequestRejectedError) CredentialsExpired() bool {
	return expiredCredentialCodes[rejected.Code]
}

// throttleErrorCodes are the API error codes the SDK treats as throttles: aws/retry
// DefaultThrottleErrorCodes (standard.go) in aws-sdk-go-v2 v1.41.12, copied so the retry package
// stays out of this package's imports.
var throttleErrorCodes = map[string]bool{
	"Throttling":                             true,
	"ThrottlingException":                    true,
	"ThrottledException":                     true,
	"RequestThrottledException":              true,
	"TooManyRequestsException":               true,
	"ProvisionedThroughputExceededException": true,
	"TransactionInProgressException":         true,
	"RequestLimitExceeded":                   true,
	"BandwidthLimitExceeded":                 true,
	"LimitExceededException":                 true,
	"RequestThrottled":                       true,
	"SlowDown":                               true,
	"PriorRequestNotComplete":                true,
	"EC2ThrottledException":                  true,
}

// SessionEndedError is the ErrSessionEnded failure with the status word that ended the session,
// for a caller that tells the user which.
type SessionEndedError struct {
	SessionARN string
	Status     string
}

func (ended *SessionEndedError) Error() string {
	return fmt.Sprintf(wording.SessionEndedWithStatusFormat, ended.SessionARN, ended.Status)
}

func (*SessionEndedError) Is(target error) bool { return target == ErrSessionEnded }

// RemoteAccessInfo is the outcome of resolving a session ARN: the websocket URL
// to dial (the interactive endpoint verbatim, path-less; each dial site names its
// route through bridge.Route), the ARN the response names (empty when none; can
// differ from the one asked) and the device label to show the user. The session times are
// not here: the daemon reads the time left from the session's status route at the add.
type RemoteAccessInfo struct {
	Endpoint string
	ARN      string
	Label    string
}

// ReturnedARN is the ARN the service names the session by: the response's, else
// askedARN.
func (info RemoteAccessInfo) ReturnedARN(askedARN string) string {
	return cmp.Or(info.ARN, askedARN)
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
// client for its lifetime: building one loads the AWS config, which reads the
// environment and the shared config files to pick a credential provider, so it is
// not cheap enough to redo on every call. The
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
	cfg, err := loadConfig(ctx)
	if err != nil {
		return nil, err
	}
	return awsRemoteAccessClient{getter: devicefarm.NewFromConfig(cfg), logger: logger}, nil
}

// loadConfig is the AWS config the Device Farm client and ResolveCredentials share.
// The SDK's default logger writes its own warnings straight to stderr, past slog;
// every client built from this config, credential providers included, drops them.
func loadConfig(ctx context.Context) (aws.Config, error) {
	cfg, err := config.LoadDefaultConfig(
		ctx,
		config.WithRegion(clientRegion),
		config.WithRetryMode(aws.RetryModeAdaptive),
		config.WithRetryMaxAttempts(maxRetryAttempts),
		config.WithLogger(logging.Nop{}),
	)
	if err != nil {
		return aws.Config{}, fmt.Errorf("load AWS config: %w", err)
	}
	if cfg.Credentials != nil {
		cfg.Credentials = markedCredentials{provider: cfg.Credentials}
	}
	return cfg, nil
}

// apiErrorCode and httpResponse match the SDK's API error and its HTTP response by shape, so
// its error packages stay out of the package's non-test imports.
type apiErrorCode interface{ ErrorCode() string }

type httpResponse interface{ HTTPStatusCode() int }

// classifyAPIError maps an API error to its sentinel: three codes by name, then any other 4xx
// that is not a throttle as ErrRequestRejected. Throttles (a throttle code, or a 429 whatever its
// code), 5xx and errors with no response pass through. The call resolves credentials first, so
// this scrubs a credential_process failure too.
func classifyAPIError(err error) error {
	err = scrubCredentialProcess(err)
	code := ""
	var apiErr apiErrorCode
	if errors.As(err, &apiErr) {
		code = apiErr.ErrorCode()
	}
	switch code {
	case "AccessDeniedException":
		return fmt.Errorf("%w: %w", ErrAccessDenied, err)
	case "NotFoundException":
		return fmt.Errorf(wording.SessionNotFoundCauseFormat, ErrSessionNotFound, err)
	case "ArgumentException":
		return fmt.Errorf("%w: %w", ErrInvalidARN, err)
	}
	var response httpResponse
	if errors.As(err, &response) && !throttleErrorCodes[code] {
		status := response.HTTPStatusCode()
		if status >= 400 && status < 500 && status != http.StatusTooManyRequests {
			return &RequestRejectedError{Code: code, Cause: err}
		}
	}
	return err
}

// GetSession looks up the remote access session named by sessionARN and maps the
// response to a RemoteAccessInfo (the interactive endpoint verbatim, plus the
// device label). Validating the ARN is the explicit first step,
// so a malformed ARN, a non-Device-Farm ARN, or one from a region this client was
// not built for fails fast, before any network call. A COMPLETED or STOPPING
// session is reported as ended whatever its device. Then a non-Android device, or
// an Android device below minimumAndroidMajor, is refused as an
// UnsupportedDeviceError (only the response names the device) before the endpoint
// check, so a session still PREPARING on such a device is refused at once, not
// after a wait and a retry. Only a RUNNING session with an endpoint resolves.
func (c awsRemoteAccessClient) GetSession(ctx context.Context, sessionARN string) (RemoteAccessInfo, error) {
	if err := ValidateSessionARN(sessionARN); err != nil {
		return RemoteAccessInfo{}, err
	}

	out, err := c.getter.GetRemoteAccessSession(ctx, &devicefarm.GetRemoteAccessSessionInput{
		Arn: aws.String(sessionARN),
	})
	if err != nil {
		return RemoteAccessInfo{}, fmt.Errorf(wording.SessionCallFailedFormat, sessionARN, classifyAPIError(err))
	}

	remoteAccessSession := out.RemoteAccessSession
	if remoteAccessSession == nil {
		return RemoteAccessInfo{}, fmt.Errorf(wording.SessionNotFoundFormat, sessionARN, ErrSessionNotFound)
	}
	// An ended session is ended whatever its device: the floor below is about attaching.
	status := remoteAccessSession.Status
	if status == types.ExecutionStatusCompleted || status == types.ExecutionStatusStopping {
		return RemoteAccessInfo{}, &SessionEndedError{SessionARN: sessionARN, Status: string(status)}
	}
	if err := refuseUnsupportedDevice(sessionARN, remoteAccessSession.Device, c.logger); err != nil {
		return RemoteAccessInfo{}, err
	}
	endpoint := ""
	if remoteAccessSession.Endpoints != nil {
		endpoint = aws.ToString(remoteAccessSession.Endpoints.InteractiveEndpoint)
	}
	// Only RUNNING is dialable: a pending session has no endpoint yet.
	if status != types.ExecutionStatusRunning || endpoint == "" {
		return RemoteAccessInfo{}, fmt.Errorf(wording.SessionNoEndpointFormat, sessionARN, ErrNoEndpointYet, status)
	}

	return RemoteAccessInfo{Endpoint: endpoint, ARN: aws.ToString(remoteAccessSession.Arn),
		Label: deviceLabel(remoteAccessSession.Device)}, nil
}

// ValidateSessionARN rejects a session ARN the client cannot serve: one that is
// malformed, outside the aws partition, not a Device Farm ARN, in a region other
// than the one the client was built for, or whose resource is not a session (a
// project or device ARN). Every refusal matches ErrInvalidARN. A typo fails fast (a
// usage error or the daemon's add error) rather than as an opaque AWS error, and an ARN from a region Device Farm may add later
// fails loudly rather than being sent to the wrong regional endpoint (the SDK
// would otherwise synthesize a plausible-looking endpoint for it).
func ValidateSessionARN(sessionARN string) error {
	parsed, err := arn.Parse(sessionARN)
	if err != nil {
		return fmt.Errorf(wording.SessionParseARNFormat, ErrInvalidARN, sessionARN, err)
	}
	if parsed.Partition != "aws" {
		return fmt.Errorf(wording.SessionNotAWSARNFormat, ErrInvalidARN, sessionARN, parsed.Partition)
	}
	if parsed.Service != "devicefarm" {
		return fmt.Errorf(wording.SessionNotDeviceFarmARNFormat, ErrInvalidARN, sessionARN, parsed.Service)
	}
	if parsed.Region != clientRegion {
		return fmt.Errorf(wording.SessionWrongRegionFormat, ErrInvalidARN, sessionARN, parsed.Region, clientRegion)
	}
	if kind, id, found := strings.Cut(parsed.Resource, ":"); !found || kind != "session" || id == "" {
		return fmt.Errorf(wording.SessionNotSessionARNFormat, ErrInvalidARN, sessionARN, parsed.Resource, clientRegion)
	}
	return nil
}

// androidPlatform is the UnsupportedDeviceError.Platform word for an Android device.
const androidPlatform = "Android"

// UnsupportedDeviceError is the refusal of a device this client cannot serve: a
// non-Android platform, or an Android release below minimumAndroidMajor. Platform
// is the word the text uses ("Android", "iOS", or the raw value). OSVersion is the
// release the response reports. It matches ErrUnsupportedDevice for errors.Is. An
// in-process caller tells the two refusals apart with errors.As and
// BelowAndroidFloor, not by reading the text; the control channel carries only the
// text.
type UnsupportedDeviceError struct {
	SessionARN string
	Platform   string
	OSVersion  string
}

// BelowAndroidFloor reports whether the device is Android but older than
// minimumAndroidMajor. Otherwise the platform itself is unsupported.
func (refusal UnsupportedDeviceError) BelowAndroidFloor() bool {
	return refusal.Platform == androidPlatform
}

func (UnsupportedDeviceError) Is(target error) bool { return target == ErrUnsupportedDevice }

// Error is the sentence the user reads on stderr.
func (refusal UnsupportedDeviceError) Error() string {
	if !refusal.BelowAndroidFloor() {
		return fmt.Sprintf(wording.SessionNotAndroidFormat, refusal.SessionARN, refusal.Platform)
	}
	return fmt.Sprintf(wording.SessionAndroidTooOldFormat, refusal.SessionARN, refusal.OSVersion, minimumAndroidMajor)
}

// refuseUnsupportedDevice returns an UnsupportedDeviceError for a non-Android
// device or an Android device below minimumAndroidMajor. A missing device or empty
// platform passes: the SDK Platform is a plain string, so absent and empty look the
// same, and the gate refuses only what the response names as non-Android. An
// Android version that does not parse passes with a warning. The gate is a
// compatibility rule, not a security bound; an odd response must not lock out a
// working device.
func refuseUnsupportedDevice(sessionARN string, device *types.Device, logger *slog.Logger) error {
	if device == nil || device.Platform == "" {
		return nil
	}
	refusal := UnsupportedDeviceError{
		SessionARN: sessionARN,
		Platform:   platformName(device.Platform),
		OSVersion:  aws.ToString(device.Os),
	}
	if device.Platform != types.DevicePlatformAndroid {
		return refusal
	}
	major, ok := androidMajor(refusal.OSVersion)
	if !ok {
		logger.Warn("device OS version not parsed; allowing the session", "os", refusal.OSVersion, "arn", sessionARN)
		return nil
	}
	if major < minimumAndroidMajor {
		return refusal
	}
	return nil
}

// platformName maps a Device Farm platform value to the refusal text's word:
// ANDROID to "Android", IOS to "iOS", anything else raw.
func platformName(platform types.DevicePlatform) string {
	switch platform {
	case types.DevicePlatformAndroid:
		return androidPlatform
	case types.DevicePlatformIos:
		return "iOS"
	default:
		return string(platform)
	}
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
	name := wording.SessionUnknownDeviceName
	osVersion := wording.SessionUnknownOSVersion
	if device != nil {
		if n := aws.ToString(device.Name); n != "" {
			name = n
		}
		if o := aws.ToString(device.Os); o != "" {
			osVersion = o
		}
	}
	return fmt.Sprintf(wording.SessionDeviceLabelFormat, name, osVersion)
}
