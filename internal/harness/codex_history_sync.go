package harness

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sync"

	"github.com/kayushkin/llm-bridge-server/internal/childprocessenv"
	"github.com/kayushkin/llm-bridge/msg"
)

type codexHistorySync struct {
	mu       sync.Mutex
	revision string
}

// SyncCodexHistory refreshes a discovered local transcript. Bridge-managed
// sessions already have an event pump and must never be re-imported here.
func (m *Manager) SyncCodexHistory(ctx context.Context, bridgeID string) (int, error) {
	sess, err := m.store.GetSession(bridgeID)
	if errors.Is(err, sql.ErrNoRows) {
		// Not a bridge session (e.g. archived, served from log-store only):
		// there is no local transcript to sync.
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if sess.Harness != msg.HarnessCodex || sess.Origin != msg.OriginDiscovery || sess.HarnessSessionID == "" {
		return 0, nil
	}
	m.mu.RLock()
	_, running := m.processes[bridgeID]
	m.mu.RUnlock()
	if running {
		return 0, nil
	}
	entry, _ := m.codexHistorySyncs.LoadOrStore(bridgeID, &codexHistorySync{})
	state := entry.(*codexHistorySync)
	state.mu.Lock()
	defer state.mu.Unlock()
	bin, ok := Available(msg.HarnessCodex)
	if !ok {
		return 0, fmt.Errorf("Codex history adapter unavailable")
	}
	run := func(flag string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, bin, flag, sess.HarnessSessionID)
		cmd.Env = childprocessenv.EnvironmentWithoutServerSecrets()
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			return nil, fmt.Errorf("Codex %s: %w: %s", flag, err, stderr.String())
		}
		return out, nil
	}
	revision, err := run("-history-revision")
	if err != nil {
		return 0, err
	}
	if state.revision == string(revision) {
		return 0, nil
	}
	// Refuse a second destination for an already imported native transcript.
	held, err := m.logStore.SessionsHoldingHarnessSessionID(sess.HarnessSessionID)
	if err != nil {
		return 0, err
	}
	for _, holder := range held {
		if holder.EventCount > 0 && holder.SessionID != bridgeID {
			return 0, fmt.Errorf("Codex transcript already belongs to %s", holder.SessionID)
		}
	}
	existing, err := m.logStore.ListEvents(bridgeID, 0, nil)
	if err != nil {
		return 0, err
	}
	output, err := run("-import-history")
	if err != nil {
		return 0, err
	}
	pending, err := codexHistorySuffix(existing, output, sess.HarnessSessionID)
	if err != nil {
		return 0, err
	}
	for i := range pending {
		ev := &pending[i]
		ev.BridgeSessionID = bridgeID
		// Make the canonical transcript durable first. If an acknowledgement is
		// lost, the next attempt re-reads log-store and skips this exact record.
		if _, err := m.logStore.PushEvent(*ev); err != nil {
			return i, err
		}
		data, err := json.Marshal(ev)
		if err != nil {
			return i + 1, err
		}
		rowID, err := m.store.StoreEventReturningID(bridgeID, string(ev.Type), ev.MessageID, ev.HarnessMessageID, data)
		if err != nil {
			return i + 1, err
		}
		// History must not trigger agents, tasks, status derivation or automation.
		m.mu.RLock()
		for _, ch := range m.subscribers[bridgeID] {
			select {
			case ch <- StoredEvent{Event: *ev, RowID: rowID}:
			default:
			}
		}
		m.mu.RUnlock()
	}
	state.revision = string(revision)
	return len(pending), nil
}

// Compare the entire durable prefix, including repeated identical records.
// Counts/hashes alone could silently accept an edited or truncated rollout.
func codexHistorySuffix(stored []json.RawMessage, output []byte, nativeID string) ([]msg.Event, error) {
	compact := func(raw json.RawMessage) (string, error) {
		var b bytes.Buffer
		if err := json.Compact(&b, raw); err != nil {
			return "", err
		}
		return b.String(), nil
	}
	var prefix []string
	for _, data := range stored {
		var ev msg.Event
		if err := json.Unmarshal(data, &ev); err != nil {
			return nil, err
		}
		if ev.HarnessSessionID != nativeID || len(ev.Raw) == 0 {
			continue
		}
		raw, err := compact(ev.Raw)
		if err != nil {
			return nil, err
		}
		prefix = append(prefix, raw)
	}
	var pending []msg.Event
	reader := bufio.NewReader(bytes.NewReader(output))
	index := 0
	for {
		line, err := reader.ReadBytes('\n')
		if err != nil && err != io.EOF {
			return nil, err
		}
		if len(bytes.TrimSpace(line)) > 0 {
			var ev msg.Event
			if err := json.Unmarshal(line, &ev); err != nil {
				return nil, err
			}
			if ev.HarnessSessionID != nativeID || len(ev.Raw) == 0 {
				return nil, fmt.Errorf("Codex import record has missing or mismatched provenance")
			}
			raw, e := compact(ev.Raw)
			if e != nil {
				return nil, e
			}
			if index < len(prefix) {
				if prefix[index] != raw {
					return nil, fmt.Errorf("Codex rollout differs from stored record %d; refusing to duplicate or overwrite history", index+1)
				}
			} else {
				pending = append(pending, ev)
			}
			index++
		}
		if err == io.EOF {
			break
		}
	}
	if index < len(prefix) {
		return nil, fmt.Errorf("Codex rollout truncated: %d records, %d already stored", index, len(prefix))
	}
	return pending, nil
}
