package httpjson

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

)

// DecodeAdministrationBody reads a JSON object into target, refusing any key
// target does not have. A key the bridge sets itself gets a refusal that
// names it, since silently replacing it would leave the caller believing
// their value was kept.
func DecodeAdministrationBody(w http.ResponseWriter, r *http.Request, target any, keysTheBridgeSets ...string) bool {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		WriteJSONError(w, http.StatusBadRequest, "invalid_request", fmt.Sprintf("read the body: %v", err))
		return false
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil {
		WriteJSONError(w, http.StatusBadRequest, "invalid_request", fmt.Sprintf("the body must be a JSON object: %v", err))
		return false
	}
	for _, key := range keysTheBridgeSets {
		if _, present := keys[key]; present {
			WriteJSONError(w, http.StatusBadRequest, key+"_not_accepted", fmt.Sprintf(
				"%s is set by this server to the calling administrator's principal and may not be sent", key))
			return false
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		WriteJSONError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return false
	}
	return true
}
