package model

// BulkCardCommand names one command a bulk request applies to many cards.
type BulkCardCommand string

const (
	BulkCommandMove       BulkCardCommand = "move"
	BulkCommandAssign     BulkCardCommand = "assign"
	BulkCommandUnassign   BulkCardCommand = "unassign"
	BulkCommandHold       BulkCardCommand = "hold"
	BulkCommandUnhold     BulkCardCommand = "unhold"
	BulkCommandAddTags    BulkCardCommand = "add_tags"
	BulkCommandRemoveTags BulkCardCommand = "remove_tags"
)

// BulkCardCommands is the vocabulary, served by GET /api/bulk-card-commands so
// no client restates it.
var BulkCardCommands = []BulkCardCommand{
	BulkCommandMove, BulkCommandAssign, BulkCommandUnassign,
	BulkCommandHold, BulkCommandUnhold, BulkCommandAddTags, BulkCommandRemoveTags,
}

// MaxBulkCardCommandCards bounds one bulk request. Each card is a full
// single-card command with its own checks and its own event, so a request is
// as slow as its cards are many.
const MaxBulkCardCommandCards = 200

// BulkCardCommandOptions is GET /api/bulk-card-commands.
type BulkCardCommandOptions struct {
	Commands []BulkCardCommand `json:"commands"`
	MaxCards int               `json:"max_cards"`
}

// BulkCardCommandRequest is POST /api/bulk-card-commands: one command, many
// cards. Only the field its command reads may be set; any other is a 400, so a
// request that says "assign" and carries a move is not half-obeyed.
type BulkCardCommandRequest struct {
	CardIDs []string        `json:"card_ids"`
	Command BulkCardCommand `json:"command"`
	// Move is read by "move". Position is where the first card lands; each
	// card after it lands one further on, so the cards keep the order they
	// were sent in.
	Move *MoveCardRequest `json:"move,omitempty"`
	// PrincipalID is read by "assign" and "unassign".
	PrincipalID string `json:"principal_id,omitempty"`
	// Reason is read by "hold".
	Reason string `json:"reason,omitempty"`
	// Tags is read by "add_tags" and "remove_tags".
	Tags []string `json:"tags,omitempty"`
}

// BulkCardCommandResult is what happened to one card: the status and body the
// single-card route answered, unchanged.
type BulkCardCommandResult struct {
	CardID string `json:"card_id"`
	Status int    `json:"status"`
	// Response is the single-card route's own JSON answer — the placement, the
	// assignment, the item, or {"error":…}.
	Response any `json:"response" tstype:"unknown"`
}

// BulkCardCommandResponse reports every card. The request is not one
// transaction: each card is its own command, exactly as if it had been sent
// alone, and a card that was refused undoes nothing done to the others.
type BulkCardCommandResponse struct {
	Command   BulkCardCommand         `json:"command"`
	Succeeded int                     `json:"succeeded"`
	Failed    int                     `json:"failed"`
	Results   []BulkCardCommandResult `json:"results"`
}
