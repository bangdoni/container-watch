package sse

import "encoding/json"

// mustJSON marshals v, returning an empty object on failure.
func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte(`{}`)
	}
	return b
}
