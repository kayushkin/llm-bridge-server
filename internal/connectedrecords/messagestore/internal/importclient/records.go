package importclient

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"

	messagestore "github.com/kayushkin/message-store"
)

// MessageHash is a message's source_hash: the hash of everything the
// importer read into it, so a re-run that read the same skips the row and one
// that learned more (a picture now in photo-store) rewrites it.
func MessageHash(message messagestore.ImportMessage) string {
	message.SourceHash = ""
	encoded, err := json.Marshal(message)
	if err != nil {
		// An ImportMessage holds only strings, numbers and bools.
		panic(fmt.Sprintf("importclient: encode a message: %v", err))
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

// ReadStringMap reads a JSON object of strings, such as an importer's
// -people map from a handle or name to a people-store id. An empty path is an
// empty map.
func ReadStringMap(path string) (map[string]string, error) {
	object := map[string]string{}
	if path == "" {
		return object, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &object); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return object, nil
}

// Sink takes what an importer reads. *Client is one; a test records instead.
type Sink interface {
	AddConversation(messagestore.ImportConversation) error
	AddMessage(messagestore.ImportMessage) error
	AddConversationDay(messagestore.ImportConversationDay) error
}
