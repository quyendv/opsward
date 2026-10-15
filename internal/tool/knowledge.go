package tool

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/quyendv/opsward/internal/core"
	"github.com/quyendv/opsward/internal/engine"
	"github.com/quyendv/opsward/internal/knowledge"
)

const (
	defaultSearchLimit = 5
	maxSearchLimit     = 20
)

type searchInput struct {
	Query string `json:"query" jsonschema:"words describing the symptom or task, such as 'pod pending insufficient memory'"`
	Limit int    `json:"limit,omitempty" jsonschema:"maximum number of documents; default 5"`
}

// SearchKnowledge is search_knowledge. It is not bound to an environment.
func SearchKnowledge(lib *knowledge.Library) (engine.Tool, error) {
	in, err := newInput[searchInput](func(s *jsonschema.Schema) {
		s.Properties["query"].MinLength = jsonschema.Ptr(1)
		s.Properties["limit"].Minimum = jsonschema.Ptr(1.0)
		s.Properties["limit"].Maximum = jsonschema.Ptr(float64(maxSearchLimit))
	})
	if err != nil {
		return engine.Tool{}, err
	}
	return engine.Tool{
		Name: "search_knowledge",
		Description: "Search the operational knowledge packs (runbooks, known failure modes) by symptom or task. " +
			"Returns id, title, severity and summary; read a whole document with get_knowledge.",
		Risk:        core.RiskRead,
		InputSchema: in.schema,
		Args:        in.names,
		Global:      true,
		Prepare: func(raw json.RawMessage) (*engine.Prepared, error) {
			v, obj, err := in.decode(raw)
			if err != nil {
				return nil, err
			}
			limit := v.Limit
			if limit == 0 {
				limit = defaultSearchLimit
			}
			return &engine.Prepared{
				Args: obj,
				Run: func(context.Context, string) (any, error) {
					return lib.Pack().Search(v.Query, limit), nil
				},
			}, nil
		},
	}, nil
}

type getInput struct {
	ID string `json:"id" jsonschema:"document id, as returned by search_knowledge or attached to a tool result"`
}

// GetKnowledge is get_knowledge. It is not bound to an environment.
func GetKnowledge(lib *knowledge.Library) (engine.Tool, error) {
	in, err := newInput[getInput](func(s *jsonschema.Schema) {
		s.Properties["id"].MinLength = jsonschema.Ptr(1)
	})
	if err != nil {
		return engine.Tool{}, err
	}
	return engine.Tool{
		Name:        "get_knowledge",
		Description: "Read one knowledge document in full, by id.",
		Risk:        core.RiskRead,
		InputSchema: in.schema,
		Args:        in.names,
		Global:      true,
		Prepare: func(raw json.RawMessage) (*engine.Prepared, error) {
			v, obj, err := in.decode(raw)
			if err != nil {
				return nil, err
			}
			return &engine.Prepared{
				Args: obj,
				Run: func(context.Context, string) (any, error) {
					d, ok := lib.Pack().Get(v.ID)
					if !ok {
						return nil, fmt.Errorf("no knowledge document %q", v.ID)
					}
					return d, nil
				},
			}, nil
		},
	}, nil
}
