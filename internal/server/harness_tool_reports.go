package server

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/agents/tools"
	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
)

// onSessionInfo forwards the built-in tools a harness reports at session
// start to the tool-store module (RecordObservedHarnessTools, what its
// POST /harness-tools/observed answers). tool-store records when
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
	s.holdRestrictedSessionToItsAllowList(sess, names)
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

// holdRestrictedSessionToItsAllowList stops a restricted session whose harness
// reports a built-in tool that is not on the allow list pinned at create. The
// harness was told not to offer it (--tools), so a report means the harness did
// not obey. The report arrives once the process has started, so this is a
// second line, not the first: it ends the session rather than let it go on.
func (s *Server) holdRestrictedSessionToItsAllowList(sess *store.Session, reported []string) {
	cfg, err := parseHarnessConfig(sess)
	if err != nil {
		log.Printf("[harness-tools] ERROR session %s: harness_config unreadable, stopping it because its allow list cannot be checked: %v", sess.SessionID, err)
		s.stopSessionOffItsAllowList(sess.SessionID)
		return
	}
	allowed, err := builtInToolAllowListPinnedOn(cfg)
	if err != nil {
		log.Printf("[harness-tools] ERROR session %s: stopping it because its allow list cannot be read: %v", sess.SessionID, err)
		s.stopSessionOffItsAllowList(sess.SessionID)
		return
	}
	if allowed == nil {
		return
	}
	onList := map[string]bool{}
	for _, name := range allowed {
		onList[name] = true
	}
	var offList []string
	for _, name := range reported {
		if !onList[name] {
			offList = append(offList, name)
		}
	}
	if len(offList) == 0 {
		return
	}
	log.Printf("[harness-tools] ERROR session %s (%s): its bundle restricts it to %v, and the harness reported %v as well; stopping the session", sess.SessionID, sess.Harness, allowed, offList)
	s.stopSessionOffItsAllowList(sess.SessionID)
}

func (s *Server) stopSessionOffItsAllowList(sessionID string) {
	if err := s.stopSession(sessionID); err != nil {
		log.Printf("[harness-tools] ERROR session %s: stopping it failed: %v", sessionID, err)
	}
}

// reportObservedHarnessTools hands names to tool-store and returns the names
// it registered for the first time.
func (s *Server) reportObservedHarnessTools(ctx context.Context, harness msg.Harness, names []string) ([]string, error) {
	if s.toolRegistry == nil {
		return nil, fmt.Errorf("%s", toolStoreOffMessage)
	}
	answer, err := s.toolRegistry.RecordObservedHarnessTools(ctx, tools.ObservedHarnessToolsRequest{Harness: string(harness), ToolNames: names})
	if err != nil {
		return nil, fmt.Errorf("tool-store could not record the harness tools %s reported: %w", harness, err)
	}
	return answer.Created, nil
}
