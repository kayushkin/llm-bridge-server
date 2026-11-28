// Package httpjson writes the JSON answers and the error envelope every route
// of this server uses.
package httpjson

import (
	"encoding/json"
	"net/http"
)

// WriteJSON writes v as the JSON body of a 200 answer.
func WriteJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

// WriteJSONError writes the {"error":{"code":…,"message":…}} envelope the
// pty_unsupported and invalid_max_budget rejections already use, so a client
// can branch on a stable code instead of matching prose. The message is for
// a human reading a log and should say how to fix the request.
func WriteJSONError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]string{"code": code, "message": message},
	})
}
