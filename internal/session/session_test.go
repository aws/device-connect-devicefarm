package session

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/devicefarm"
	"github.com/aws/aws-sdk-go-v2/service/devicefarm/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// hermeticAWSEnv pins the AWS environment so a test that builds a real client
// loads config identically on any host. Placeholder credentials stand in for a
// missing ~/.aws setup. AWS_PROFILE is cleared because a set profile switches the
// SDK to a strict shared-config loader that fails when the named profile is
// absent, so a developer or CI host that exports one would otherwise break these
// tests for reasons unrelated to what they assert. Legacy defaults mode keeps the
// load from consulting IMDS.
func hermeticAWSEnv(t *testing.T) {
	t.Helper()
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	t.Setenv("AWS_PROFILE", "")
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(t.TempDir(), "no-config"))
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(t.TempDir(), "no-credentials"))
	t.Setenv("AWS_DEFAULTS_MODE", "legacy")
}

// closedEndpoint returns the URL of a local port that has been bound and released,
// so a connection to it is refused immediately. It lets a test drive the real
// GetSession path to a deterministic connection failure without a real AWS call.
func closedEndpoint(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := listener.Addr().String()
	require.NoError(t, listener.Close())
	return "http://" + addr
}

const testSessionARN = "arn:aws:devicefarm:us-west-2:123456789012:session:abc/def/00000"

// fakeGetter is a remoteAccessGetter that returns canned output, so the session
// mapping is exercised without a real AWS call. It records the ARN it was asked
// for and counts calls.
type fakeGetter struct {
	out *devicefarm.GetRemoteAccessSessionOutput
	err error

	gotARN string
	calls  int
}

func (f *fakeGetter) GetRemoteAccessSession(
	_ context.Context,
	params *devicefarm.GetRemoteAccessSessionInput,
	_ ...func(*devicefarm.Options),
) (*devicefarm.GetRemoteAccessSessionOutput, error) {
	f.gotARN = aws.ToString(params.Arn)
	f.calls++
	return f.out, f.err
}

func sessionOutput(endpoint, name, os string) *devicefarm.GetRemoteAccessSessionOutput {
	return &devicefarm.GetRemoteAccessSessionOutput{
		RemoteAccessSession: &types.RemoteAccessSession{
			Endpoints: &types.RemoteAccessEndpoints{InteractiveEndpoint: aws.String(endpoint)},
			Device:    &types.Device{Name: aws.String(name), Os: aws.String(os)},
		},
	}
}

// platformSessionOutput is sessionOutput plus the device platform the Android
// floor keys on.
func platformSessionOutput(platform types.DevicePlatform, os string) *devicefarm.GetRemoteAccessSessionOutput {
	out := sessionOutput("wss://host.example/ws", "Lab device", os)
	out.RemoteAccessSession.Device.Platform = platform
	return out
}

// newClient wraps a fake getter with a discarding logger. A test that reads the
// log builds its own client.
func newClient(getter remoteAccessGetter) awsRemoteAccessClient {
	return awsRemoteAccessClient{getter: getter, logger: slog.New(slog.DiscardHandler)}
}

func TestGivenRunningSessionWhenResolvedThenReturnsEndpointAndLabel(t *testing.T) {
	fake := &fakeGetter{out: sessionOutput("wss://host.example/ws", "Google Pixel 9", "16")}

	got, err := newClient(fake).GetSession(context.Background(), testSessionARN)

	require.NoError(t, err)
	assert.Equal(t, testSessionARN, fake.gotARN, "the resolved ARN must be passed through to the API")
	assert.Equal(t, "Google Pixel 9 (OS 16)", got.Label)
	assert.Equal(t, "wss://host.example/ws", got.Endpoint, "the interactive endpoint is returned verbatim")
}

func TestGivenResolvedSessionWhenReturnedThenEndpointCarriesNoPathParameter(t *testing.T) {
	// The route is the dialer's business: a path baked in here would be a second
	// source of truth for it. Any query the endpoint already carries stays untouched.
	fake := &fakeGetter{out: sessionOutput("wss://host.example/ws?token=xyz", "Pixel", "15")}

	got, err := newClient(fake).GetSession(context.Background(), testSessionARN)

	require.NoError(t, err)
	assert.Equal(t, "wss://host.example/ws?token=xyz", got.Endpoint,
		"byte-exact pass-through: a presigned query must not be re-encoded here")
	parsed, err := url.Parse(got.Endpoint)
	require.NoError(t, err)
	assert.False(t, parsed.Query().Has("path"), "the resolved endpoint must not name a route")
}

func TestGivenSessionWithoutInteractiveEndpointWhenResolvedThenErrors(t *testing.T) {
	tests := []struct {
		name string
		out  *devicefarm.GetRemoteAccessSessionOutput
	}{
		{name: "nil session", out: &devicefarm.GetRemoteAccessSessionOutput{}},
		{
			name: "nil endpoints",
			out:  &devicefarm.GetRemoteAccessSessionOutput{RemoteAccessSession: &types.RemoteAccessSession{}},
		},
		{
			name: "empty interactive endpoint",
			out: &devicefarm.GetRemoteAccessSessionOutput{
				RemoteAccessSession: &types.RemoteAccessSession{
					Endpoints: &types.RemoteAccessEndpoints{InteractiveEndpoint: aws.String("")},
				},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := newClient(&fakeGetter{out: tc.out}).GetSession(context.Background(), testSessionARN)
			require.Error(t, err)
		})
	}
}

func TestGivenAPIErrorWhenResolvedThenErrorPropagated(t *testing.T) {
	fake := &fakeGetter{err: errors.New("access denied")}

	_, err := newClient(fake).GetSession(context.Background(), testSessionARN)

	require.Error(t, err)
}

func TestGivenSessionMissingDeviceFieldsWhenResolvedThenLabelDegradesGracefully(t *testing.T) {
	// No device means no platform or version to check, so the floor passes.
	out := &devicefarm.GetRemoteAccessSessionOutput{
		RemoteAccessSession: &types.RemoteAccessSession{
			Endpoints: &types.RemoteAccessEndpoints{InteractiveEndpoint: aws.String("wss://host.example/ws")},
		},
	}

	got, err := newClient(&fakeGetter{out: out}).GetSession(context.Background(), testSessionARN)

	require.NoError(t, err)
	assert.Equal(t, "unknown device (OS unknown)", got.Label)
}

func TestGivenPinnedEnvWhenClientBuiltThenRegionAndRetryConfigured(t *testing.T) {
	// Building the client loads AWS config but makes no network call (credentials
	// resolve lazily). The generated *devicefarm.Client's Options expose what the
	// constructor promises: the single supported region and the small adaptive
	// retry budget.
	hermeticAWSEnv(t)

	client, err := NewRemoteAccessClient(context.Background(), nil)

	require.NoError(t, err)
	awsClient, ok := client.(awsRemoteAccessClient)
	require.True(t, ok, "the real client must be an awsRemoteAccessClient")
	getter, ok := awsClient.getter.(*devicefarm.Client)
	require.True(t, ok, "the real getter must be a *devicefarm.Client")
	options := getter.Options()
	assert.Equal(t, clientRegion, options.Region)
	assert.Equal(t, aws.RetryModeAdaptive, options.RetryMode)
	assert.Equal(t, maxRetryAttempts, options.RetryMaxAttempts)
	assert.Same(t, slog.Default(), awsClient.logger, "a nil logger must default to the process logger")
}

func TestGivenRepeatedCallsWhenSessionsFetchedThenOneGetterServesEveryCall(t *testing.T) {
	// The reason the client is built once: every GetSession must go through the same
	// getter rather than constructing a new one (and reloading AWS config, which
	// walks the credential chain) per call. A counting getter injected into the real
	// implementation proves the member is reused across calls.
	counting := &fakeGetter{out: sessionOutput("wss://host.example/ws", "Pixel", "16")}
	client := newClient(counting)

	for range 3 {
		_, err := client.GetSession(context.Background(), testSessionARN)
		require.NoError(t, err)
	}

	assert.Equal(t, 3, counting.calls, "every call must reuse the one getter the client holds")
}

func TestGivenValidARNWhenSessionFetchedThenCallsThroughToTheAPI(t *testing.T) {
	// Drive the whole GetSession path with a valid ARN: it must pass ARN validation
	// and issue the GetRemoteAccessSession call. Pointing the SDK at a closed local
	// port (via AWS_ENDPOINT_URL_DEVICE_FARM) makes that call fail with a connection
	// error deterministically, so the happy-path wiring runs without reaching real
	// AWS. A short context bounds the SDK's adaptive retry so the test cannot hang.
	hermeticAWSEnv(t)
	t.Setenv("AWS_ENDPOINT_URL_DEVICE_FARM", closedEndpoint(t))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := NewRemoteAccessClient(ctx, nil)
	require.NoError(t, err)

	_, err = client.GetSession(ctx, testSessionARN)

	require.Error(t, err, "a call to a closed endpoint must surface as an error")
	assert.Contains(t, err.Error(), "GetRemoteAccessSession", "the error must come from the API call, not client construction")
}

func TestGivenUnusableARNWhenSessionFetchedThenFailsFastWithoutCallingTheAPI(t *testing.T) {
	// GetSession validates the ARN as its first step, so an ARN it cannot serve
	// fails before the API call. A counting getter proves the rejection happens
	// before the call rather than after it. A wrong-region ARN is rejected because
	// the client is bound to a single region (Device Farm remote access is
	// us-west-2-only today).
	tests := []struct {
		name string
		arn  string
	}{
		{name: "not an arn", arn: "not-an-arn"},
		{name: "wrong service", arn: "arn:aws:s3:us-west-2:1:bucket"},
		{name: "no region", arn: "arn:aws:devicefarm::1:session:a/b/0"},
		{name: "unsupported region", arn: "arn:aws:devicefarm:eu-central-1:1:session:a/b/0"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			getter := &fakeGetter{out: sessionOutput("wss://host.example/ws", "Pixel", "16")}

			_, err := newClient(getter).GetSession(context.Background(), tc.arn)

			require.Error(t, err)
			assert.Zero(t, getter.calls, "an unusable ARN must be rejected before the API call")
		})
	}
}

func TestGivenUnsupportedRegionARNWhenSessionFetchedThenErrorNamesTheSupportedRegion(t *testing.T) {
	// The user-facing error has to explain WHY a syntactically valid ARN was
	// refused, since the reason (single-region support) is not obvious.
	_, err := newClient(&fakeGetter{}).
		GetSession(context.Background(), "arn:aws:devicefarm:eu-central-1:1:session:a/b/0")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "eu-central-1", "the error must name the region that was rejected")
	assert.Contains(t, err.Error(), clientRegion, "the error must name the region that is supported")
}

func TestGivenAndroidDeviceBelowNineWhenSessionFetchedThenErrorNamesTheVersionAndTheFloor(t *testing.T) {
	// Only the response names the device, so the refusal runs after the call. Like
	// the region refusal, the text names the reported value and the supported one.
	for _, osVersion := range []string{"8", "8.0.0"} {
		t.Run(osVersion, func(t *testing.T) {
			fake := &fakeGetter{out: platformSessionOutput(types.DevicePlatformAndroid, osVersion)}

			_, err := newClient(fake).GetSession(context.Background(), testSessionARN)

			require.Error(t, err)
			assert.Equal(t,
				fmt.Sprintf("remote access session %q is on an Android %s device, but this client only supports Android 9 and above",
					testSessionARN, osVersion),
				err.Error(), "the error must name the reported version and the floor")
			assert.Equal(t, 1, fake.calls, "the device is known only from the response, so the call must be made")
		})
	}
}

func TestGivenAndroidDeviceNineOrAboveWhenSessionFetchedThenInfoReturned(t *testing.T) {
	for _, osVersion := range []string{"9", "10.3.2"} {
		t.Run(osVersion, func(t *testing.T) {
			fake := &fakeGetter{out: platformSessionOutput(types.DevicePlatformAndroid, osVersion)}

			got, err := newClient(fake).GetSession(context.Background(), testSessionARN)

			require.NoError(t, err)
			assert.Equal(t, "wss://host.example/ws", got.Endpoint)
			assert.Equal(t, "Lab device (OS "+osVersion+")", got.Label)
		})
	}
}

func TestGivenAndroidDeviceWithUnparsableOSWhenSessionFetchedThenSessionAllowedWithAWarning(t *testing.T) {
	// The warning is the only trace of the skipped check.
	tests := []struct {
		name string
		os   *string
	}{
		{name: "no os", os: nil},
		{name: "codename", os: aws.String("Q")},
		{name: "prefixed", os: aws.String("Android 9")},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out := platformSessionOutput(types.DevicePlatformAndroid, "")
			out.RemoteAccessSession.Device.Os = tc.os
			logs := &bytes.Buffer{}
			client := awsRemoteAccessClient{getter: &fakeGetter{out: out}, logger: slog.New(slog.NewTextHandler(logs, nil))}

			got, err := client.GetSession(context.Background(), testSessionARN)

			require.NoError(t, err)
			assert.Equal(t, "wss://host.example/ws", got.Endpoint)
			assert.Contains(t, got.Label, "Lab device (OS ", "the label still renders")
			assert.Contains(t, logs.String(), "device OS version not parsed", "the skipped check must leave a warning")
		})
	}
}

func TestGivenPreparingSessionOnAndroidBelowNineWhenSessionFetchedThenVersionRefusalComesFirst(t *testing.T) {
	// The device is known before the endpoint exists, so the user learns at once
	// that the device is unsupported instead of waiting for RUNNING and retrying.
	out := platformSessionOutput(types.DevicePlatformAndroid, "8.0.0")
	out.RemoteAccessSession.Endpoints = nil
	out.RemoteAccessSession.Status = types.ExecutionStatusPreparing

	_, err := newClient(&fakeGetter{out: out}).GetSession(context.Background(), testSessionARN)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "only supports Android 9 and above", "the version refusal must come first")
	assert.NotContains(t, err.Error(), "interactive endpoint", "the endpoint check must not run before the floor")
}

func TestGivenIOSDeviceWhenSessionFetchedThenVersionIsNotChecked(t *testing.T) {
	// An iOS version below the floor must not be read as Android.
	fake := &fakeGetter{out: platformSessionOutput(types.DevicePlatformIos, "8.4")}

	got, err := newClient(fake).GetSession(context.Background(), testSessionARN)

	require.NoError(t, err)
	assert.Equal(t, "Lab device (OS 8.4)", got.Label)
}

func TestGivenOSStringsWhenAndroidMajorParsedThenMajorAndOk(t *testing.T) {
	tests := []struct {
		os    string
		major int
		ok    bool
	}{
		{os: "9", major: 9, ok: true},
		{os: "8.0.0", major: 8, ok: true},
		{os: "10.3.2", major: 10, ok: true},
		{os: " 14 ", major: 14, ok: true},
		{os: "", ok: false},
		{os: ".9", ok: false},
		{os: "Q", ok: false},
		{os: "Android 9", ok: false},
		{os: "v9", ok: false},
	}

	for _, tc := range tests {
		t.Run(tc.os, func(t *testing.T) {
			major, ok := androidMajor(tc.os)

			assert.Equal(t, tc.ok, ok, "whether the text parses")
			assert.Equal(t, tc.major, major)
		})
	}
}
