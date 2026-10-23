package signinstore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// errNotFoundUpstream is another store answering 404.
var errNotFoundUpstream = errors.New("not found upstream")

// kindThatSignsIn is principal-store's kind for a person. Only a person signs
// in here: a group is a set of principals, a contact is an outside party, and
// a peer server acts through its own token.
const kindThatSignsIn = "human"

// Principal is the part of principal-store's record this service reads.
type Principal struct {
	ID              string `json:"id"`
	Kind            string `json:"kind"`
	DisplayName     string `json:"display_name"`
	DisabledAt      int64  `json:"disabled_at"`
	IsAdministrator bool   `json:"is_administrator"`
	Groups          []struct {
		ID string `json:"id"`
	} `json:"groups"`
}

// GroupIDs is the ids of the active groups the principal is in.
func (principal Principal) GroupIDs() []string {
	ids := make([]string, 0, len(principal.Groups))
	for _, group := range principal.Groups {
		ids = append(ids, group.ID)
	}
	return ids
}

// refusalToSignIn says why a principal may not sign in, or nil.
func (principal Principal) refusalToSignIn() error {
	if principal.Kind != kindThatSignsIn {
		return fmt.Errorf("%w: %s is a %q principal, and only a person signs in", ErrMayNotSignIn, principal.ID, principal.Kind)
	}
	if principal.DisabledAt != 0 {
		return fmt.Errorf("%w: %s is disabled in principal-store", ErrMayNotSignIn, principal.ID)
	}
	return nil
}

// ErrMayNotSignIn is a principal that exists but may not sign in.
var ErrMayNotSignIn = errors.New("may not sign in")

// principalCacheLifetime is how long principal-store's answer about one
// principal is reused. It is the delay on a change there: disable someone and
// their session keeps working for up to this long. The film player asks every
// few seconds of playback, so asking every time would be thousands of reads.
const principalCacheLifetime = 30 * time.Second

// Directory reads principal-store and people-store, and sends through
// multichat.
type Directory struct {
	principalStoreURL string
	peopleStoreURL    string
	multichatURL      string
	authStoreURL      string
	authStoreToken    string
	multichatCredID   string
	client            *http.Client
	now               func() time.Time

	cacheMutex sync.Mutex
	cache      map[string]cachedPrincipal
}

type cachedPrincipal struct {
	principal Principal
	readAt    time.Time
}

// DirectoryConfig is where the other services are.
type DirectoryConfig struct {
	PrincipalStoreURL     string
	PeopleStoreURL        string
	MultichatURL          string
	AuthStoreURL          string
	AuthStoreToken        string
	MultichatCredentialID string
}

// NewDirectory builds a Directory.
func NewDirectory(config DirectoryConfig) *Directory {
	trim := func(raw string) string { return strings.TrimRight(raw, "/") }
	return &Directory{
		principalStoreURL: trim(config.PrincipalStoreURL), peopleStoreURL: trim(config.PeopleStoreURL),
		multichatURL: trim(config.MultichatURL), authStoreURL: trim(config.AuthStoreURL),
		authStoreToken: config.AuthStoreToken, multichatCredID: config.MultichatCredentialID,
		client: &http.Client{Timeout: 15 * time.Second}, now: time.Now, cache: map[string]cachedPrincipal{},
	}
}

func (d *Directory) getJSON(ctx context.Context, rawURL string, headers map[string]string, out any) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	response, err := d.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if err != nil {
		return err
	}
	if response.StatusCode == http.StatusNotFound {
		return fmt.Errorf("%w: GET %s: %s", errNotFoundUpstream, rawURL, strings.TrimSpace(string(body)))
	}
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s answered %d: %s", rawURL, response.StatusCode, strings.TrimSpace(string(body)))
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("GET %s: parse: %w", rawURL, err)
	}
	return nil
}

// Principal returns principal-store's record, from the cache when it was read
// less than principalCacheLifetime ago. A missing principal wraps ErrNotFound.
func (d *Directory) Principal(ctx context.Context, principalID string) (Principal, error) {
	d.cacheMutex.Lock()
	cached, known := d.cache[principalID]
	d.cacheMutex.Unlock()
	if known && d.now().Sub(cached.readAt) < principalCacheLifetime {
		return cached.principal, nil
	}
	var principal Principal
	err := d.getJSON(ctx, d.principalStoreURL+"/principals/"+url.PathEscape(principalID), nil, &principal)
	if errors.Is(err, errNotFoundUpstream) {
		return Principal{}, fmt.Errorf("%w: principal-store has no %s", ErrNotFound, principalID)
	}
	if err != nil {
		return Principal{}, fmt.Errorf("principal-store: %w", err)
	}
	if principal.ID != principalID || principal.Kind == "" {
		return Principal{}, fmt.Errorf("principal-store answered for %q kind %q, not %s", principal.ID, principal.Kind, principalID)
	}
	d.cacheMutex.Lock()
	d.cache[principalID] = cachedPrincipal{principal: principal, readAt: d.now()}
	d.cacheMutex.Unlock()
	return principal, nil
}

// Person is the part of people-store's person this service reads.
type Person struct {
	ID          string `json:"id"`
	PrincipalID string `json:"principal_id"`
	Identities  []struct {
		Kind  string `json:"kind"`
		Value string `json:"value"`
	} `json:"identities"`
}

// PersonOwningHandle resolves a handle through people-store, or nil when
// nobody owns it.
func (d *Directory) PersonOwningHandle(ctx context.Context, kind, value string) (*Person, error) {
	var resolved struct {
		Person struct {
			ID string `json:"id"`
		} `json:"person"`
	}
	query := url.Values{"kind": {kind}, "value": {value}}
	err := d.getJSON(ctx, d.peopleStoreURL+"/identities/resolve?"+query.Encode(), nil, &resolved)
	if errors.Is(err, errNotFoundUpstream) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("people-store: %w", err)
	}
	var person Person
	if err := d.getJSON(ctx, d.peopleStoreURL+"/persons/"+url.PathEscape(resolved.Person.ID), nil, &person); err != nil {
		return nil, fmt.Errorf("people-store: %w", err)
	}
	return &person, nil
}

// resolveCredential reads a secret from auth-store at the time of use, so a
// rotated one is used at once. reason goes in auth-store's audit trail.
func (d *Directory) resolveCredential(ctx context.Context, credentialID, reason string) (string, error) {
	var resolved struct {
		AccessToken string `json:"access_token"`
		APIKey      string `json:"api_key"`
	}
	headers := map[string]string{"Authorization": "Bearer " + d.authStoreToken, "X-Auth-App": ServiceName, "X-Auth-Reason": reason}
	if err := d.getJSON(ctx, d.authStoreURL+"/api/credentials/"+url.PathEscape(credentialID)+"/resolve", headers, &resolved); err != nil {
		return "", fmt.Errorf("auth-store credential %s: %w", credentialID, err)
	}
	if resolved.AccessToken != "" {
		return resolved.AccessToken, nil
	}
	if resolved.APIKey != "" {
		return resolved.APIKey, nil
	}
	return "", fmt.Errorf("auth-store credential %s holds no secret", credentialID)
}

// multichatToken is multichat's bearer.
func (d *Directory) multichatToken(ctx context.Context) (string, error) {
	return d.resolveCredential(ctx, d.multichatCredID, "send a sign-in link through multichat")
}

// PrincipalOfIdentity is the principal an outside identity is bound to in
// principal-store, or ErrNotFound when none is.
func (d *Directory) PrincipalOfIdentity(ctx context.Context, provider, tenantID, subject string) (Principal, error) {
	var resolution struct {
		Principal Principal `json:"principal"`
	}
	query := url.Values{"provider": {provider}, "tenant_id": {tenantID}, "subject": {subject}}
	err := d.getJSON(ctx, d.principalStoreURL+"/identity-bindings/resolve?"+query.Encode(), nil, &resolution)
	if errors.Is(err, errNotFoundUpstream) {
		return Principal{}, fmt.Errorf("%w: no %s identity %s is bound", ErrNotFound, provider, subject)
	}
	if err != nil {
		return Principal{}, fmt.Errorf("principal-store: %w", err)
	}
	if resolution.Principal.ID == "" {
		return Principal{}, fmt.Errorf("principal-store resolved %s identity %s to no principal", provider, subject)
	}
	return resolution.Principal, nil
}

// errIdentityAlreadyBound is principal-store refusing a link because the
// identity is bound to someone already.
var errIdentityAlreadyBound = errors.New("identity already bound")

// LinkIdentity binds an outside identity to a principal in principal-store.
func (d *Directory) LinkIdentity(ctx context.Context, principalID, provider, tenantID, subject, linkedBy, note string) error {
	payload, err := json.Marshal(map[string]string{"principal_id": principalID, "provider": provider, "tenant_id": tenantID,
		"subject": subject, "linked_by": linkedBy, "note": note})
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, d.principalStoreURL+"/identity-bindings", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := d.client.Do(request)
	if err != nil {
		return fmt.Errorf("principal-store: %w", err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
	switch {
	case response.StatusCode == http.StatusConflict:
		return fmt.Errorf("%w: %s", errIdentityAlreadyBound, strings.TrimSpace(string(body)))
	case response.StatusCode/100 != 2:
		return fmt.Errorf("principal-store POST /identity-bindings answered %d: %s", response.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

// directRoomWith finds the one-to-one conversation with a bridged chat
// account: a room of at most three members (the person, the operator and
// the bridge bot) that holds that account.
func (d *Directory) directRoomWith(ctx context.Context, token, chatAccount string) (string, error) {
	var conversations []struct {
		RoomID      string `json:"room_id"`
		MemberCount int    `json:"member_count"`
		Members     []struct {
			UserID string `json:"user_id"`
		} `json:"members"`
	}
	if err := d.getJSON(ctx, d.multichatURL+"/api/conversations", map[string]string{"Authorization": "Bearer " + token}, &conversations); err != nil {
		return "", fmt.Errorf("multichat: %w", err)
	}
	for _, conversation := range conversations {
		if conversation.MemberCount > 3 {
			continue
		}
		for _, member := range conversation.Members {
			if member.UserID == chatAccount {
				return conversation.RoomID, nil
			}
		}
	}
	return "", nil
}

func (d *Directory) send(ctx context.Context, token, roomID, body string) error {
	payload, err := json.Marshal(map[string]string{"body": body})
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, d.multichatURL+"/api/conversations/"+url.PathEscape(roomID)+"/send", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := d.client.Do(request)
	if err != nil {
		return fmt.Errorf("multichat: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode/100 != 2 {
		text, _ := io.ReadAll(io.LimitReader(response.Body, 500))
		return fmt.Errorf("multichat answered %d: %s", response.StatusCode, strings.TrimSpace(string(text)))
	}
	return nil
}

// SendToPersonsChat sends body to the person's one-to-one chat on an account
// whose multichat user id starts with one of chatAccountPrefixes. It reports
// whether a room was found.
func (d *Directory) SendToPersonsChat(ctx context.Context, person *Person, chatAccountPrefixes []string, body string) (bool, error) {
	token, err := d.multichatToken(ctx)
	if err != nil {
		return false, err
	}
	for _, identity := range person.Identities {
		if identity.Kind != "matrix_user" {
			continue
		}
		for _, prefix := range chatAccountPrefixes {
			if !strings.HasPrefix(identity.Value, prefix) {
				continue
			}
			room, err := d.directRoomWith(ctx, token, identity.Value)
			if err != nil {
				return false, err
			}
			if room != "" {
				return true, d.send(ctx, token, room, body)
			}
		}
	}
	return false, nil
}
