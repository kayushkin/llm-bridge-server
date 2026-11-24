package model

// ItemsBatchRequest reads named items without filtering or discovery rules.
type ItemsBatchRequest struct {
	IDs []string `json:"ids"`
}

// ItemsBatchResult preserves input order and duplicates. Missing/deleted items
// occupy a null slot; held items are included, just as for GET /items/{id}.
type ItemsBatchResult struct {
	Items []*Item `json:"items" tstype:"(Item | null)[],required"`
}
