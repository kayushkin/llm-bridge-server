package sessions

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"

	"github.com/kayushkin/llm-bridge-server/internal/httpjson"

	"github.com/kayushkin/llm-bridge/msg"
)

// Canonical types — defined in llm-bridge/msg/server.go.
// DO NOT define new request/response types here. Add them to msg/ instead,
// then run generate-ts.sh so the TypeScript frontend stays in sync.
type (
	BridgePrefs     = msg.BridgePrefs
	HarnessDefaults = msg.HarnessDefaults
)

type BridgePrefsStore struct {
	Mu   sync.RWMutex
	Data BridgePrefs
	path string
}

func NewBridgePrefsStore(path string) *BridgePrefsStore {
	s := &BridgePrefsStore{path: path}
	s.load()
	return s
}

func (s *BridgePrefsStore) load() {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("bridge-prefs load error: %v", err)
		}
		return
	}
	if err := json.Unmarshal(data, &s.Data); err != nil {
		log.Printf("bridge-prefs parse error: %v", err)
		return
	}
	// One-shot legacy migration: bypass_permissions=true → permission_mode=bypass.
	// Only fires when permission_mode is unset; preserves the user's intent
	// and stops writing the legacy field thereafter.
	if s.Data.PermissionMode == "" && s.Data.BypassPermissions {
		s.Data.PermissionMode = msg.PermissionModeBypass
	}
	// Always drop the legacy field on save (next save will be without it).
	s.Data.BypassPermissions = false
}

func (s *BridgePrefsStore) save() {
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		log.Printf("bridge-prefs mkdir error: %v", err)
		return
	}
	data, err := json.MarshalIndent(s.Data, "", "  ")
	if err != nil {
		log.Printf("bridge-prefs marshal error: %v", err)
		return
	}
	if err := os.WriteFile(s.path, data, 0644); err != nil {
		log.Printf("bridge-prefs write error: %v", err)
	}
}

func (s *BridgePrefsStore) Get() BridgePrefs {
	s.Mu.RLock()
	defer s.Mu.RUnlock()
	return s.Data
}

// SetPermissionMode writes the global permission_mode unambiguously (no
// merge semantics — every call overwrites). Used by the dedicated
// /bridge/permission-mode endpoint, which needs to set any of the three
// values explicitly without the partial-update logic in set() blanking
// the field when callers send an empty string.
func (s *BridgePrefsStore) SetPermissionMode(mode string) {
	s.Mu.Lock()
	defer s.Mu.Unlock()
	s.Data.PermissionMode = mode
	s.Data.BypassPermissions = false // legacy field stops being written
	s.save()
}

// set folds a partial record onto the stored one. Every string field here
// treats "" as "not sent", which is what lets a caller PUT one field without
// blanking the rest.
func (s *BridgePrefsStore) set(prefs BridgePrefs) {
	s.Mu.Lock()
	defer s.Mu.Unlock()
	if prefs.LastHarness != "" {
		s.Data.LastHarness = prefs.LastHarness
	}
	if prefs.LastInstanceID != "" {
		s.Data.LastInstanceID = prefs.LastInstanceID
	}
	if prefs.LastInstance != nil {
		if s.Data.LastInstance == nil {
			s.Data.LastInstance = make(map[string]string)
		}
		for k, v := range prefs.LastInstance {
			if v == "" {
				delete(s.Data.LastInstance, k)
			} else {
				s.Data.LastInstance[k] = v
			}
		}
	}
	if prefs.LastSession != nil {
		if s.Data.LastSession == nil {
			s.Data.LastSession = make(map[string]string)
		}
		for k, v := range prefs.LastSession {
			if v == "" {
				delete(s.Data.LastSession, k)
			} else {
				s.Data.LastSession[k] = v
			}
		}
	}
	if prefs.Defaults != nil {
		if s.Data.Defaults == nil {
			s.Data.Defaults = make(map[string]HarnessDefaults)
		}
		for k, v := range prefs.Defaults {
			s.Data.Defaults[k] = v
		}
	}
	s.save()
}

func (s *Service) HandleBridgePrefs(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		httpjson.WriteJSON(w, s.BridgePrefs.Get())
	case http.MethodPut:
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, `{"error":"invalid request"}`, http.StatusBadRequest)
			return
		}
		var prefs BridgePrefs
		if err := json.Unmarshal(body, &prefs); err != nil {
			http.Error(w, `{"error":"invalid request"}`, http.StatusBadRequest)
			return
		}
		s.BridgePrefs.set(prefs)
		httpjson.WriteJSON(w, map[string]string{"status": "ok"})
	default:
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
	}
}
