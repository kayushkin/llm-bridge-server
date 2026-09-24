package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

// onSessionInfo forwards the built-in tools a harness reports at session
// start to tool-store's POST /harness-tools/observed. tool-store records when
// each was last seen and registers a name it has never seen as a harness tool
// that is switched off and tagged unreviewed; every later session then gets it
// in disabled_tools until a person reviews it. MCP tools (mcp__…) are
// tool-store's own rows already and are not reported.
//
// It runs on its own goroutine after the info is stored. A failure cannot stop
// a session that has already started, so it is logged loudly instead.
func (s *Server) onSessionInfo(bridgeID string, info *msg.SessionInfo) {
	if info == nil || len(info.Tools) == 0 {
		return
	}
	sess, err := s.store.GetSession(bridgeID)
	if err != nil {
		log.Printf("[harness-tools] session %s reported %d tools but its row could not be read, so they were not reported to tool-store: %v", bridgeID, len(info.Tools), err)
		return
	}
	names := make([]string, 0, len(info.Tools))
	for _, tool := range info.Tools {
		if tool.Name != "" && !strings.HasPrefix(tool.Name, "mcp__") {
			names = append(names, tool.Name)
		}
	}
	if len(names) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	created, err := s.reportObservedHarnessTools(ctx, sess.Harness, names)
	if err != nil {
		log.Printf("[harness-tools] ERROR session %s (%s): reporting %d tools to tool-store failed; a tool this harness added will not be registered or switched off: %v", bridgeID, sess.Harness, len(names), err)
		return
	}
	if len(created) > 0 {
		log.Printf("[harness-tools] session %s (%s) reported tools tool-store had never seen: %v; they are registered switched off until reviewed, and this session already has them", bridgeID, sess.Harness, created)
	}
}

// reportObservedHarnessTools posts names to tool-store and returns the names
// it registered for the first time.
func (s *Server) reportObservedHarnessTools(ctx context.Context, harness msg.Harness, names []string) ([]string, error) {
	if s.cfg == nil || s.cfg.ToolStoreURL == "" {
		return nil, fmt.Errorf("this server has no tool-store (LLMBRIDGE_TOOL_STORE_URL)")
	}
	payload, err := json.Marshal(map[string]any{"harness": string(harness), "tool_names": names})
	if err != nil {
		return nil, err
	}
	requestURL := s.cfg.ToolStoreURL + "/harness-tools/observed"
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, requestURL, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("build POST %s: %w", requestURL, err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := (&http.Client{Timeout: 10 * time.Second}).Do(request)
	if err != nil {
		return nil, fmt.Errorf("tool-store unreachable: POST %s: %w", requestURL, err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, fmt.Errorf("read tool-store response: %w", err)
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("tool-store POST %s returned %d: %s", requestURL, response.StatusCode, strings.TrimSpace(string(body)))
	}
	var answer struct {
		Created []string `json:"created"`
	}
	if err := json.Unmarshal(body, &answer); err != nil {
		return nil, fmt.Errorf("tool-store POST %s answered unparseable JSON: %w", requestURL, err)
	}
	return answer.Created, nil
}
