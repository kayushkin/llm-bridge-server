package api

import (
	"encoding/json"
	"github.com/kayushkin/llm-bridge-server/internal/tasktracking/items/model"
	"net/http"
)

// BatchItems is POST /items/batch: the named items in the order named,
// with null for one that names no live item.
func (a *API) BatchItems(w http.ResponseWriter, r *http.Request) {
	var request model.ItemsBatchRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	out, err := a.Batch(request.IDs)
	if err != nil {
		writeCallError(w, err)
		return
	}
	writeJSON(w, 200, out)
}
