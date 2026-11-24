package api

import (
	"encoding/json"
	"github.com/kayushkin/noteboard/model"
	"net/http"
)

func (a *API) batchItems(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, 405, "method not allowed")
		return
	}
	var request model.ItemsBatchRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	if request.IDs == nil {
		writeError(w, 400, "ids is required")
		return
	}
	query := model.ItemsQuery{IDs: request.IDs, IncludeItems: true}
	if err := query.Validate(); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	result, err := a.store.QueryItems(&query)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	byID := map[string]*model.Item{}
	for _, item := range result.Items {
		byID[item.ID] = item
	}
	out := model.ItemsBatchResult{Items: make([]*model.Item, len(request.IDs))}
	for i, id := range request.IDs {
		out.Items[i] = byID[id]
	}
	writeJSON(w, 200, out)
}
