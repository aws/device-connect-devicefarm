package session

import (
	"context"
	"errors"
	"time"

	"github.com/aws/aws-sdk-go-v2/credentials/processcreds"
)

// CredentialSummary: where the chain resolved from and when it lapses. Never the key.
type CredentialSummary struct {
	Source    string
	CanExpire bool
	Expires   time.Time
}

// errCredentialProcessOutput replaces every credential_process failure, cause included: the
// SDK's own text quotes the helper's whole output, keys included.
var errCredentialProcessOutput = errors.New("the credential_process helper failed or returned unusable output")

// ResolveCredentials walks the default credential chain once, as the Device Farm client does
// on its first call. An empty chain is an error.
func ResolveCredentials(ctx context.Context) (CredentialSummary, error) {
	cfg, err := loadConfig(ctx)
	if err != nil {
		return CredentialSummary{}, err
	}
	credentials, err := cfg.Credentials.Retrieve(ctx)
	if err != nil {
		var processErr *processcreds.ProviderError
		if errors.As(err, &processErr) {
			err = errCredentialProcessOutput
		}
		return CredentialSummary{}, err
	}
	return CredentialSummary{Source: credentials.Source, CanExpire: credentials.CanExpire, Expires: credentials.Expires}, nil
}
