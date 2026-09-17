package server

import (
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge/msg"
)

// The model is parked waiting on these tasks by name, so the notice has to
// name them; "some tasks died" leaves it guessing what it is still owed.
func TestBackgroundTasksLostMessageNamesEachTask(t *testing.T) {
	text := backgroundTasksLostMessage([]msg.BackgroundTask{
		{TaskID: "af9b03cbb5668505d", TaskType: msg.TaskTypeLocalAgent, Description: "Map dash frontend and proxies"},
		{TaskID: "bq136qbl3", TaskType: msg.TaskTypeLocalBash, Description: "Wait for the full test run to finish"},
	})
	for _, want := range []string{
		"Map dash frontend and proxies", "af9b03cbb5668505d", msg.TaskTypeLocalAgent,
		"Wait for the full test run to finish", "bq136qbl3", msg.TaskTypeLocalBash,
		"do not wait for it", "not a new instruction",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("notice does not contain %q:\n%s", want, text)
		}
	}
}

// With no list the notice must say so, not imply that nothing was running.
func TestBackgroundTasksLostMessageSaysSoWhenItHasNoList(t *testing.T) {
	text := backgroundTasksLostMessage(nil)
	if !strings.Contains(text, "no list of what was running") {
		t.Errorf("notice does not admit it has no list:\n%s", text)
	}
}
