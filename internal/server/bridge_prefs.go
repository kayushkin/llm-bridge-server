package server

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"

	"github.com/kayushkin/llm-bridge/msg"
)

// Canonical types — defined in llm-bridge/msg/server.go.
// DO NOT define new request/response types here. Add them to msg/ instead,
// then run generate-ts.sh so the TypeScript frontend stays in sync.
type (
	BridgePrefs     = msg.BridgePrefs
	HarnessDefaults = msg.HarnessDefaults
)

type bridgePrefsStore struct {
	mu   sync.RWMutex
	data BridgePrefs
	path string
}

func newBridgePrefsStore(path string) *bridgePrefsStore {
	s := &bridgePrefsStore{path: path}
	s.load()
	return s
}

func (s *bridgePrefsStore) load() {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("bridge-prefs load error: %v", err)
		}
		return
	}
	if err := json.Unmarshal(data, &s.data); err != nil {
		log.Printf("bridge-prefs parse error: %v", err)
		return
	}
	// One-shot legacy migration: bypass_permissions=true → permission_mode=bypass.
	// Only fires when permission_mode is unset; preserves the user's intent
	// and stops writing the legacy field thereafter.
	if s.data.PermissionMode == "" && s.data.BypassPermissions {
		s.data.PermissionMode = msg.PermissionModeBypass
	}
	// Always drop the legacy field on save (next save will be without it).
	s.data.BypassPermissions = false
}

func (s *bridgePrefsStore) save() {
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		log.Printf("bridge-prefs mkdir error: %v", err)
		return
	}
	data, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		log.Printf("bridge-prefs marshal error: %v", err)
		return
	}
	if err := os.WriteFile(s.path, data, 0644); err != nil {
		log.Printf("bridge-prefs write error: %v", err)
	}
}

func (s *bridgePrefsStore) get() BridgePrefs {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.data
}

// setPermissionMode writes the global permission_mode unambiguously (no
// merge semantics — every call overwrites). Used by the dedicated
// /bridge/permission-mode endpoint, which needs to set any of the three
// values explicitly without the partial-update logic in set() blanking
// the field when callers send an empty string.
func (s *bridgePrefsStore) setPermissionMode(mode string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.PermissionMode = mode
	s.data.BypassPermissions = false // legacy field stops being written
	s.save()
}

func (s *bridgePrefsStore) set(prefs BridgePrefs) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if prefs.LastHarness != "" {
		s.data.LastHarness = prefs.LastHarness
	}
	if prefs.LastSession != nil {
		if s.data.LastSession == nil {
			s.data.LastSession = make(map[string]string)
		}
		for k, v := range prefs.LastSession {
			if v == "" {
				delete(s.data.LastSession, k)
			} else {
				s.data.LastSession[k] = v
			}
		}
	}
	if prefs.Defaults != nil {
		if s.data.Defaults == nil {
			s.data.Defaults = make(map[string]HarnessDefaults)
		}
		for k, v := range prefs.Defaults {
			s.data.Defaults[k] = v
		}
	}
	s.save()
}

func (s *Server) handleBridgePrefs(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, s.bridgePrefs.get())
	case http.MethodPut:
		var prefs BridgePrefs
		if err := json.NewDecoder(r.Body).Decode(&prefs); err != nil {
			http.Error(w, `{"error":"invalid request"}`, http.StatusBadRequest)
			return
		}
		s.bridgePrefs.set(prefs)
		writeJSON(w, map[string]string{"status": "ok"})
	default:
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
	}
}
