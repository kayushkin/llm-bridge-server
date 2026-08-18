package server

import (
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/store"
)

// A turn killed before its first tool call has left nothing behind, so the
// instruction still describes work that has not happened. Replaying it
// verbatim is the whole point of auto-resume and must survive.
func TestATurnKilledBeforeItRanAnythingIsReplayedVerbatim(t *testing.T) {
	const instruction = "deploy the gateway"

	text, replayed := resumeMessage(store.InterruptedTurn{
		UserMessageText:     instruction,
		ToolCallsAlreadyRun: 0,
	})

	if !replayed {
		t.Error("replayedVerbatim = false; a turn that ran nothing must still be replayed")
	}
	if text != instruction {
		t.Errorf("text = %q, want the instruction unchanged %q", text, instruction)
	}
}

// The regression this whole change exists for. A turn killed after its tool
// calls has already changed state outside the process, and re-sending the
// instruction is indistinguishable from the user asking for all of it again.
// Measured on this host: a deploy turn was killed by the restart its own
// deploy triggered, and the approval came back looking freshly typed.
func TestATurnKilledMidWorkIsNotHandedBackAsAFreshInstruction(t *testing.T) {
	const instruction = "Yeah lets go ahead a merge/fix those branches and get it deployed"

	text, replayed := resumeMessage(store.InterruptedTurn{
		UserMessageText:     instruction,
		ToolCallsAlreadyRun: 23,
	})

	if replayed {
		t.Fatal("replayedVerbatim = true; a turn with side effects must not be replayed verbatim")
	}
	if strings.Contains(text, instruction) {
		t.Errorf("notice repeats the instruction, which is the bug:\n%s", text)
	}
	// The count is what tells the model how much of its own transcript to go
	// back and check, so a notice without it is not doing the job.
	if !strings.Contains(text, "23") {
		t.Errorf("notice does not say how many tool calls had run:\n%s", text)
	}
	if !strings.Contains(text, "not finish") {
		t.Errorf("notice does not say the turn was interrupted:\n%s", text)
	}
}

// One tool call is enough to make a turn unsafe to replay. The boundary is
// "did anything at all happen", not "did enough happen to worry about" —
// nothing here can tell whether that one call was a read or a deploy.
func TestASingleToolCallIsEnoughToSuppressTheReplay(t *testing.T) {
	_, replayed := resumeMessage(store.InterruptedTurn{
		UserMessageText:     "do the thing",
		ToolCallsAlreadyRun: 1,
	})
	if replayed {
		t.Error("replayedVerbatim = true after one tool call; the boundary is any work at all")
	}
}
