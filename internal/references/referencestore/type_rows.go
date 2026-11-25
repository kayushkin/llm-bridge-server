package referencestore

import (
	"bytes"
	"encoding/json"
)

// TypeRows is each of types encoded as GET /types answers it, for a module in
// the same process that passes reference-store's list on unchanged.
func TypeRows(types []ReferenceType) ([]json.RawMessage, error) {
	rows := make([]json.RawMessage, 0, len(types))
	for i := range types {
		var encoded bytes.Buffer
		encoder := json.NewEncoder(&encoded)
		// As writeJSON: an & in a description arrives as itself.
		encoder.SetEscapeHTML(false)
		if err := encoder.Encode(types[i]); err != nil {
			return nil, err
		}
		rows = append(rows, json.RawMessage(bytes.TrimSuffix(encoded.Bytes(), []byte("\n"))))
	}
	return rows, nil
}
