// Package emit owns the output side of the processor: canonical JSON
// serialization, gzip size budgets, and syncing the managed file set into
// the derived repository checkout (write-if-different, stale deletion).
package emit

import (
	"bytes"
	"encoding/json"
)

// Canonical serializes v as the repository's canonical JSON: stock Go
// encoder, sorted object keys (Go map marshaling), 1-space indent, UTF-8,
// LF, no HTML escaping, exactly one trailing newline.
func Canonical(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", " ")
	if err := enc.Encode(v); err != nil { // Encode appends the single trailing \n
		return nil, err
	}
	return buf.Bytes(), nil
}
