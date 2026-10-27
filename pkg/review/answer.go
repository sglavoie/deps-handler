package review

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// decodeAnswer validates a single JSON verdict against Schema. Every agent
// adapter uses it: requesting structured output is not a substitute for
// checking the response before it can authorize a merge.
func decodeAnswer(data []byte) (Answer, error) {
	// Decoding straight into Answer accepts duplicate and case-insensitive
	// keys, missing fields and nulls. A later "decision" could overwrite a
	// rejection, so enforce the schema before constructing the answer.
	dec := json.NewDecoder(bytes.NewReader(data))
	if token, err := dec.Token(); err != nil || token != json.Delim('{') {
		return Answer{}, errors.New("answer must be a JSON object")
	}
	fields := make(map[string]any)
	for dec.More() {
		token, err := dec.Token()
		if err != nil {
			return Answer{}, err
		}
		key, ok := token.(string)
		if !ok {
			return Answer{}, errors.New("answer field must be a string")
		}
		if _, exists := fields[key]; exists {
			return Answer{}, fmt.Errorf("duplicate answer field %q", Clean(key))
		}
		var value any
		if err := dec.Decode(&value); err != nil {
			return Answer{}, err
		}
		switch key {
		case "decision", "summary":
			if _, ok := value.(string); !ok {
				return Answer{}, fmt.Errorf("%s must be a string", key)
			}
		case "breaking_changes", "concerns":
			items, ok := value.([]any)
			if !ok {
				return Answer{}, fmt.Errorf("%s must be an array of strings", key)
			}
			for _, item := range items {
				if _, ok := item.(string); !ok {
					return Answer{}, fmt.Errorf("%s must contain only strings", key)
				}
			}
		default:
			return Answer{}, fmt.Errorf("unknown answer field %q", Clean(key))
		}
		fields[key] = value
	}
	if _, err := dec.Token(); err != nil {
		return Answer{}, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return Answer{}, errors.New("more than one JSON value")
	}
	if len(fields) != 4 {
		return Answer{}, errors.New("answer requires decision, summary, breaking_changes and concerns")
	}
	var a Answer
	if err := json.Unmarshal(data, &a); err != nil {
		return Answer{}, err
	}
	if a.Decision != "approve" && a.Decision != "reject" {
		return Answer{}, fmt.Errorf("decision %q is neither approve nor reject", Clean(a.Decision))
	}
	return a, nil
}
