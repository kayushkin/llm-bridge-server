// Package sendanswer is the answer to one message sent into a session, as
// POST /sessions/{id}/send writes it and a sender inside this server reads it.
package sendanswer

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// Answer is the answer to one send: the status, content type and
// body POST /sessions/{id}/send writes. A sender inside this server reads the
// same answer a person calling the route gets.
type Answer struct {
	Status      int
	contentType string
	Body        []byte
}

// Write answers a request with it, as http.Error and httpjson.WriteJSON would have.
func (answer Answer) Write(w http.ResponseWriter) {
	w.Header().Set("Content-Type", answer.contentType)
	if strings.HasPrefix(answer.contentType, "text/plain") {
		w.Header().Set("X-Content-Type-Options", "nosniff")
	}
	w.WriteHeader(answer.Status)
	w.Write(answer.Body)
}

// Sent reports whether the message went to the session or was held.
func (answer Answer) Sent() bool {
	return answer.Status >= 200 && answer.Status <= 299
}

// PlainText is what http.Error writes.
func PlainText(status int, text string) Answer {
	return Answer{Status: status, contentType: "text/plain; charset=utf-8", Body: []byte(text + "\n")}
}

// JSON is a JSON body, encoded as httpjson.WriteJSON and WriteJSONError
// encode it.
func JSON(status int, value any) Answer {
	var body bytes.Buffer
	if err := json.NewEncoder(&body).Encode(value); err != nil {
		return PlainText(http.StatusInternalServerError, fmt.Sprintf("encode the answer: %v", err))
	}
	return Answer{Status: status, contentType: "application/json", Body: body.Bytes()}
}

// JSONError is the error envelope httpjson.WriteJSONError writes.
func JSONError(status int, code, message string) Answer {
	return JSON(status, map[string]any{"error": map[string]string{"code": code, "message": message}})
}

// NotBridgeControlled refuses a send to a session its parent harness drives.
func NotBridgeControlled() Answer {
	return JSONError(http.StatusConflict, "not_bridge_controlled",
		"this session is controlled by its parent harness process and cannot be messaged directly; message the session that spawned it")
}
