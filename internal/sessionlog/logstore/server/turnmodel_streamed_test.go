package server

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/sessionlog/logstore/store"
	"github.com/kayushkin/llm-bridge/msg"
)

// textDelta is one streamed fragment of an assistant message's text, as
// llm-bridge-codex emits it for an agent message: a stream event whose delta is a
// text_delta, stamped with the message's canonical id.
func textDelta(t *testing.T, id int64, ts time.Time, messageID, text string) store.EventRow {
	t.Helper()
	return mkRow(t, id, msg.Event{
		Type:      msg.EventStream,
		TurnID:    "turn1",
		MessageID: messageID,
		Timestamp: ts,
		Stream:    &msg.HarnessStream{Delta: &msg.BlockDelta{Type: msg.DeltaText, Text: text}},
	})
}

func toolCall(t *testing.T, id int64, ts time.Time, messageID, toolID string) store.EventRow {
	t.Helper()
	return mkRow(t, id, msg.Event{
		Type: msg.EventToolCall, TurnID: "turn1", MessageID: messageID, Timestamp: ts,
		ToolCall: &msg.ToolCallEvent{ToolID: toolID, Name: "command_execution", Input: json.RawMessage(`{"command":"ls"}`)},
	})
}

func toolResult(t *testing.T, id int64, ts time.Time, messageID, toolID string) store.EventRow {
	t.Helper()
	return mkRow(t, id, msg.Event{
		Type: msg.EventToolResult, TurnID: "turn1", MessageID: messageID, Timestamp: ts,
		ToolResult: &msg.ToolResultEvent{ToolID: toolID, Name: "command_execution", Output: "ok"},
	})
}

// visibleText lists the text of every entry the collapsed view draws as assistant
// prose, in turn order.
func visibleText(m TurnModel) []string {
	var out []string
	for _, turn := range m.Turns {
		for _, id := range turn.EntryIDs {
			e := m.Entries[id]
			if !e.Duplicate && e.Role == "assistant" && (e.Kind == "text" || e.Kind == "result") && e.Text != "" {
				out = append(out, e.Kind+": "+e.Text)
			}
		}
	}
	return out
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// The fault. A Codex turn: the agent says what it is about to do, runs a command,
// then answers. Its words arrive as stream deltas and no block is ever sent; only
// the answer is repeated by the result. The page used to carry the result alone,
// so the first message was drawn while a browser watched it arrive and was gone
// on a reload. It must now be one visible text entry, whole, where it began.
func TestBuildTurnModel_StreamedMessageBeforeAToolIsOneVisibleEntry(t *testing.T) {
	base := time.Date(2026, 11, 17, 21, 17, 45, 0, time.UTC)
	rows := []store.EventRow{
		mkRow(t, 1, msg.Event{Type: msg.EventUserMessage, TurnID: "turn1", MessageID: "msg_user", Timestamp: base, Result: &msg.ResultEvent{Text: "show me the comparison"}}),
		textDelta(t, 2, base.Add(6*time.Second), "msg_note", "I’ll split"),
		// Bookkeeping lands between fragments on the real wire.
		mkRow(t, 3, msg.Event{Type: msg.EventSessionStatus, TurnID: "turn1", Timestamp: base.Add(6 * time.Second)}),
		textDelta(t, 4, base.Add(6*time.Second), "msg_note", " the comparison"),
		textDelta(t, 5, base.Add(7*time.Second), "msg_note", " into one image per asset."),
		mkRow(t, 6, msg.Event{Type: msg.EventSystem, TurnID: "turn1", Timestamp: base.Add(7 * time.Second), System: &msg.SystemEvent{Subtype: "agent_message_complete", Message: "I’ll split the comparison into one image per asset."}}),
		toolCall(t, 7, base.Add(21*time.Second), "msg_tool", "exec-1"),
		toolResult(t, 8, base.Add(22*time.Second), "msg_tool", "exec-1"),
		textDelta(t, 9, base.Add(44*time.Second), "msg_answer", "Each image shows"),
		textDelta(t, 10, base.Add(45*time.Second), "msg_answer", " five versions."),
		mkRow(t, 11, msg.Event{Type: msg.EventResult, TurnID: "turn1", MessageID: "msg_answer", Timestamp: base.Add(52 * time.Second), Result: &msg.ResultEvent{Text: "Each image shows five versions."}}),
	}
	m := buildTurnModel("s", rows, false)

	first := m.Entries["e_2"]
	if first.Duplicate || !first.Primary {
		t.Fatalf("the message's first delta must be visible; got primary=%v duplicate=%v", first.Primary, first.Duplicate)
	}
	if want := "I’ll split the comparison into one image per asset."; first.Text != want {
		t.Errorf("first delta text = %q, want the whole message %q", first.Text, want)
	}
	if first.Kind != "text" || first.Role != "assistant" || first.MessageID != "msg_note" {
		t.Errorf("first delta is %s/%s of message %q, want assistant/text of msg_note", first.Role, first.Kind, first.MessageID)
	}
	// The other fragments stay hidden and keep their own text: one event, one entry.
	for id, text := range map[string]string{"e_4": " the comparison", "e_5": " into one image per asset."} {
		if e := m.Entries[id]; !e.Duplicate || e.Primary || e.Text != text {
			t.Errorf("%s must stay a hidden fragment %q; got primary=%v duplicate=%v text=%q", id, text, e.Primary, e.Duplicate, e.Text)
		}
	}
	// The answer is the result alone. Its deltas say the same words.
	for _, id := range []string{"e_9", "e_10"} {
		if e := m.Entries[id]; !e.Duplicate || e.Primary {
			t.Errorf("%s: a delta of the message the result repeats must stay hidden; got primary=%v duplicate=%v", id, e.Primary, e.Duplicate)
		}
	}
	want := []string{
		"text: I’ll split the comparison into one image per asset.",
		"result: Each image shows five versions.",
	}
	if got := visibleText(m); !sameStrings(got, want) {
		t.Errorf("collapsed view reads %q, want %q", got, want)
	}
}

// What a reader is sent. The page drops hidden entries, so the streamed message
// must reach it as exactly one entry, between the prompt and the tool it preceded.
func TestProjectForReading_CarriesAStreamedMessageAsOneEntryInPlace(t *testing.T) {
	base := time.Date(2026, 11, 17, 21, 17, 45, 0, time.UTC)
	rows := []store.EventRow{
		mkRow(t, 1, msg.Event{Type: msg.EventUserMessage, TurnID: "turn1", MessageID: "msg_user", Timestamp: base, Result: &msg.ResultEvent{Text: "q"}}),
		textDelta(t, 2, base.Add(time.Second), "msg_note", "Checking"),
		textDelta(t, 3, base.Add(time.Second), "msg_note", " the files."),
		toolCall(t, 4, base.Add(2*time.Second), "msg_tool", "exec-1"),
		toolResult(t, 5, base.Add(3*time.Second), "msg_tool", "exec-1"),
		textDelta(t, 6, base.Add(4*time.Second), "msg_answer", "Done."),
		mkRow(t, 7, msg.Event{Type: msg.EventResult, TurnID: "turn1", MessageID: "msg_answer", Timestamp: base.Add(5 * time.Second), Result: &msg.ResultEvent{Text: "Done."}}),
	}
	page := projectForReading(buildTurnModel("s", rows, false))

	if len(page.Turns) != 1 {
		t.Fatalf("page has %d turns, want 1", len(page.Turns))
	}
	want := []string{"e_1", "e_2", "e_4", "e_5", "e_7"}
	if got := page.Turns[0].EntryIDs; !sameStrings(got, want) {
		t.Fatalf("page entries = %v, want %v", got, want)
	}
	if got := page.Entries["e_2"].Text; got != "Checking the files." {
		t.Errorf("streamed message reads %q on the page, want %q", got, "Checking the files.")
	}
}

// A page read in the middle of a turn must not report half a message: chat-core
// drops its own live copy of whatever a page reports, and the live copy is the
// fuller one. Until something follows the message, it stays off the page.
func TestBuildTurnModel_StreamedMessageStillBeingWrittenStaysHidden(t *testing.T) {
	base := time.Date(2026, 11, 17, 21, 17, 45, 0, time.UTC)
	rows := []store.EventRow{
		mkRow(t, 1, msg.Event{Type: msg.EventUserMessage, TurnID: "turn1", MessageID: "msg_user", Timestamp: base, Result: &msg.ResultEvent{Text: "q"}}),
		textDelta(t, 2, base.Add(time.Second), "msg_note", "I’ll split"),
		textDelta(t, 3, base.Add(time.Second), "msg_note", " the comp"),
		// Bookkeeping after the last fragment says nothing about the message.
		mkRow(t, 4, msg.Event{Type: msg.EventSessionStatus, TurnID: "turn1", Timestamp: base.Add(time.Second)}),
		mkRow(t, 5, msg.Event{Type: msg.EventSystem, TurnID: "turn1", Timestamp: base.Add(time.Second), System: &msg.SystemEvent{Subtype: "item_started", Message: "agentMessage"}}),
	}
	m := buildTurnModel("s", rows, false)
	for _, id := range []string{"e_2", "e_3"} {
		if e := m.Entries[id]; !e.Duplicate || e.Primary {
			t.Errorf("%s: a message still being written must stay hidden; got primary=%v duplicate=%v", id, e.Primary, e.Duplicate)
		}
	}
	if e := m.Entries["e_2"]; e.Text != "I’ll split" {
		t.Errorf("an unfinished message's first delta must keep its own text; got %q", e.Text)
	}
}

// A tool begun before the message can report while the message is still being
// written. That does not finish the message; an entry of a message that began
// after it does.
func TestBuildTurnModel_EarlierToolReportingDoesNotFinishAStreamedMessage(t *testing.T) {
	base := time.Date(2026, 11, 17, 21, 17, 45, 0, time.UTC)
	rows := []store.EventRow{
		mkRow(t, 1, msg.Event{Type: msg.EventUserMessage, TurnID: "turn1", MessageID: "msg_user", Timestamp: base, Result: &msg.ResultEvent{Text: "q"}}),
		toolCall(t, 2, base.Add(time.Second), "msg_slow_tool", "exec-1"),
		textDelta(t, 3, base.Add(2*time.Second), "msg_note", "While that runs"),
		toolResult(t, 4, base.Add(3*time.Second), "msg_slow_tool", "exec-1"),
	}
	m := buildTurnModel("s", rows, false)
	if e := m.Entries["e_3"]; !e.Duplicate || e.Primary {
		t.Fatalf("a result of a tool begun earlier must not finish the message; got primary=%v duplicate=%v", e.Primary, e.Duplicate)
	}

	rows = append(rows,
		textDelta(t, 5, base.Add(4*time.Second), "msg_note", ", I’ll read the log."),
		toolCall(t, 6, base.Add(5*time.Second), "msg_next_tool", "exec-2"),
	)
	m = buildTurnModel("s", rows, false)
	if e := m.Entries["e_3"]; e.Duplicate || !e.Primary || e.Text != "While that runs, I’ll read the log." {
		t.Errorf("a tool begun after the message finishes it; got primary=%v duplicate=%v text=%q", e.Primary, e.Duplicate, e.Text)
	}
}

// A turn that fails after the agent spoke: no result ever repeats the words, and
// the error ends the turn, so the streamed message is the only record of them.
func TestBuildTurnModel_StreamedMessageShowsWhenTheTurnEndsInAnError(t *testing.T) {
	base := time.Date(2026, 11, 17, 21, 17, 45, 0, time.UTC)
	rows := []store.EventRow{
		mkRow(t, 1, msg.Event{Type: msg.EventUserMessage, TurnID: "turn1", MessageID: "msg_user", Timestamp: base, Result: &msg.ResultEvent{Text: "q"}}),
		textDelta(t, 2, base.Add(time.Second), "msg_partial", "The build is"),
		textDelta(t, 3, base.Add(time.Second), "msg_partial", " failing because"),
		mkRow(t, 4, msg.Event{Type: msg.EventError, TurnID: "turn1", MessageID: "msg_partial", Timestamp: base.Add(2 * time.Second), Error: &msg.ErrorEvent{Code: "TURN_FAILED", Message: "turn failed"}}),
	}
	m := buildTurnModel("s", rows, false)
	if e := m.Entries["e_2"]; e.Duplicate || !e.Primary || e.Text != "The build is failing because" {
		t.Errorf("the words before the error must show; got primary=%v duplicate=%v text=%q", e.Primary, e.Duplicate, e.Text)
	}
}

// A turn whose result carries no text ends the turn all the same, and repeats
// nothing, so the streamed message it ends on shows.
func TestBuildTurnModel_StreamedMessageShowsWhenItsResultHasNoText(t *testing.T) {
	base := time.Date(2026, 11, 17, 21, 17, 45, 0, time.UTC)
	rows := []store.EventRow{
		textDelta(t, 1, base, "msg_only", "Stopped here."),
		mkRow(t, 2, msg.Event{Type: msg.EventResult, TurnID: "turn1", MessageID: "msg_only", Timestamp: base.Add(time.Second), Result: &msg.ResultEvent{Text: ""}}),
	}
	m := buildTurnModel("s", rows, false)
	if e := m.Entries["e_1"]; e.Duplicate || !e.Primary {
		t.Errorf("a result with no text repeats nothing, so the message must show; got primary=%v duplicate=%v", e.Primary, e.Duplicate)
	}
}

// A harness that streams a message and then sends it as a block has sent it whole.
// The block rules decide what shows, and the deltas must not show beside it.
func TestBuildTurnModel_StreamedMessageWithABlockLeavesItToTheBlock(t *testing.T) {
	base := time.Date(2026, 11, 17, 21, 17, 45, 0, time.UTC)
	rows := []store.EventRow{
		mkRow(t, 1, msg.Event{Type: msg.EventUserMessage, TurnID: "turn1", MessageID: "msg_user", Timestamp: base, Result: &msg.ResultEvent{Text: "q"}}),
		textDelta(t, 2, base.Add(time.Second), "msg_narr", "Now the"),
		textDelta(t, 3, base.Add(time.Second), "msg_narr", " change:"),
		narrationTextBlock(t, 4, base.Add(2*time.Second), "msg_narr", "Now the change:"),
		toolCall(t, 5, base.Add(3*time.Second), "msg_narr", "t1"),
		mkRow(t, 6, msg.Event{Type: msg.EventResult, TurnID: "turn1", MessageID: "msg_answer", Timestamp: base.Add(4 * time.Second), Result: &msg.ResultEvent{Text: "done"}}),
	}
	m := buildTurnModel("s", rows, false)
	for _, id := range []string{"e_2", "e_3"} {
		if e := m.Entries[id]; !e.Duplicate || e.Primary {
			t.Errorf("%s: a delta of a message that has a block must stay hidden; got primary=%v duplicate=%v", id, e.Primary, e.Duplicate)
		}
	}
	want := []string{"text: Now the change:", "result: done"}
	if got := visibleText(m); !sameStrings(got, want) {
		t.Errorf("collapsed view reads %q, want %q", got, want)
	}
}

// Streamed reasoning is not prose. It must not be joined into a visible text entry.
func TestBuildTurnModel_StreamedReasoningIsNotSurfacedAsProse(t *testing.T) {
	base := time.Date(2026, 11, 17, 21, 17, 45, 0, time.UTC)
	rows := []store.EventRow{
		mkRow(t, 1, msg.Event{
			Type: msg.EventStream, TurnID: "turn1", MessageID: "msg_think", Timestamp: base,
			Stream: &msg.HarnessStream{Delta: &msg.BlockDelta{Type: msg.DeltaThinking, Thinking: "weighing the options"}},
		}),
		toolCall(t, 2, base.Add(time.Second), "msg_tool", "exec-1"),
		mkRow(t, 3, msg.Event{Type: msg.EventResult, TurnID: "turn1", MessageID: "msg_tool", Timestamp: base.Add(2 * time.Second), Result: &msg.ResultEvent{Text: "done"}}),
	}
	m := buildTurnModel("s", rows, false)
	if e := m.Entries["e_1"]; !e.Duplicate || e.Primary || e.Kind != "thinking" {
		t.Errorf("streamed reasoning must stay hidden thinking; got kind=%q primary=%v duplicate=%v", e.Kind, e.Primary, e.Duplicate)
	}
}

// Each streamed message is its own entry: two messages before two tools are two
// entries, each where its message began.
func TestBuildTurnModel_EachStreamedMessageIsItsOwnEntry(t *testing.T) {
	base := time.Date(2026, 11, 17, 21, 17, 45, 0, time.UTC)
	rows := []store.EventRow{
		mkRow(t, 1, msg.Event{Type: msg.EventUserMessage, TurnID: "turn1", MessageID: "msg_user", Timestamp: base, Result: &msg.ResultEvent{Text: "q"}}),
		textDelta(t, 2, base.Add(time.Second), "msg_first", "Reading the config."),
		toolCall(t, 3, base.Add(2*time.Second), "msg_tool_1", "exec-1"),
		toolResult(t, 4, base.Add(3*time.Second), "msg_tool_1", "exec-1"),
		textDelta(t, 5, base.Add(4*time.Second), "msg_second", "Now"),
		textDelta(t, 6, base.Add(4*time.Second), "msg_second", " the tests."),
		toolCall(t, 7, base.Add(5*time.Second), "msg_tool_2", "exec-2"),
		toolResult(t, 8, base.Add(6*time.Second), "msg_tool_2", "exec-2"),
		textDelta(t, 9, base.Add(7*time.Second), "msg_answer", "All pass."),
		mkRow(t, 10, msg.Event{Type: msg.EventResult, TurnID: "turn1", MessageID: "msg_answer", Timestamp: base.Add(8 * time.Second), Result: &msg.ResultEvent{Text: "All pass."}}),
	}
	page := projectForReading(buildTurnModel("s", rows, false))
	want := []string{"e_1", "e_2", "e_3", "e_4", "e_5", "e_7", "e_8", "e_10"}
	if got := page.Turns[0].EntryIDs; !sameStrings(got, want) {
		t.Fatalf("page entries = %v, want %v", got, want)
	}
	if got := page.Entries["e_5"].Text; got != "Now the tests." {
		t.Errorf("second message reads %q, want %q", got, "Now the tests.")
	}
}
