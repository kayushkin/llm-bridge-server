// Command people-import-multichat proposes one people-store suggestion per
// contact group multichat reports at GET /api/contacts/unified.
//
// multichat groups bridged chat accounts by people-store person id (by
// principal-store id for part of 2026-10-01, by lowercased display name before),
// and those links are already attached here, so their handles are skipped.
// What is left is mostly single accounts linked to no one. This command only
// proposes: people-store attaches nothing until someone accepts the
// suggestion, and leaves out every handle already attached to a person or
// already proposed by an importer, so a second run proposes only what is new.
//
// Then it reports activity for every handle it read, so people-store can put
// the people the operator talks to first: from multichat's message log (paged
// whole, edits left out) how many messages each handle sent and when it last
// did, and from multichat's conversations which handles share a one-to-one
// room with the operator and when that room was last active. Only handles in
// GET /api/contacts/unified are reported; multichat leaves our own puppets out
// of that list, and the message log does not mark them.
//
// Settings come from the environment, all required, and are declared in
// peoplestore.ImporterSettingDefinitions:
//
//	PEOPLE_STORE_URL  people-store's base URL (http://127.0.0.1:8321)
//	MULTICHAT_URL     multichat's base URL (http://localhost:8402)
//	AUTH_STORE_URL    auth-store, which holds multichat's API token as provider "multichat"
//	AUTH_STORE_TOKEN  auth-store's own bearer
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/kayushkin/llm-bridge/servicesettings"
	peoplestore "github.com/kayushkin/people-store"
)

const authStoreProvider = "multichat"

// unifiedContact is one entry of multichat's GET /api/contacts/unified.
type unifiedContact struct {
	DisplayName string `json:"display_name"`
	Identities  []struct {
		UserID   string `json:"user_id"`
		Platform string `json:"platform"`
	} `json:"identities"`
	Platforms []string `json:"platforms"`
}

var httpClient = &http.Client{Timeout: 30 * time.Second}

func main() {
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: people-import-multichat\n\nProposes a people-store suggestion for each multichat contact group with handles nobody has yet.\n")
	}
	flag.Parse()

	settings, err := peoplestore.NewImporterSettingsRegistry(servicesettings.ProcessEnvironment())
	if err != nil {
		fail("%v", err)
	}
	if err := settings.CheckRequired(); err != nil {
		fail("%v", err)
	}
	peopleStoreURL := strings.TrimRight(settings.String(peoplestore.ImporterSettingPeopleStoreURL), "/")
	multichatURL := strings.TrimRight(settings.String(peoplestore.ImporterSettingMultichatURL), "/")

	token, err := resolveMultichatToken(settings.String(peoplestore.ImporterSettingAuthStoreURL), settings.String(peoplestore.ImporterSettingAuthStoreToken))
	if err != nil {
		fail("%v", err)
	}
	contacts, err := fetchUnifiedContacts(multichatURL, token)
	if err != nil {
		fail("%v", err)
	}

	created, nothingNew, failed := 0, 0, 0
	for _, contact := range contacts {
		input := peoplestore.SuggestionInput{Source: peoplestore.SuggestionSourceMultichat, DisplayName: contact.DisplayName}
		for _, identity := range contact.Identities {
			input.Identities = append(input.Identities, peoplestore.IdentityInput{Kind: peoplestore.IdentityKindMatrixUser, Value: identity.UserID, Label: identity.Platform})
		}
		detail, err := json.Marshal(map[string]any{"platforms": contact.Platforms})
		if err != nil {
			fail("%v", err)
		}
		input.SourceDetail = detail
		proposal, err := propose(peopleStoreURL, input)
		if err != nil {
			failed++
			fmt.Fprintf(os.Stderr, "people-import-multichat: %q: %v\n", contact.DisplayName, err)
			continue
		}
		if proposal.Created {
			created++
			fmt.Printf("%s  %s (%d handles)\n", proposal.Suggestion.ID, contact.DisplayName, len(proposal.Suggestion.Payload.Identities))
		} else {
			nothingNew++
		}
	}
	if failed > 0 {
		summary, _ := json.Marshal(map[string]int{"contact_groups": len(contacts), "suggestions_created": created, "nothing_new": nothingNew, "failed": failed})
		fmt.Println(string(summary))
		fail("%d contact groups failed; activity not reported", failed)
	}

	contactHandles := map[string]bool{}
	for _, contact := range contacts {
		for _, identity := range contact.Identities {
			contactHandles[identity.UserID] = true
		}
	}
	started := time.Now()
	activity, logStats, err := gatherActivity(multichatURL, token, contactHandles)
	if err != nil {
		fail("%v", err)
	}
	reported, err := reportActivity(peopleStoreURL, activity)
	if err != nil {
		fail("%v", err)
	}
	summary, err := json.Marshal(map[string]any{
		"contact_groups": len(contacts), "suggestions_created": created, "nothing_new": nothingNew, "failed": failed,
		"message_log_rows": logStats.rows, "message_log_pages": logStats.pages, "senders_in_log": logStats.senders,
		"senders_not_in_contacts": logStats.sendersNotInContacts, "direct_chats": logStats.directChats,
		"handles_with_activity": reported, "activity_seconds": int(time.Since(started).Seconds()),
	})
	if err != nil {
		fail("%v", err)
	}
	fmt.Println(string(summary))
}

// messageLogPageSize is the most GET /api/message-log answers at once.
const messageLogPageSize = 1000

// activityReportBatchSize is how many handles one PUT /handle-activity carries.
const activityReportBatchSize = 500

type activityStats struct {
	rows, pages, senders, sendersNotInContacts, directChats int
}

// loggedMessage is the part of one GET /api/message-log row this command reads.
type loggedMessage struct {
	ID           int64     `json:"id"`
	SenderUserID string    `json:"sender_user_id"`
	SentAt       time.Time `json:"sent_at"`
}

// conversation is the part of one GET /api/conversations entry this command reads.
type conversation struct {
	RoomID                     string `json:"room_id"`
	LastActivity               int64  `json:"last_activity"` // milliseconds
	DirectMessagePartnerUserID string `json:"direct_message_partner_user_id"`
}

// gatherActivity reads the whole message log and every conversation, and
// returns activity for the handles in contactHandles.
func gatherActivity(multichatURL, token string, contactHandles map[string]bool) ([]peoplestore.HandleActivity, activityStats, error) {
	var stats activityStats
	byHandle := map[string]*peoplestore.HandleActivity{}
	entry := func(handle string) *peoplestore.HandleActivity {
		activity, ok := byHandle[handle]
		if !ok {
			activity = &peoplestore.HandleActivity{Kind: peoplestore.IdentityKindMatrixUser, Value: handle}
			byHandle[handle] = activity
		}
		return activity
	}

	// The log is newest first and `before` is exclusive on sent_at, so a page
	// that ends partway through one millisecond would lose the rest of it.
	// Each next page starts a millisecond after the last row's time instead,
	// and rows already counted are skipped by id.
	seen := map[int64]bool{}
	senders := map[string]bool{}
	before := ""
	for {
		query := url.Values{"limit": {fmt.Sprint(messageLogPageSize)}}
		if before != "" {
			query.Set("before", before)
		}
		var page struct {
			Messages []loggedMessage `json:"messages"`
		}
		if err := getMultichat(multichatURL, token, "/api/message-log?"+query.Encode(), &page); err != nil {
			return nil, stats, err
		}
		stats.pages++
		fresh := 0
		for _, message := range page.Messages {
			if seen[message.ID] {
				continue
			}
			seen[message.ID] = true
			fresh++
			stats.rows++
			if message.SenderUserID == "" {
				return nil, stats, fmt.Errorf("multichat: message-log row %d has no sender_user_id", message.ID)
			}
			if message.SentAt.IsZero() {
				return nil, stats, fmt.Errorf("multichat: message-log row %d has no sent_at", message.ID)
			}
			senders[message.SenderUserID] = true
			if !contactHandles[message.SenderUserID] {
				continue
			}
			activity := entry(message.SenderUserID)
			activity.MessageCount++
			if sentAt := message.SentAt.Unix(); sentAt > activity.LastActivityAt {
				activity.LastActivityAt = sentAt
			}
		}
		if len(page.Messages) < messageLogPageSize {
			break
		}
		if fresh == 0 {
			return nil, stats, fmt.Errorf("multichat: message-log page before %s held %d rows already read; more than a page share one millisecond, so paging cannot go on", before, len(page.Messages))
		}
		last := page.Messages[len(page.Messages)-1].SentAt
		before = last.Add(time.Millisecond).UTC().Format(time.RFC3339Nano)
	}
	stats.senders = len(senders)
	for sender := range senders {
		if !contactHandles[sender] {
			stats.sendersNotInContacts++
		}
	}

	var conversations []conversation
	if err := getMultichat(multichatURL, token, "/api/conversations", &conversations); err != nil {
		return nil, stats, err
	}
	for _, room := range conversations {
		partner := room.DirectMessagePartnerUserID
		if partner == "" || !contactHandles[partner] {
			continue
		}
		stats.directChats++
		activity := entry(partner)
		activity.DirectChat = true
		if lastActivity := room.LastActivity / 1000; lastActivity > activity.LastActivityAt {
			activity.LastActivityAt = lastActivity
		}
	}

	activities := make([]peoplestore.HandleActivity, 0, len(byHandle))
	for _, activity := range byHandle {
		activities = append(activities, *activity)
	}
	return activities, stats, nil
}

// reportActivity sends the activity to people-store in batches.
func reportActivity(peopleStoreURL string, activities []peoplestore.HandleActivity) (int, error) {
	reported := 0
	for start := 0; start < len(activities); start += activityReportBatchSize {
		end := min(start+activityReportBatchSize, len(activities))
		encoded, err := json.Marshal(peoplestore.HandleActivityReport{Source: peoplestore.SuggestionSourceMultichat, Activities: activities[start:end]})
		if err != nil {
			return reported, err
		}
		request, err := http.NewRequest(http.MethodPut, peopleStoreURL+"/handle-activity", bytes.NewReader(encoded))
		if err != nil {
			return reported, err
		}
		request.Header.Set("Content-Type", "application/json")
		response, err := httpClient.Do(request)
		if err != nil {
			return reported, fmt.Errorf("people-store: %w", err)
		}
		body, err := io.ReadAll(response.Body)
		response.Body.Close()
		if err != nil {
			return reported, fmt.Errorf("people-store: %w", err)
		}
		if response.StatusCode != http.StatusOK {
			return reported, fmt.Errorf("people-store: PUT /handle-activity answered %d: %s", response.StatusCode, strings.TrimSpace(string(body)))
		}
		var result peoplestore.HandleActivityResult
		if err := json.Unmarshal(body, &result); err != nil {
			return reported, fmt.Errorf("people-store: %w", err)
		}
		reported += result.Updated
	}
	return reported, nil
}

// getMultichat GETs one multichat route and decodes the answer into target.
func getMultichat(multichatURL, token, path string, target any) error {
	request, err := http.NewRequest(http.MethodGet, multichatURL+path, nil)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := httpClient.Do(request)
	if err != nil {
		return fmt.Errorf("multichat: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return fmt.Errorf("multichat: %w", err)
	}
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("multichat: GET %s answered %d: %s", path, response.StatusCode, strings.TrimSpace(string(body)))
	}
	if err := json.Unmarshal(body, target); err != nil {
		return fmt.Errorf("multichat: GET %s: %w", path, err)
	}
	return nil
}

func fetchUnifiedContacts(multichatURL, token string) ([]unifiedContact, error) {
	request, err := http.NewRequest(http.MethodGet, multichatURL+"/api/contacts/unified", nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := httpClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("multichat: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, fmt.Errorf("multichat: %w", err)
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("multichat: GET /api/contacts/unified answered %d: %s", response.StatusCode, strings.TrimSpace(string(body)))
	}
	var contacts []unifiedContact
	if err := json.Unmarshal(body, &contacts); err != nil {
		return nil, fmt.Errorf("multichat: contacts: %w", err)
	}
	return contacts, nil
}

func propose(peopleStoreURL string, input peoplestore.SuggestionInput) (*peoplestore.SuggestionProposal, error) {
	encoded, err := json.Marshal(input)
	if err != nil {
		return nil, err
	}
	response, err := httpClient.Post(peopleStoreURL+"/suggestions", "application/json", bytes.NewReader(encoded))
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusCreated {
		return nil, fmt.Errorf("POST /suggestions answered %d: %s", response.StatusCode, strings.TrimSpace(string(body)))
	}
	var proposal peoplestore.SuggestionProposal
	if err := json.Unmarshal(body, &proposal); err != nil {
		return nil, err
	}
	return &proposal, nil
}

func resolveMultichatToken(authStoreURL, authStoreToken string) (string, error) {
	request, err := http.NewRequest(http.MethodGet, strings.TrimRight(authStoreURL, "/")+"/api/resolve/"+authStoreProvider, nil)
	if err != nil {
		return "", err
	}
	request.Header.Set("Authorization", "Bearer "+authStoreToken)
	request.Header.Set("X-Auth-App", "people-import-multichat")
	request.Header.Set("X-Auth-Reason", "read contacts, message log and conversations")
	response, err := httpClient.Do(request)
	if err != nil {
		return "", fmt.Errorf("auth-store: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return "", fmt.Errorf("auth-store: %w", err)
	}
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("auth-store: resolve %s: %d %s", authStoreProvider, response.StatusCode, strings.TrimSpace(string(body)))
	}
	var credential struct {
		AuthType string `json:"auth_type"`
		APIKey   string `json:"api_key"`
	}
	if err := json.Unmarshal(body, &credential); err != nil {
		return "", fmt.Errorf("auth-store: %w", err)
	}
	if credential.AuthType != "api_key" || credential.APIKey == "" {
		return "", fmt.Errorf("auth-store: provider %s is %q with an empty api_key; expected an api_key credential", authStoreProvider, credential.AuthType)
	}
	return credential.APIKey, nil
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "people-import-multichat: "+format+"\n", args...)
	os.Exit(1)
}
