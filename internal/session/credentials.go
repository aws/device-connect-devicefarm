// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go-v2/credentials/processcreds"
)

// CredentialSummary: the credential chain's source. Never the key.
type CredentialSummary struct {
	Source string
}

// errCredentialProcessOutput replaces every credential_process failure, cause included: the
// SDK's own text quotes the helper's whole output, keys included.
var errCredentialProcessOutput = errors.New("the credential_process helper failed or returned unusable output")

// scrubCredentialProcess swaps a credential_process failure for errCredentialProcessOutput.
// Every path that resolves credentials calls it before its error leaves the package.
func scrubCredentialProcess(err error) error {
	var processErr *processcreds.ProviderError
	if errors.As(err, &processErr) {
		return errCredentialProcessOutput
	}
	return err
}

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
