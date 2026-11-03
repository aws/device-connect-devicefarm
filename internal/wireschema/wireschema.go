// Package wireschema holds the shared control-frame contract: the JSON Schema for a
// frame body and the conformance fixture every peer agrees on. Both files are staged
// copies, checked in and embedded: the binary runs where the contract's source is
// absent. The build compares them with the source and fails when they differ. Never
// edit them by hand.
//
// Validate never returns the instance value: a frame may carry a bearer token, so an
// error names only the failing location and keywords.
package wireschema

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/santhosh-tekuri/jsonschema/v6/kind"
)

//go:embed control.schema.json
var controlSchemaDocument []byte

//go:embed conformance.json
var conformanceDocument []byte

// controlSchemaResourceName is a URN: a bare name resolves against the working
// directory, which may be gone when the daemon starts.
const controlSchemaResourceName = "urn:adbcontrol:v1:control.schema.json"

// controlFrameSchema compiles once at init. A failure is a build defect and panics
// like regexp.MustCompile.
var controlFrameSchema = mustCompileControlFrame()

func mustCompileControlFrame() *jsonschema.Schema {
	schema, err := compileControlFrame(controlSchemaDocument)
	if err != nil {
		panic(fmt.Sprintf("wireschema: %v", err))
	}
	return schema
}

// compileControlFrame compiles the document as-is: its root $ref already selects the
// control-frame definition.
func compileControlFrame(schemaDocument []byte) (*jsonschema.Schema, error) {
	document, err := jsonschema.UnmarshalJSON(bytes.NewReader(schemaDocument))
	if err != nil {
		return nil, fmt.Errorf("parsing the control-frame schema: %w", err)
	}
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource(controlSchemaResourceName, document); err != nil {
		return nil, fmt.Errorf("adding the control-frame schema: %w", err)
	}
	schema, err := compiler.Compile(controlSchemaResourceName)
	if err != nil {
		return nil, fmt.Errorf("compiling the control-frame schema: %w", err)
	}
	return schema, nil
}

// Validate checks one frame body, the JSON after the magic, against the schema. The
// codec strips the magic and applies the strict-parse rules first. The error names
// the failing JSON pointer and keywords, never the value or the library message.
func Validate(document []byte) error {
	instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(document))
	if err != nil {
		return errors.New("control frame body is not JSON")
	}
	err = controlFrameSchema.Validate(instance)
	if err == nil {
		return nil
	}
	var validationErr *jsonschema.ValidationError
	if !errors.As(err, &validationErr) {
		return errors.New("control frame does not match the schema")
	}
	pointer, keywords := deepestFailure(validationErr)
	return fmt.Errorf("control frame does not match the schema at %s (%s)",
		pointer, strings.Join(keywords, ", "))
}

// deepestFailure picks the deepest failing instance location, the smallest pointer
// on a tie, and returns it with the sorted, unique keywords that failed there.
func deepestFailure(root *jsonschema.ValidationError) (string, []string) {
	leaves := collectLeaves(root, nil)
	pointer := ""
	depth := -1
	for _, leaf := range leaves {
		leafPointer := jsonPointer(leaf.InstanceLocation)
		deeper := len(leaf.InstanceLocation) > depth
		if deeper || (len(leaf.InstanceLocation) == depth && leafPointer < pointer) {
			pointer, depth = leafPointer, len(leaf.InstanceLocation)
		}
	}
	var keywords []string
	for _, leaf := range leaves {
		if jsonPointer(leaf.InstanceLocation) == pointer {
			keywords = append(keywords, failingKeyword(leaf.ErrorKind))
		}
	}
	slices.Sort(keywords)
	return pointer, slices.Compact(keywords)
}

func collectLeaves(node *jsonschema.ValidationError, leaves []*jsonschema.ValidationError) []*jsonschema.ValidationError {
	if len(node.Causes) == 0 {
		return append(leaves, node)
	}
	for _, cause := range node.Causes {
		leaves = collectLeaves(cause, leaves)
	}
	return leaves
}

// failingKeyword names the keyword behind an error kind; not has no keyword path.
func failingKeyword(errorKind jsonschema.ErrorKind) string {
	if _, isNot := errorKind.(*kind.Not); isNot {
		return "not"
	}
	path := errorKind.KeywordPath()
	if len(path) == 0 {
		return "schema"
	}
	return path[len(path)-1]
}

// jsonPointer joins a location with no escaping: every name in it comes from the
// schema, since an unknown key fails at its parent.
func jsonPointer(location []string) string {
	if len(location) == 0 {
		return "/"
	}
	return "/" + strings.Join(location, "/")
}

// Fixture is the conformance fixture: whole websocket messages every decoder judges,
// and parsed instances the schema alone judges.
type Fixture struct {
	Frames       []FrameVector `json:"frames"`
	ControlFrame []ValueCase   `json:"controlFrame"`
}

// FrameVector is one whole websocket message, magic included, with its verdict. Arm,
// Fields and Canonical are set on valid vectors only.
type FrameVector struct {
	ID        string      `json:"id"`
	Hex       string      `json:"hex"`
	Valid     bool        `json:"valid"`
	Arm       string      `json:"arm"`
	Fields    FrameFields `json:"fields"`
	Canonical bool        `json:"canonical"`
	Why       string      `json:"why"`
}

// FrameFields are the members a valid frame carries. Members an arm lacks stay zero.
type FrameFields struct {
	SessionUUID  string `json:"sessionUuid"`
	Reason       string `json:"reason"`
	Token        string `json:"token"`
	CustomerPort int    `json:"customerPort"`
}

// ValueCase is one parsed frame body and the schema's verdict on it.
type ValueCase struct {
	ID    string          `json:"id"`
	JSON  json.RawMessage `json:"json"`
	Valid bool            `json:"valid"`
	Why   string          `json:"why"`
}

// LoadFixture parses the embedded conformance fixture.
func LoadFixture() (Fixture, error) {
	var fixture Fixture
	if err := json.Unmarshal(conformanceDocument, &fixture); err != nil {
		return Fixture{}, fmt.Errorf("parsing the embedded fixture: %w", err)
	}
	return fixture, nil
}
