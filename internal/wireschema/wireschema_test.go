// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package wireschema

import (
	"encoding/hex"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// frameMagicLen is the magic length; the schema judges the body after it.
const frameMagicLen = 7

// armKeys are the arms the contract names; the fixture must exercise each.
var armKeys = []string{
	"newSession", "reattach", "error", "notifierHello", "notifierReady",
	"reverseOpen", "ping", "pong", "connectorHello",
}

func loadFixture(t *testing.T) Fixture {
	t.Helper()
	fixture, err := LoadFixture()
	require.NoError(t, err)
	return fixture
}

func valueCase(t *testing.T, fixture Fixture, id string) ValueCase {
	t.Helper()
	for _, testCase := range fixture.ControlFrame {
		if testCase.ID == id {
			return testCase
		}
	}
	t.Fatalf("the fixture has no controlFrame case %q", id)
	return ValueCase{}
}

func assertVerdict(t *testing.T, valid bool, err error, why string) {
	t.Helper()
	if valid {
		assert.NoError(t, err, "the fixture says valid: %s", why)
	} else {
		assert.Error(t, err, "the fixture says invalid: %s", why)
	}
}

func TestGivenEveryFixtureValueCaseWhenValidatedThenTheVerdictMatches(t *testing.T) {
	fixture := loadFixture(t)

	for _, testCase := range fixture.ControlFrame {
		t.Run(testCase.ID, func(t *testing.T) {
			assertVerdict(t, testCase.Valid, Validate(testCase.JSON), testCase.Why)
		})
	}
}

func TestGivenEveryFixtureFrameTheSchemaJudgesWhenItsBodyIsValidatedThenTheVerdictMatches(t *testing.T) {
	fixture := loadFixture(t)

	judged := 0
	for _, vector := range fixture.Frames {
		if !vector.Valid && !strings.HasPrefix(vector.Why, "law 3") {
			continue
		}
		judged++
		t.Run(vector.ID, func(t *testing.T) {
			message, err := hex.DecodeString(vector.Hex)
			require.NoError(t, err)
			require.Greater(t, len(message), frameMagicLen)

			assertVerdict(t, vector.Valid, Validate(message[frameMagicLen:]), vector.Why)
		})
	}
	assert.Greater(t, judged, 0, "the fixture must carry frames the schema judges")
}

func TestGivenTheStagedFixtureWhenLoadedThenEachSectionHasBothVerdictsAndTheFramesNameAllNineArms(t *testing.T) {
	fixture := loadFixture(t)

	frameVerdicts := map[bool]int{}
	arms := map[string]bool{}
	for _, vector := range fixture.Frames {
		frameVerdicts[vector.Valid]++
		if vector.Valid {
			arms[vector.Arm] = true
		}
	}
	caseVerdicts := map[bool]int{}
	for _, testCase := range fixture.ControlFrame {
		caseVerdicts[testCase.Valid]++
	}

	assert.Positive(t, frameVerdicts[true], "frames must hold a valid vector")
	assert.Positive(t, frameVerdicts[false], "frames must hold an invalid vector")
	assert.Positive(t, caseVerdicts[true], "controlFrame must hold a valid case")
	assert.Positive(t, caseVerdicts[false], "controlFrame must hold an invalid case")
	for _, arm := range armKeys {
		assert.True(t, arms[arm], "a valid frame must carry the %s arm", arm)
	}
	assert.Len(t, arms, len(armKeys), "valid frames name only the contract's arms")
}

func TestGivenAnInvalidTokenWhenValidatedThenTheErrorNamesTheLocationButNotTheToken(t *testing.T) {
	testCase := valueCase(t, loadFixture(t), "connector-hello-non-uuid-token")

	err := Validate(testCase.JSON)

	require.Error(t, err)
	assert.Equal(t, "control frame does not match the schema at /connectorHello/token (minLength, pattern)", err.Error())
}

func TestGivenAFrameWithNoArmWhenValidatedThenTheErrorNamesTheRoot(t *testing.T) {
	testCase := valueCase(t, loadFixture(t), "no-arm")

	err := Validate(testCase.JSON)

	require.Error(t, err)
	assert.Equal(t, "control frame does not match the schema at / (minProperties)", err.Error())
}

func TestGivenFailuresAtTwoDepthsWhenValidatedThenTheErrorNamesTheDeepest(t *testing.T) {
	// A second arm fails maxProperties at the root; the port fails minimum two levels down.
	body := []byte(`{"reverseOpen":{"token":"f47ac10b-58cc-4372-a567-0e02b2c3d479","customerPort":0},"pong":{}}`)

	err := Validate(body)

	require.Error(t, err)
	assert.Equal(t, "control frame does not match the schema at /reverseOpen/customerPort (minimum)", err.Error())
}

func TestGivenFailuresAtOneDepthInTwoMembersWhenValidatedThenTheErrorNamesTheSmallerPointer(t *testing.T) {
	// The token fails minLength and pattern, the port fails minimum, both two levels down.
	body := []byte(`{"reverseOpen":{"token":"x","customerPort":0}}`)

	err := Validate(body)

	require.Error(t, err)
	assert.Equal(t, "control frame does not match the schema at /reverseOpen/customerPort (minimum)", err.Error())
}

// The race detector is the assertion here; without it this is the verdict test again.
func TestGivenManyGoroutinesWhenTheyValidateConcurrentlyThenEveryVerdictMatches(t *testing.T) {
	fixture := loadFixture(t)
	const goroutines = 10

	var waitGroup sync.WaitGroup
	mismatches := make(chan string, goroutines*len(fixture.ControlFrame))
	for range goroutines {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			for _, testCase := range fixture.ControlFrame {
				if (Validate(testCase.JSON) == nil) != testCase.Valid {
					mismatches <- testCase.ID
				}
			}
		}()
	}
	waitGroup.Wait()
	close(mismatches)

	for id := range mismatches {
		t.Errorf("verdict mismatch under concurrency: %s", id)
	}
}

func TestGivenADeletedWorkingDirectoryWhenTheSchemaCompilesThenItSucceeds(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the working directory cannot be removed while in use on windows")
	}
	gone := t.TempDir()
	t.Chdir(gone)
	require.NoError(t, os.RemoveAll(gone))

	schema, err := compileControlFrame(controlSchemaDocument)

	require.NoError(t, err)
	assert.NotNil(t, schema)
}

func TestGivenABrokenSchemaWhenCompiledThenItFails(t *testing.T) {
	tests := []struct {
		name     string
		document string
	}{
		{name: "not JSON", document: `{"$ref": `},
		{name: "no controlFrame definition", document: `{"$ref": "#/$defs/controlFrame", "$defs": {}}`},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			schema, err := compileControlFrame([]byte(testCase.document))

			assert.Error(t, err)
			assert.Nil(t, schema)
		})
	}
}

func TestGivenADocumentThatIsNotJSONWhenValidatedThenItFails(t *testing.T) {
	err := Validate([]byte(`{"ping": `))

	require.Error(t, err)
	assert.Equal(t, "control frame body is not JSON", err.Error())
}
