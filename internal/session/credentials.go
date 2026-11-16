// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials/processcreds"

	"github.com/aws/device-connect-devicefarm/internal/wording"
)

// CredentialSummary: the credential chain's source. Never the key.
type CredentialSummary struct {
	Source string
}

// errCredentialProcessOutput replaces every credential_process failure, cause included: the
// SDK's own text quotes the helper's whole output, keys included.
var errCredentialProcessOutput = errors.New(wording.SessionCredentialProcessFailed)

// scrubCredentialProcess swaps a credential_process failure for errCredentialProcessOutput.
// Every path that resolves credentials calls it before its error leaves the package.
func scrubCredentialProcess(err error) error {
	var processErr *processcreds.ProviderError
	if errors.As(err, &processErr) {
		return credentialsError{cause: errCredentialProcessOutput}
	}
	return err
}

// markedCredentials is loadConfig's credential provider: the SDK's, with a retrieval failure
// marked ErrNoCredentials unless the caller's context ended. The SDK gives that failure no type
// of its own.
type markedCredentials struct{ provider aws.CredentialsProvider }

func (marked markedCredentials) Retrieve(ctx context.Context) (aws.Credentials, error) {
	credentials, err := marked.provider.Retrieve(ctx)
	if err != nil && ctx.Err() == nil {
		return aws.Credentials{}, credentialsError{cause: err}
	}
	return credentials, err
}

// ProviderSources passes the SDK provider's sources through, for the client's user agent.
func (marked markedCredentials) ProviderSources() []aws.CredentialSource {
	if sources, ok := marked.provider.(aws.CredentialProviderSource); ok {
		return sources.ProviderSources()
	}
	return nil
}

// credentialsError matches ErrNoCredentials and keeps its cause's text.
type credentialsError struct{ cause error }

func (credentialsErr credentialsError) Error() string { return credentialsErr.cause.Error() }

func (credentialsErr credentialsError) Unwrap() error { return credentialsErr.cause }

func (credentialsError) Is(target error) bool { return target == ErrNoCredentials }

// ResolveCredentials walks the default credential chain once, as the Device Farm client does
// on its first call. An empty chain is an error.
func ResolveCredentials(ctx context.Context) (CredentialSummary, error) {
	cfg, err := loadConfig(ctx)
	if err != nil {
		return CredentialSummary{}, err
	}
	credentials, err := cfg.Credentials.Retrieve(ctx)
	if err != nil {
		return CredentialSummary{}, scrubCredentialProcess(err)
	}
	return CredentialSummary{Source: credentials.Source}, nil
}
