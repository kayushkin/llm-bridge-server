package aiauth

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// AuthStore is the on-disk format for auth-profiles.json.
type AuthStore struct {
	Version    int                       `json:"version"`
	Profiles   map[string]*Credential    `json:"profiles"`
	LastGood   map[string]string         `json:"lastGood,omitempty"`
	UsageStats map[string]*UsageStats    `json:"usageStats,omitempty"`
}

// UsageStats tracks per-profile usage.
type UsageStats struct {
	LastUsed      int64 `json:"lastUsed,omitempty"`
	ErrorCount    int   `json:"errorCount,omitempty"`
	LastFailureAt int64 `json:"lastFailureAt,omitempty"`
}

// Store manages reading/writing auth profiles.
type Store struct {
	mu   sync.Mutex
	path string
	data *AuthStore
}

// DefaultStore loads from the default OpenClaw auth-profiles.json path.
func DefaultStore() *Store {
	home, err := os.UserHomeDir()
	if err != nil {
		return &Store{path: "", data: &AuthStore{Version: 1, Profiles: make(map[string]*Credential)}}
	}
	p := filepath.Join(home, ".openclaw", "agents", "main", "agent", "auth-profiles.json")
	s, _ := NewStore(p)
	return s
}

// NewStore loads auth profiles from the given path.
func NewStore(path string) (*Store, error) {
	s := &Store{
		path: path,
		data: &AuthStore{Version: 1, Profiles: make(map[string]*Credential)},
	}
	if err := s.load(); err != nil && !os.IsNotExist(err) {
		return s, err
	}
	return s, nil
}

// Path returns the store file path.
func (s *Store) Path() string { return s.path }

// Profiles returns all profiles (not a copy — do not modify without lock).
func (s *Store) Profiles() map[string]*Credential {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.data.Profiles
}

// SetProfile adds or updates a profile and saves.
func (s *Store) SetProfile(name string, cred *Credential) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.data.Profiles == nil {
		s.data.Profiles = make(map[string]*Credential)
	}
	s.data.Profiles[name] = cred
	return s.save()
}

// DeleteProfile removes a profile and saves.
func (s *Store) DeleteProfile(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.data.Profiles == nil {
		return nil
	}
	delete(s.data.Profiles, name)
	return s.save()
}

// ProfilesForProvider returns all credentials for a given provider, sorted by priority.
func (s *Store) ProfilesForProvider(provider string) []*Credential {
	s.mu.Lock()
	defer s.mu.Unlock()

	var oauth, tokens, apiKeys []*Credential
	for _, c := range s.data.Profiles {
		if c.Provider != provider {
			continue
		}
		switch c.Type {
		case "oauth":
			oauth = append(oauth, c)
		case "token":
			tokens = append(tokens, c)
		case "api_key":
			apiKeys = append(apiKeys, c)
		}
	}
	result := make([]*Credential, 0, len(oauth)+len(tokens)+len(apiKeys))
	result = append(result, oauth...)
	result = append(result, tokens...)
	result = append(result, apiKeys...)
	return result
}

func (s *Store) load() error {
	data, err := os.ReadFile(s.path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, s.data)
}

func (s *Store) save() error {
	data, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	return os.WriteFile(s.path, data, 0600)
}

// Reload re-reads the store from disk.
func (s *Store) Reload() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.load()
}

// UpdateProfile updates a profile in-place and saves. Thread-safe.
func (s *Store) UpdateProfile(name string, cred *Credential) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.Profiles[name] = cred
	return s.save()
}

// FindProfileName returns the profile name for a credential pointer.
func (s *Store) FindProfileName(cred *Credential) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	for name, c := range s.data.Profiles {
		if c == cred {
			return name
		}
	}
	return fmt.Sprintf("%s:unknown", cred.Provider)
}
