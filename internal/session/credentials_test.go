package session

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/aws/device-connect-devicefarm/internal/testsupport"
)

// noEnvCredentials is the hermetic pin minus the environment keys, so the chain falls through
// to the config files.
func noEnvCredentials(t *testing.T) {
	t.Helper()
	testsupport.HermeticAWSEnv(t)
	t.Setenv("AWS_ACCESS_KEY_ID", "")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "")
}

func TestGivenEnvironmentCredentialsWhenResolvedThenTheSummaryNamesTheSourceAndNoExpiry(t *testing.T) {
	testsupport.HermeticAWSEnv(t)

	summary, err := ResolveCredentials(context.Background())

	require.NoError(t, err)
	assert.Equal(t, "EnvConfigCredentials", summary.Source)
	assert.False(t, summary.CanExpire)
}

func TestGivenAnEmptyCredentialChainWhenResolvedThenItFails(t *testing.T) {
	noEnvCredentials(t)

	_, err := ResolveCredentials(context.Background())

	assert.Error(t, err)
}

func TestGivenACredentialProcessPrintingAKeyWhenItsOutputIsUnusableThenTheErrorHidesTheOutput(t *testing.T) {
	// The helper prints a truncated JSON object with a key-shaped marker; the SDK's own error
	// quotes that output whole.
	noEnvCredentials(t)
	const keyMarker = "AKIA-KEY-MARKER-0123456789"
	configFile := filepath.Join(t.TempDir(), "config")
	require.NoError(t, os.WriteFile(configFile, []byte("[default]\ncredential_process = echo '{\"Version\":1,\"AccessKeyId\":\""+keyMarker+"\"'\n"), 0o600))
	t.Setenv("AWS_CONFIG_FILE", configFile)

	_, err := ResolveCredentials(context.Background())

	require.Error(t, err)
	assert.ErrorIs(t, err, errCredentialProcessOutput)
	assert.NotContains(t, err.Error(), keyMarker, "the helper's output must not reach the caller")
}

func TestGivenAMissingProfileWhenResolvedThenTheConfigLoadFails(t *testing.T) {
	// A named profile switches the SDK to the strict shared-config loader, which refuses a
	// profile the pinned config files lack.
	testsupport.HermeticAWSEnv(t)
	t.Setenv("AWS_PROFILE", "no-such-profile")

	_, resolveErr := ResolveCredentials(context.Background())
	_, clientErr := NewRemoteAccessClient(context.Background(), nil)

	assert.ErrorContains(t, resolveErr, "load AWS config")
	assert.ErrorContains(t, clientErr, "load AWS config")
}

func TestGivenACredentialProcessPrintingAKeyWhenASessionIsResolvedThenTheErrorHidesTheOutput(t *testing.T) {
	// The Device Farm client resolves credentials on its first call, so the same helper output
	// would reach GetSession's error.
	noEnvCredentials(t)
	const keyMarker = "AKIA-KEY-MARKER-9876543210"
	configFile := filepath.Join(t.TempDir(), "config")
	require.NoError(t, os.WriteFile(configFile, []byte("[default]\ncredential_process = echo '{\"Version\":1,\"AccessKeyId\":\""+keyMarker+"\"'\n"), 0o600))
	t.Setenv("AWS_CONFIG_FILE", configFile)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := NewRemoteAccessClient(ctx, nil)
	require.NoError(t, err)

	_, err = client.GetSession(ctx, testSessionARN)

	require.Error(t, err)
	assert.ErrorIs(t, err, errCredentialProcessOutput)
	assert.NotContains(t, err.Error(), keyMarker, "the helper's output must not reach the caller")
}
