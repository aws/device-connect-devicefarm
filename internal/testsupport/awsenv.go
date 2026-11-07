// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package testsupport

import (
	"path/filepath"
	"testing"
)

// HermeticAWSEnv pins the AWS environment so a test that loads real AWS config resolves the
// same on any host: placeholder environment credentials, empty config files, legacy defaults
// mode, and every other chain input off (profiles, legacy key aliases, web identity, container
// credentials, the instance metadata lookup). A set profile would switch the SDK to a strict
// shared-config loader that fails when the profile is absent.
func HermeticAWSEnv(t *testing.T) {
	t.Helper()
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(t.TempDir(), "no-config"))
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(t.TempDir(), "no-credentials"))
	t.Setenv("AWS_DEFAULTS_MODE", "legacy")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	for _, name := range []string{"AWS_PROFILE", "AWS_DEFAULT_PROFILE", "AWS_SESSION_TOKEN", "AWS_ACCESS_KEY", "AWS_SECRET_KEY",
		"AWS_WEB_IDENTITY_TOKEN_FILE", "AWS_ROLE_ARN", "AWS_CONTAINER_CREDENTIALS_RELATIVE_URI",
		"AWS_CONTAINER_CREDENTIALS_FULL_URI", "AWS_CONTAINER_AUTHORIZATION_TOKEN", "AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE"} {
		t.Setenv(name, "")
	}
}
