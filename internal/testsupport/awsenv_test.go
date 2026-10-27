package testsupport

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGivenAHostWithAProfileWhenTheEnvIsPinnedThenTheSDKResolvesThePlaceholderKeys(t *testing.T) {
	// A profile the pinned files lack would fail the load.
	t.Setenv("AWS_PROFILE", "team-admin")
	HermeticAWSEnv(t)

	cfg, err := config.LoadDefaultConfig(context.Background())
	require.NoError(t, err)
	credentials, err := cfg.Credentials.Retrieve(context.Background())

	require.NoError(t, err)
	assert.Equal(t, "EnvConfigCredentials", credentials.Source)
	assert.False(t, credentials.CanExpire)
}
