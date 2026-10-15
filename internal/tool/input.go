// Package tool defines the agent's vocabulary: operational tasks with typed arguments.
package tool

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/google/jsonschema-go/jsonschema"
)

// input is a typed argument struct with its JSON Schema, derived from the struct.
type input[I any] struct {
	schema   *jsonschema.Schema
	resolved *jsonschema.Resolved
	names    []string
}

// newInput derives the schema for I and lets the caller refine it (enums, patterns)
// before it is resolved.
func newInput[I any](refine func(*jsonschema.Schema)) (*input[I], error) {
	s, err := jsonschema.For[I](nil)
	if err != nil {
		return nil, fmt.Errorf("schema: %w", err)
	}
	if refine != nil {
		refine(s)
	}
	r, err := s.Resolve(nil)
	if err != nil {
		return nil, fmt.Errorf("schema: %w", err)
	}
	names := make([]string, 0, len(s.Properties))
	for name := range s.Properties {
		names = append(names, name)
	}
	return &input[I]{schema: s, resolved: r, names: names}, nil
}

// decode validates raw against the schema and returns both the typed value and the
// generic object that policy conditions and audit see.
func (in *input[I]) decode(raw json.RawMessage) (I, map[string]any, error) {
	var zero I
	if len(bytes.TrimSpace(raw)) == 0 {
		raw = json.RawMessage(`{}`)
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return zero, nil, fmt.Errorf("arguments must be a JSON object: %w", err)
	}
	if err := in.resolved.Validate(obj); err != nil {
		return zero, nil, err
	}
	var v I
	if err := json.Unmarshal(raw, &v); err != nil {
		return zero, nil, err
	}
	return v, obj, nil
}
