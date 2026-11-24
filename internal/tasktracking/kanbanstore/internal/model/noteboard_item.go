package model

// PriorityOfNoteboardItem reads the noteboard priority off an opaque item. A
// missing or unreadable priority is the unranked value, which earns no rung
// and no limit. It lives here, not in the API package, because both the
// timeline handler and the message-trigger dispatcher need the same answer.
func PriorityOfNoteboardItem(item map[string]any) int {
	if item == nil {
		return UnsetPriorityValue
	}
	switch v := item["priority"].(type) {
	case float64:
		return int(v)
	case int:
		return v
	}
	return UnsetPriorityValue
}
