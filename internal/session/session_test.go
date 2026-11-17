// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/service/devicefarm"
	"github.com/aws/aws-sdk-go-v2/service/devicefarm/types"
	"github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/aws/device-connect-devicefarm/internal/testsupport"
)

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

// withoutRetryWaits swaps in an adaptive retryer with no waits: zero backoff and a free
// attempt token, so adaptive mode never sleeps after a throttle. The client's wrap still
// applies the production attempt budget, so tests count production attempts.
func withoutRetryWaits() func(*devicefarm.Options) {
	return func(options *devicefarm.Options) {
		options.Retryer = retry.NewAdaptiveMode(func(adaptive *retry.AdaptiveModeOptions) {
			adaptive.RequestCost = 0
			adaptive.StandardOptions = append(adaptive.StandardOptions, func(standard *retry.StandardOptions) {
				standard.Backoff = retry.BackoffDelayerFunc(func(int, error) (time.Duration, error) { return 0, nil })
			})
		})
	}
}

// throttlingAPI is a fake remote access API on a local port. It answers ThrottlingException
// (HTTP 400) to the first throttles requests, then a RUNNING session, and counts the requests.
type throttlingAPI struct {
	throttles int
	requests  atomic.Int32
}

func (api *throttlingAPI) ServeHTTP(response http.ResponseWriter, _ *http.Request) {
	served := int(api.requests.Add(1))
	response.Header().Set("Content-Type", "application/x-amz-json-1.1")
	if served <= api.throttles {
		response.WriteHeader(http.StatusBadRequest)
		_, _ = response.Write([]byte(`{"__type":"ThrottlingException","message":"Rate exceeded"}`))
		return
	}
	_, _ = response.Write([]byte(`{"remoteAccessSession":{"status":"RUNNING",` +
		`"endpoints":{"interactiveEndpoint":"wss://host.example/ws"},` +
		`"device":{"name":"Pixel","os":"16","platform":"ANDROID"}}}`))
}

// newThrottlingClient builds the production client against a throttlingAPI through the pinned
// endpoint env, without the retryer's waits.
func newThrottlingClient(t *testing.T, throttles int) (RemoteAccessClient, *throttlingAPI) {
	t.Helper()
	testsupport.HermeticAWSEnv(t)
	api := &throttlingAPI{throttles: throttles}
	server := httptest.NewServer(api)
	t.Cleanup(server.Close)
	t.Setenv("AWS_ENDPOINT_URL_DEVICE_FARM", server.URL)
	client, err := newRemoteAccessClient(context.Background(), nil, withoutRetryWaits())
	require.NoError(t, err)
	return client, api
}

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

// sessionOutput builds a session on an Android device with its interactive
// endpoint set.
func sessionOutput(endpoint, name, os string) *devicefarm.GetRemoteAccessSessionOutput {
	return &devicefarm.GetRemoteAccessSessionOutput{
		RemoteAccessSession: &types.RemoteAccessSession{
			Status:    types.ExecutionStatusRunning,
			Endpoints: &types.RemoteAccessEndpoints{InteractiveEndpoint: aws.String(endpoint)},
			Device: &types.Device{
				Name:     aws.String(name),
				Os:       aws.String(os),
				Platform: types.DevicePlatformAndroid,
			},
		},
	}
}

// platformSessionOutput is sessionOutput with the device platform replaced.
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

// apiFailure is the error chain the SDK hands back for a modeled or generic API error.
func apiFailure(apiErr error) error {
	return apiFailureWithStatus(http.StatusBadRequest, apiErr)
}

// apiFailureWithStatus is apiFailure with the response's HTTP status.
func apiFailureWithStatus(status int, apiErr error) error {
	return &smithy.OperationError{ServiceID: "Device Farm", OperationName: "GetRemoteAccessSession",
		Err: &awshttp.ResponseError{ResponseError: &smithyhttp.ResponseError{
			Response: &smithyhttp.Response{Response: &http.Response{StatusCode: status}}, Err: apiErr}}}
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

func TestGivenASessionThatIsNotRunningWhenResolvedThenTheErrorCarriesItsClass(t *testing.T) {
	withoutEndpoint := func(status types.ExecutionStatus) *types.RemoteAccessSession {
		return &types.RemoteAccessSession{Status: status}
	}
	tests := []struct {
		name     string
		session  *types.RemoteAccessSession
		want     error
		wantText string
	}{
		{name: "nil session", want: ErrSessionNotFound, wantText: `remote access session "` + testSessionARN + `" not found`},
		{name: "completed", want: ErrSessionEnded, wantText: "has ended (status COMPLETED)",
			session: &types.RemoteAccessSession{Status: types.ExecutionStatusCompleted,
				Endpoints: &types.RemoteAccessEndpoints{InteractiveEndpoint: aws.String("wss://host.example/ws")}}},
		{name: "stopping", session: withoutEndpoint(types.ExecutionStatusStopping), want: ErrSessionEnded, wantText: "has ended"},
		{name: "completed on an Android device below the floor", want: ErrSessionEnded, wantText: "has ended (status COMPLETED)",
			session: &types.RemoteAccessSession{Status: types.ExecutionStatusCompleted,
				Device: &types.Device{Platform: types.DevicePlatformAndroid, Os: aws.String("8.0.0")}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out := &devicefarm.GetRemoteAccessSessionOutput{RemoteAccessSession: tc.session}

			_, err := newClient(&fakeGetter{out: out}).GetSession(context.Background(), testSessionARN)

			assert.ErrorIs(t, err, tc.want)
			assert.NotErrorIs(t, err, ErrUnsupportedDevice, "an ended session is ended whatever its device")
			assert.ErrorContains(t, err, tc.wantText)
		})
	}
}

func TestGivenASessionWithNoEndpointWhenResolvedThenTheErrorForksOnWhetherItIsRunning(t *testing.T) {
	const running = types.ExecutionStatusRunning
	sessions := []types.RemoteAccessSession{{Status: running,
		Endpoints: &types.RemoteAccessEndpoints{InteractiveEndpoint: aws.String("")}}}
	// Every status the service knows, with no endpoints. An ended session is another class.
	for _, status := range running.Values() {
		if status != types.ExecutionStatusCompleted && status != types.ExecutionStatusStopping {
			sessions = append(sessions, types.RemoteAccessSession{Status: status})
		}
	}
	require.Greater(t, len(sessions), 3, "the SDK lists statuses before RUNNING")
	for _, remoteAccessSession := range sessions {
		status := remoteAccessSession.Status
		t.Run(string(status), func(t *testing.T) {
			wantText := `remote access session "` + testSessionARN + `" is not running yet (status ` + string(status) + `)`
			if status == running {
				wantText = `remote access session "` + testSessionARN + `" is running, but Device Farm returned no interactive endpoint for it`
			}
			out := &devicefarm.GetRemoteAccessSessionOutput{RemoteAccessSession: &remoteAccessSession}

			_, err := newClient(&fakeGetter{out: out}).GetSession(context.Background(), testSessionARN)

			require.EqualError(t, err, wantText)
			assert.ErrorIs(t, err, ErrNoEndpointYet, "the add's fix and the detach reasons match the sentinel")
			var noEndpoint *NoEndpointError
			require.ErrorAs(t, err, &noEndpoint)
			assert.Equal(t, status == running, noEndpoint.Running())
		})
	}
}

func TestGivenAnEndedSessionWhenResolvedThenTheErrorNamesTheStatusWord(t *testing.T) {
	for _, status := range []types.ExecutionStatus{types.ExecutionStatusCompleted, types.ExecutionStatusStopping} {
		t.Run(string(status), func(t *testing.T) {
			out := &devicefarm.GetRemoteAccessSessionOutput{RemoteAccessSession: &types.RemoteAccessSession{Status: status}}

			_, err := newClient(&fakeGetter{out: out}).GetSession(context.Background(), testSessionARN)

			var ended *SessionEndedError
			require.ErrorAs(t, err, &ended)
			assert.Equal(t, string(status), ended.Status)
			assert.Equal(t, testSessionARN, ended.SessionARN)
		})
	}
}

func TestGivenAPIErrorWhenResolvedThenErrorPropagated(t *testing.T) {
	fake := &fakeGetter{err: errors.New("access denied")}

	_, err := newClient(fake).GetSession(context.Background(), testSessionARN)

	require.Error(t, err)
}

func TestGivenAClassifiedAPIErrorWhenResolvedThenTheErrorCarriesItsSentinel(t *testing.T) {
	tests := []struct {
		name     string
		apiErr   error
		sentinel error
	}{
		{name: "access denied", apiErr: &smithy.GenericAPIError{Code: "AccessDeniedException", Message: "refused"}, sentinel: ErrAccessDenied},
		{name: "not found", apiErr: &types.NotFoundException{Message: aws.String("refused")}, sentinel: ErrSessionNotFound},
		{name: "argument", apiErr: &types.ArgumentException{Message: aws.String("refused")}, sentinel: ErrInvalidARN},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := newClient(&fakeGetter{err: apiFailure(tc.apiErr)}).GetSession(context.Background(), testSessionARN)

			assert.ErrorIs(t, err, tc.sentinel)
			assert.ErrorContains(t, err, "refused", "the SDK text stays inside the wrap")
		})
	}
	throttled := apiFailure(&smithy.GenericAPIError{Code: "ThrottlingException", Message: "slow down"})
	_, err := newClient(&fakeGetter{err: throttled}).GetSession(context.Background(), testSessionARN)
	for _, sentinel := range []error{ErrAccessDenied, ErrSessionNotFound, ErrInvalidARN, ErrRequestRejected} {
		assert.NotErrorIs(t, err, sentinel)
	}
}

func TestGivenAFourHundredThatIsNoThrottleWhenResolvedThenTheErrorIsARejectedRequestWithItsCode(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		code    string
		expired bool
	}{
		{name: "expired token", status: http.StatusForbidden, code: "ExpiredTokenException", expired: true},
		{name: "expired token, the older code", status: http.StatusForbidden, code: "ExpiredToken", expired: true},
		{name: "unknown key", status: http.StatusBadRequest, code: "UnrecognizedClientException"},
		{name: "bad signature", status: http.StatusForbidden, code: "InvalidSignatureException"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			failure := apiFailureWithStatus(tc.status, &smithy.GenericAPIError{Code: tc.code, Message: "refused"})

			_, err := newClient(&fakeGetter{err: failure}).GetSession(context.Background(), testSessionARN)

			var rejected *RequestRejectedError
			require.ErrorAs(t, err, &rejected)
			assert.ErrorIs(t, err, ErrRequestRejected)
			assert.Equal(t, tc.code, rejected.Code)
			assert.Equal(t, tc.expired, rejected.CredentialsExpired())
			assert.Equal(t, failure, errors.Unwrap(rejected), "the SDK's chain stays reachable behind the class")
			assert.ErrorContains(t, err, "request rejected ("+tc.code+"): ", "the code reads in place")
			assert.ErrorContains(t, err, "refused", "the SDK text stays inside the wrap")
		})
	}
}

func TestGivenAThrottleAServerFaultOrATransportErrorWhenResolvedThenNoRejectionIsClassified(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{name: "throttled 400", err: apiFailure(&smithy.GenericAPIError{Code: "ThrottlingException", Message: "slow down"})},
		{name: "429 with an unknown code", err: apiFailureWithStatus(http.StatusTooManyRequests,
			&smithy.GenericAPIError{Code: "SomethingNew", Message: "slow down"})},
		{name: "429 with no error body", err: apiFailureWithStatus(http.StatusTooManyRequests, errors.New("too many requests"))},
		{name: "server fault 503", err: apiFailureWithStatus(http.StatusServiceUnavailable,
			&smithy.GenericAPIError{Code: "InternalFailure", Message: "down"})},
		{name: "no response", err: &smithy.OperationError{ServiceID: "Device Farm", OperationName: "GetRemoteAccessSession",
			Err: errors.New("dial tcp 127.0.0.1:1: connect: connection refused")}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := newClient(&fakeGetter{err: tc.err}).GetSession(context.Background(), testSessionARN)

			require.Error(t, err)
			assert.NotErrorIs(t, err, ErrRequestRejected, "a retry may well succeed")
		})
	}
}

func TestGivenSessionMissingDeviceFieldsWhenResolvedThenLabelDegradesGracefully(t *testing.T) {
	// No device means no platform or version to check, so the device gate passes.
	out := &devicefarm.GetRemoteAccessSessionOutput{
		RemoteAccessSession: &types.RemoteAccessSession{
			Status:    types.ExecutionStatusRunning,
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
	testsupport.HermeticAWSEnv(t)

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
	// AWS. The retryer waits for nothing, and a short context bounds the SDK's retries
	// so the test cannot hang.
	testsupport.HermeticAWSEnv(t)
	t.Setenv("AWS_ENDPOINT_URL_DEVICE_FARM", closedEndpoint(t))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := newRemoteAccessClient(ctx, nil, withoutRetryWaits())
	require.NoError(t, err)

	_, err = client.GetSession(ctx, testSessionARN)

	require.Error(t, err, "a call to a closed endpoint must surface as an error")
	assert.Contains(t, err.Error(), "GetRemoteAccessSession", "the error must come from the API call, not client construction")
	assert.NotErrorIs(t, err, ErrNoCredentials, "a connection failure is not a credentials failure")
}

func TestGivenAResponseWithAMalformedDateHeaderWhenSessionFetchedThenTheSDKWritesNothingToStderr(t *testing.T) {
	// The SDK logger keeps the os.Stderr it saw when the config loaded, so the test
	// swaps stderr first.
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		writer.Header().Set("Date", "not-a-date")
		writer.Header().Set("Content-Type", "application/x-amz-json-1.1")
		_, _ = writer.Write([]byte("{}"))
	}))
	t.Cleanup(server.Close)
	testsupport.HermeticAWSEnv(t)
	t.Setenv("AWS_ENDPOINT_URL_DEVICE_FARM", server.URL)
	stderrFile, err := os.CreateTemp(t.TempDir(), "stderr")
	require.NoError(t, err)
	realStderr := os.Stderr
	os.Stderr = stderrFile
	t.Cleanup(func() { os.Stderr = realStderr })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	client, err := NewRemoteAccessClient(ctx, slog.New(slog.DiscardHandler))
	require.NoError(t, err)
	_, _ = client.GetSession(ctx, testSessionARN)

	require.Positive(t, requests.Load(), "the call must reach the endpoint")
	written, err := os.ReadFile(stderrFile.Name())
	require.NoError(t, err)
	assert.Empty(t, string(written))
}

func TestGivenALookupThrottledThreeTimesWhenItSucceedsOnTheFourthAttemptThenTheSessionResolves(t *testing.T) {
	// The context fails the row loudly should the retryer's waits ever come back.
	client, api := newThrottlingClient(t, 3)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	got, err := client.GetSession(ctx, testSessionARN)

	require.NoError(t, err, "the fourth attempt answers RUNNING")
	assert.Equal(t, int32(4), api.requests.Load(), "one request per attempt, the whole budget")
	assert.Equal(t, "wss://host.example/ws", got.Endpoint)
	assert.Equal(t, "Pixel (OS 16)", got.Label)
}

func TestGivenALookupThrottledOnEveryAttemptWhenTheBudgetEndsThenTheErrorIsTheThrottle(t *testing.T) {
	client, api := newThrottlingClient(t, math.MaxInt)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := client.GetSession(ctx, testSessionARN)

	require.Error(t, err)
	assert.Equal(t, int32(4), api.requests.Load(), "the budget is spent whole, then the lookup fails")
	var exhausted *retry.MaxAttemptsError
	require.ErrorAs(t, err, &exhausted)
	assert.Equal(t, 4, exhausted.Attempt)
	var apiErr smithy.APIError
	require.ErrorAs(t, err, &apiErr)
	assert.Equal(t, "ThrottlingException", apiErr.ErrorCode(), "the throttle code reaches the caller")
	for _, sentinel := range []error{ErrAccessDenied, ErrSessionNotFound, ErrInvalidARN, ErrRequestRejected,
		ErrSessionEnded, ErrNoEndpointYet, ErrUnsupportedDevice} {
		assert.NotErrorIs(t, err, sentinel, "a throttle carries no sentinel")
	}
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
		{name: "a project arn", arn: "arn:aws:devicefarm:us-west-2:1:project:a"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			getter := &fakeGetter{out: sessionOutput("wss://host.example/ws", "Pixel", "16")}

			_, err := newClient(getter).GetSession(context.Background(), tc.arn)

			assert.ErrorIs(t, err, ErrInvalidARN)
			assert.Zero(t, getter.calls, "an unusable ARN must be rejected before the API call")
		})
	}
}

func TestGivenArnShapesWhenValidatedThenOnlyARemoteAccessSessionArnPasses(t *testing.T) {
	require.NoError(t, ValidateSessionARN(testSessionARN), "a session ARN passes")
	tests := []struct {
		name     string
		arn      string
		wantText string
	}{
		{name: "a project arn", arn: "arn:aws:devicefarm:us-west-2:1:project:a",
			wantText: "is not a remote access session ARN"},
		{name: "a device arn", arn: "arn:aws:devicefarm:us-west-2:1:device:a",
			wantText: "is not a remote access session ARN"},
		{name: "session with nothing after it", arn: "arn:aws:devicefarm:us-west-2:1:session:",
			wantText: "is not a remote access session ARN"},
		{name: "an empty partition", arn: "arn::devicefarm:us-west-2:1:session:a/b/0",
			wantText: `is not an AWS ARN (partition "")`},
		{name: "another service", arn: "arn:aws:s3:us-west-2:1:bucket",
			wantText: "is not a Device Farm ARN"},
		{name: "another region", arn: "arn:aws:devicefarm:eu-central-1:1:session:a/b/0",
			wantText: "only supported in us-west-2"},
		{name: "not an arn", arn: "not-an-arn-at-all", wantText: "invalid prefix"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateSessionARN(tc.arn)

			require.ErrorIs(t, err, ErrInvalidARN)
			assert.Contains(t, err.Error(), tc.wantText)
		})
	}
}

func TestGivenAResponseWithAnArnWhenResolvedThenTheInfoCarriesIt(t *testing.T) {
	out := sessionOutput("wss://host.example/ws", "Pixel", "16")
	out.RemoteAccessSession.Arn = aws.String(testSessionARN)

	got, err := newClient(&fakeGetter{out: out}).GetSession(context.Background(), testSessionARN)

	require.NoError(t, err)
	assert.Equal(t, testSessionARN, got.ARN)
}

func TestGivenAResponseWithoutAnArnWhenResolvedThenTheInfoArnIsEmpty(t *testing.T) {
	got, err := newClient(&fakeGetter{out: sessionOutput("wss://host.example/ws", "Pixel", "16")}).
		GetSession(context.Background(), testSessionARN)

	require.NoError(t, err)
	assert.Empty(t, got.ARN)
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
			assert.ErrorIs(t, err, ErrUnsupportedDevice, "the refusal must carry its class")
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

func TestGivenPreparingSessionOnUnsupportedDeviceWhenSessionFetchedThenRefusalComesFirst(t *testing.T) {
	// The device is known before the endpoint exists, so the user learns at once
	// that the device is unsupported instead of waiting for RUNNING and retrying.
	tests := []struct {
		name         string
		platform     types.DevicePlatform
		os           string
		wantFragment string
	}{
		{name: "Android below the floor", platform: types.DevicePlatformAndroid, os: "8.0.0", wantFragment: "only supports Android 9 and above"},
		{name: "iOS", platform: types.DevicePlatformIos, os: "17.0", wantFragment: "is on an iOS device"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out := platformSessionOutput(tc.platform, tc.os)
			out.RemoteAccessSession.Endpoints = nil
			out.RemoteAccessSession.Status = types.ExecutionStatusPreparing

			_, err := newClient(&fakeGetter{out: out}).GetSession(context.Background(), testSessionARN)

			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantFragment, "the device refusal must come first")
			assert.NotContains(t, err.Error(), "interactive endpoint", "the endpoint check must not run before the gate")
		})
	}
}

func TestGivenNonAndroidDeviceWhenSessionFetchedThenErrorNamesThePlatformAndAndroidOnly(t *testing.T) {
	// IOS is spelled "iOS"; an unknown platform is spelled raw. The version plays
	// no part: iOS 8.4 is refused as iOS, not as Android below the floor.
	tests := []struct {
		platform types.DevicePlatform
		wantName string
	}{
		{platform: types.DevicePlatformIos, wantName: "iOS"},
		{platform: types.DevicePlatform("WATCH"), wantName: "WATCH"},
	}

	for _, tc := range tests {
		t.Run(tc.wantName, func(t *testing.T) {
			fake := &fakeGetter{out: platformSessionOutput(tc.platform, "8.4")}

			_, err := newClient(fake).GetSession(context.Background(), testSessionARN)

			require.Error(t, err)
			assert.Equal(t,
				fmt.Sprintf("remote access session %q is on an %s device, but this client only supports Android devices",
					testSessionARN, tc.wantName),
				err.Error(), "the error must name the platform and that only Android is served")
			assert.ErrorIs(t, err, ErrUnsupportedDevice, "the refusal must carry its class")
			assert.Equal(t, 1, fake.calls, "the device is known only from the response, so the call must be made")
		})
	}
}

func TestGivenUnsupportedDeviceWhenTheRefusalIsWrappedThenErrorsAsYieldsThePlatformAndOS(t *testing.T) {
	// The daemon wraps the resolver's error as below. An in-process caller reaches
	// the refusal with errors.As and tells an iOS device from an old Android
	// release without reading the text.
	tests := []struct {
		name           string
		platform       types.DevicePlatform
		os             string
		wantPlatform   string
		wantBelowFloor bool
	}{
		{name: "iOS", platform: types.DevicePlatformIos, os: "17.0", wantPlatform: "iOS", wantBelowFloor: false},
		{name: "Android below the floor", platform: types.DevicePlatformAndroid, os: "8.0.0", wantPlatform: "Android", wantBelowFloor: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeGetter{out: platformSessionOutput(tc.platform, tc.os)}

			_, err := newClient(fake).GetSession(context.Background(), testSessionARN)
			wrapped := fmt.Errorf("resolve session: %w", err)

			var refusal UnsupportedDeviceError
			require.ErrorAs(t, wrapped, &refusal)
			assert.Equal(t,
				UnsupportedDeviceError{SessionARN: testSessionARN, Platform: tc.wantPlatform, OSVersion: tc.os},
				refusal)
			assert.Equal(t, tc.wantBelowFloor, refusal.BelowAndroidFloor())
		})
	}
}

func TestGivenDeviceWithoutPlatformWhenSessionFetchedThenSessionAllowed(t *testing.T) {
	// No platform in the response, so the gate fails open, as for a missing device.
	fake := &fakeGetter{out: platformSessionOutput("", "16")}

	got, err := newClient(fake).GetSession(context.Background(), testSessionARN)

	require.NoError(t, err)
	assert.Equal(t, "Lab device (OS 16)", got.Label)
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
