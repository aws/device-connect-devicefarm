// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package version

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGivenEmbeddedVersionFileWhenReadThenIsATrimmedSemanticVersionWithLeadingV(t *testing.T) {
	// Release tooling derives file names and the tag from this shape.
	got := String()

	assert.Regexp(t, `^v[0-9]+\.[0-9]+\.[0-9]+$`, got)
	onDisk, err := os.ReadFile("VERSION")
	require.NoError(t, err)
	assert.Equal(t, got+"\n", string(onDisk), "VERSION is one line plus LF")
}
