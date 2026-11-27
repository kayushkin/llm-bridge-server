// Package photocontext reads who a photo's source names — the chat it was
// sent in, or the transcript that cites it — as people-store person ids,
// and holds the plain HTTP helpers photo-store's commands share.
package photocontext

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/photos"
	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/photos/internal/messagecitation"
)

// LargestChatForContext is the most members a room may have for all of them
// to count as context; a bigger room counts only its sender.
const LargestChatForContext = 8

var personReference = regexp.MustCompile(`ref:(person_[0-9]{6,})`)

// Client makes the JSON calls. Every request goes through HTTP. Do writes
// each wait for a store that refuses connections to Diagnostics, which must
// be set.
type Client struct {
	HTTP        *http.Client
	Diagnostics io.Writer
}

// GetJSON reads a JSON answer into out; a 404 is ErrNotFound.
func (c Client) GetJSON(ctx context.Context, target string, headers map[string]string, out any) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	response, err := c.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return err
	}
	if response.StatusCode == http.StatusNotFound {
		return ErrNotFound
	}
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %d %s", target, response.StatusCode, bytes.TrimSpace(body))
	}
	return json.Unmarshal(body, out)
}

// ErrNotFound is a 404.
var ErrNotFound = fmt.Errorf("not found")

// PostJSON sends payload and answers the status and body.
func (c Client) PostJSON(ctx context.Context, target string, payload any) (int, []byte, error) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return 0, nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(encoded))
	if err != nil {
		return 0, nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := c.Do(request)
	if err != nil {
		return 0, nil, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	return response.StatusCode, body, err
}

// restartWait is how long Do waits out a refused connection: a deploy
// restarts a store in seconds, and a long run should not die of it.
const restartWait = 2 * time.Minute

// Do sends a request, sending it again every 5 seconds while the connection
// is refused, for up to restartWait. Any other failure is returned at once,
// and so is the end of the request's context while it waits.
func (c Client) Do(request *http.Request) (*http.Response, error) {
	deadline := time.Now().Add(restartWait)
	for {
		if request.GetBody != nil {
			body, err := request.GetBody()
			if err != nil {
				return nil, err
			}
			request.Body = body
		}
		response, err := c.HTTP.Do(request)
		if err == nil || !errors.Is(err, syscall.ECONNREFUSED) || time.Now().After(deadline) {
			return response, err
		}
		fmt.Fprintf(c.Diagnostics, "%s %s: connection refused; trying again in 5 s\n", request.Method, request.URL.Host)
		select {
		case <-request.Context().Done():
			return nil, request.Context().Err()
		case <-time.After(5 * time.Second):
		}
	}
}

// Sources are where a photo's context is read.
type Sources struct {
	MultichatURL, MultichatToken, JournalURL, PeopleURL string
	Messages                                            messagecitation.Store
}

// Reader finds the people a photo's source names, caching each look-up.
type Reader struct {
	Client
	Sources
	rooms   map[string]room
	persons map[string]string // matrix user id → person id, "" for nobody
	entries map[string][]string
	// conversations caches message-store's conversations by id.
	conversations map[string]messagecitation.Conversation
	operatorID    string
}

// NewReader reads multichat's rooms once and answers a Reader. operatorID is
// the person whose photos these are, counted in every small chat.
func NewReader(ctx context.Context, client Client, sources Sources, operatorID string) (*Reader, error) {
	reader := &Reader{Client: client, Sources: sources, persons: map[string]string{}, entries: map[string][]string{},
		conversations: map[string]messagecitation.Conversation{}, operatorID: operatorID}
	return reader, reader.loadRooms(ctx)
}

type room struct {
	MemberCount int `json:"member_count"`
	Members     []struct {
		UserID string `json:"user_id"`
	} `json:"members"`
	RoomID string `json:"room_id"`
}

func (r *Reader) loadRooms(ctx context.Context) error {
	var rooms []room
	if err := r.GetJSON(ctx, r.MultichatURL+"/api/conversations", map[string]string{"Authorization": "Bearer " + r.MultichatToken}, &rooms); err != nil {
		return fmt.Errorf("multichat conversations: %w", err)
	}
	r.rooms = map[string]room{}
	for _, each := range rooms {
		r.rooms[each.RoomID] = each
	}
	return nil
}

func (r *Reader) personOfUser(ctx context.Context, userID string) (string, error) {
	if personID, known := r.persons[userID]; known {
		return personID, nil
	}
	var answer struct {
		Person struct {
			ID string `json:"id"`
		} `json:"person"`
	}
	err := r.GetJSON(ctx, r.PeopleURL+"/identities/resolve?"+url.Values{"kind": {"matrix_user"}, "value": {userID}}.Encode(), nil, &answer)
	if err != nil && err != ErrNotFound {
		return "", fmt.Errorf("people-store resolve %s: %w", userID, err)
	}
	r.persons[userID] = answer.Person.ID
	return answer.Person.ID, nil
}

// PeopleOf answers the people a photo's source names, with a short reason
// in markdown for the evidence.
//
// message-store's look-ups go through messagecitation, whose calls take no
// context, so PeopleOf checks ctx before each one instead.
func (r *Reader) PeopleOf(ctx context.Context, photo photos.Photo) (map[string]string, error) {
	named := map[string]string{}
	switch {
	case strings.HasPrefix(photo.SourcePath, "message_"):
		var answer struct {
			Messages []struct {
				RoomID       string `json:"room_id"`
				SenderUserID string `json:"sender_user_id"`
				RoomName     string `json:"room_name"`
			} `json:"messages"`
		}
		err := r.GetJSON(ctx, r.MultichatURL+"/api/message-log/"+url.PathEscape(photo.SourcePath), map[string]string{"Authorization": "Bearer " + r.MultichatToken}, &answer)
		if err == ErrNotFound || (err == nil && len(answer.Messages) == 0) {
			return named, nil
		}
		if err != nil {
			return nil, fmt.Errorf("multichat %s: %w", photo.SourcePath, err)
		}
		message := answer.Messages[0]
		users := []string{message.SenderUserID}
		reason := fmt.Sprintf("sent in [%s](ref:%s)", message.RoomName, photo.SourcePath)
		if each, known := r.rooms[message.RoomID]; known && each.MemberCount <= LargestChatForContext {
			users = users[:0]
			for _, member := range each.Members {
				users = append(users, member.UserID)
			}
			// The operator is in every small chat of their own, under
			// whichever handle that app gives them.
			if r.operatorID != "" {
				named[r.operatorID] = reason
			}
		} else {
			reason = fmt.Sprintf("posted in [%s](ref:%s)", message.RoomName, photo.SourcePath)
		}
		for _, userID := range users {
			personID, err := r.personOfUser(ctx, userID)
			if err != nil {
				return nil, err
			}
			if personID != "" {
				named[personID] = reason
			}
		}
	case strings.HasPrefix(photo.SourcePath, "chat_message_"):
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		message, err := r.Sources.Messages.MessageByID(photo.SourcePath)
		if err != nil {
			return nil, err
		}
		conversation, cached := r.conversations[message.ConversationID]
		if !cached {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if conversation, err = r.Sources.Messages.ConversationByID(message.ConversationID); err != nil {
				return nil, err
			}
			r.conversations[message.ConversationID] = conversation
		}
		reason := fmt.Sprintf("sent in [%s](ref:%s)", conversation.Title, photo.SourcePath)
		if len(conversation.Participants) > LargestChatForContext {
			if message.SenderPersonID != "" {
				named[message.SenderPersonID] = fmt.Sprintf("posted in [%s](ref:%s)", conversation.Title, photo.SourcePath)
			}
			break
		}
		// Everyone in a small chat, the operator included under whichever
		// handle that app gives them.
		for _, participant := range conversation.Participants {
			if participant.PersonID != "" {
				named[participant.PersonID] = reason
			}
		}
		if r.operatorID != "" {
			named[r.operatorID] = reason
		}
	case strings.HasPrefix(photo.SourcePath, "entry_"):
		people, cached := r.entries[photo.SourcePath]
		if !cached {
			var entry struct {
				Body string `json:"body"`
			}
			if err := r.GetJSON(ctx, r.JournalURL+"/entries/"+url.PathEscape(photo.SourcePath), nil, &entry); err != nil && err != ErrNotFound {
				return nil, fmt.Errorf("journal-store %s: %w", photo.SourcePath, err)
			}
			for _, match := range personReference.FindAllStringSubmatch(entry.Body, -1) {
				people = append(people, match[1])
			}
			r.entries[photo.SourcePath] = people
		}
		for _, personID := range people {
			named[personID] = fmt.Sprintf("in [the day's chat](ref:%s)", photo.SourcePath)
		}
		// The operator is in every chat of their own.
		if len(people) > 0 && r.operatorID != "" {
			named[r.operatorID] = fmt.Sprintf("in [the day's chat](ref:%s)", photo.SourcePath)
		}
	}
	return named, nil
}

// LoadLinks reads every photo-people link.
func LoadLinks(ctx context.Context, web Client, storeURL string) ([]photos.PhotoPerson, error) {
	var links []photos.PhotoPerson
	for offset := 0; ; offset += 5000 {
		var page []photos.PhotoPerson
		if err := web.GetJSON(ctx, fmt.Sprintf("%s/photo-people?limit=5000&offset=%d", storeURL, offset), nil, &page); err != nil {
			return nil, fmt.Errorf("photo-people: %w", err)
		}
		links = append(links, page...)
		if len(page) < 5000 {
			return links, nil
		}
	}
}

// LoadPhotos reads every live photo.
func LoadPhotos(ctx context.Context, web Client, storeURL string) ([]photos.Photo, error) {
	var storedPhotos []photos.Photo
	for offset := 0; ; offset += 1000 {
		var page []photos.Photo
		if err := web.GetJSON(ctx, fmt.Sprintf("%s/photos?limit=1000&offset=%d", storeURL, offset), nil, &page); err != nil {
			return nil, fmt.Errorf("photos: %w", err)
		}
		storedPhotos = append(storedPhotos, page...)
		if len(page) < 1000 {
			return storedPhotos, nil
		}
	}
}
