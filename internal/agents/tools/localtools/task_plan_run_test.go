package localtools

import (
	"context"
	"strings"
	"testing"
)

// planWithTasks seeds a plan file so a test can act on tasks that already exist.
func planWithTasks(t *testing.T, root string, tasks ...TaskItem) {
	t.Helper()
	if err := savePlan(root, &TaskPlan{Tasks: tasks}); err != nil {
		t.Fatalf("seed plan: %v", err)
	}
}

// setBuildCommand overrides the auto-build for one test and restores it after.
// Most tests here leave a task outstanding so the build never fires at all.
func setBuildCommand(t *testing.T, cmd string) {
	t.Helper()
	previous := TaskPlanBuildCommand
	TaskPlanBuildCommand = cmd
	t.Cleanup(func() { TaskPlanBuildCommand = previous })
}

func TestRunTaskPlanAddAppendsAPendingTask(t *testing.T) {
	root := t.TempDir()

	out, err := runTaskPlan(context.Background(), root, `{"action":"add","task":"Write the seed test","description":"cmd/tool-store"}`)
	if err != nil {
		t.Fatalf("runTaskPlan: %v", err)
	}
	if !strings.Contains(out, "1 task(s)") {
		t.Errorf("summary = %q, want it to report 1 task", out)
	}

	plan := loadPlan(root)
	if len(plan.Tasks) != 1 {
		t.Fatalf("plan has %d tasks, want 1", len(plan.Tasks))
	}
	got := plan.Tasks[0]
	if got.Task != "Write the seed test" {
		t.Errorf("task = %q, want the one that was added", got.Task)
	}
	if got.Description != "cmd/tool-store" {
		t.Errorf("description = %q, want it carried through", got.Description)
	}
	if got.Status != TaskPending {
		t.Errorf("status = %q, want %q", got.Status, TaskPending)
	}
}

func TestRunTaskPlanUpdateReplacesTheWholeList(t *testing.T) {
	root := t.TempDir()
	planWithTasks(t, root, TaskItem{Task: "old", Status: TaskPending})

	_, err := runTaskPlan(context.Background(), root,
		`{"action":"update","tasks":[{"task":"new one","status":"pending"},{"task":"new two","status":"in_progress"}]}`)
	if err != nil {
		t.Fatalf("runTaskPlan: %v", err)
	}

	plan := loadPlan(root)
	if len(plan.Tasks) != 2 {
		t.Fatalf("plan has %d tasks, want 2", len(plan.Tasks))
	}
	if plan.Tasks[0].Task != "new one" || plan.Tasks[1].Task != "new two" {
		t.Errorf("tasks = %+v, want the replacement list", plan.Tasks)
	}
	for _, task := range plan.Tasks {
		if task.Task == "old" {
			t.Error("update kept a task from the previous list")
		}
	}
}

func TestRunTaskPlanStartMarksOnlyTheNamedIndex(t *testing.T) {
	root := t.TempDir()
	planWithTasks(t, root,
		TaskItem{Task: "first", Status: TaskPending},
		TaskItem{Task: "second", Status: TaskPending},
	)

	if _, err := runTaskPlan(context.Background(), root, `{"action":"start","index":1}`); err != nil {
		t.Fatalf("runTaskPlan: %v", err)
	}

	plan := loadPlan(root)
	if plan.Tasks[1].Status != TaskInProgress {
		t.Errorf("task 1 status = %q, want %q", plan.Tasks[1].Status, TaskInProgress)
	}
	if plan.Tasks[0].Status != TaskPending {
		t.Errorf("task 0 status = %q, want it untouched at %q", plan.Tasks[0].Status, TaskPending)
	}
}

// Completing a task removes it, which is the behavior that keeps the plan
// short. A second task is left outstanding so the auto-build does not fire.
func TestRunTaskPlanCompleteRemovesThatTaskAndKeepsTheRest(t *testing.T) {
	root := t.TempDir()
	planWithTasks(t, root,
		TaskItem{Task: "done one", Status: TaskPending},
		TaskItem{Task: "still to do", Status: TaskPending},
	)

	out, err := runTaskPlan(context.Background(), root, `{"action":"complete","index":0}`)
	if err != nil {
		t.Fatalf("runTaskPlan: %v", err)
	}
	if !strings.Contains(out, "1 task(s) completed") || !strings.Contains(out, "1 remaining") {
		t.Errorf("summary = %q, want it to report 1 completed and 1 remaining", out)
	}

	plan := loadPlan(root)
	if len(plan.Tasks) != 1 {
		t.Fatalf("plan has %d tasks, want 1", len(plan.Tasks))
	}
	if plan.Tasks[0].Task != "still to do" {
		t.Errorf("remaining task = %q, want %q", plan.Tasks[0].Task, "still to do")
	}
}

// Emptying the plan triggers the auto-build. The command is overridden so the
// test pins the wiring without shelling out to a real build.
func TestRunTaskPlanRunsTheBuildWhenTheLastTaskIsCompleted(t *testing.T) {
	setBuildCommand(t, "echo the-build-ran")
	root := t.TempDir()
	planWithTasks(t, root, TaskItem{Task: "the last one", Status: TaskPending})

	out, err := runTaskPlan(context.Background(), root, `{"action":"complete","index":0}`)
	if err != nil {
		t.Fatalf("runTaskPlan: %v", err)
	}

	if !strings.Contains(out, "the-build-ran") {
		t.Errorf("summary = %q, want the build output in it", out)
	}
	if !strings.Contains(out, "Auto-build passed") {
		t.Errorf("summary = %q, want it to report the build passed", out)
	}
}

// A failing build must add a fix task rather than report all-done, which is the
// whole point of running it.
func TestRunTaskPlanAddsAFixTaskWhenTheBuildFails(t *testing.T) {
	setBuildCommand(t, "echo the-compiler-complaint >&2; exit 1")
	root := t.TempDir()
	planWithTasks(t, root, TaskItem{Task: "the last one", Status: TaskPending})

	out, err := runTaskPlan(context.Background(), root, `{"action":"complete","index":0}`)
	if err != nil {
		t.Fatalf("runTaskPlan: %v", err)
	}

	if !strings.Contains(out, "auto-build FAILED") {
		t.Errorf("summary = %q, want it to report the failure", out)
	}
	plan := loadPlan(root)
	if len(plan.Tasks) == 0 {
		t.Fatal("the plan is empty, want a fix task added so the failure is not lost")
	}
}

func TestRunTaskPlanReportsAnIndexOutOfRange(t *testing.T) {
	for _, action := range []string{"complete", "start"} {
		root := t.TempDir()
		planWithTasks(t, root, TaskItem{Task: "only one", Status: TaskPending})

		out, err := runTaskPlan(context.Background(), root, `{"action":"`+action+`","index":5}`)
		if err != nil {
			t.Errorf("%s: err = %v, want the refusal in the output instead", action, err)
		}
		if !strings.Contains(out, "out of range") {
			t.Errorf("%s: out = %q, want it to say the index is out of range", action, out)
		}
		if plan := loadPlan(root); len(plan.Tasks) != 1 || plan.Tasks[0].Status != TaskPending {
			t.Errorf("%s: an out-of-range index changed the plan: %+v", action, plan.Tasks)
		}
	}
}

func TestRunTaskPlanReportsAMissingIndex(t *testing.T) {
	for _, action := range []string{"complete", "start"} {
		root := t.TempDir()
		planWithTasks(t, root, TaskItem{Task: "only one", Status: TaskPending})

		out, err := runTaskPlan(context.Background(), root, `{"action":"`+action+`"}`)
		if err != nil {
			t.Errorf("%s: err = %v, want the refusal in the output instead", action, err)
		}
		if !strings.Contains(out, "'index' required") {
			t.Errorf("%s: out = %q, want it to name the missing index", action, out)
		}
	}
}

// Index 0 must be usable. A missing index is signalled by a nil pointer rather
// than by the zero value, and this is the case that tells the two apart.
func TestRunTaskPlanTreatsIndexZeroAsGiven(t *testing.T) {
	root := t.TempDir()
	planWithTasks(t, root,
		TaskItem{Task: "first", Status: TaskPending},
		TaskItem{Task: "second", Status: TaskPending},
	)

	out, err := runTaskPlan(context.Background(), root, `{"action":"start","index":0}`)
	if err != nil {
		t.Fatalf("runTaskPlan: %v", err)
	}
	if strings.Contains(out, "'index' required") {
		t.Fatalf("index 0 was read as a missing index: %q", out)
	}
	if got := loadPlan(root).Tasks[0].Status; got != TaskInProgress {
		t.Errorf("task 0 status = %q, want %q", got, TaskInProgress)
	}
}

func TestRunTaskPlanAddRequiresATaskName(t *testing.T) {
	root := t.TempDir()

	out, err := runTaskPlan(context.Background(), root, `{"action":"add","description":"no name"}`)
	if err != nil {
		t.Fatalf("err = %v, want the refusal in the output instead", err)
	}
	if !strings.Contains(out, "'task' required") {
		t.Errorf("out = %q, want it to name the missing field", out)
	}
	if plan := loadPlan(root); len(plan.Tasks) != 0 {
		t.Errorf("a nameless task was added anyway: %+v", plan.Tasks)
	}
}

func TestRunTaskPlanReportsAnUnknownAction(t *testing.T) {
	root := t.TempDir()

	out, err := runTaskPlan(context.Background(), root, `{"action":"delete"}`)
	if err != nil {
		t.Fatalf("err = %v, want the refusal in the output instead", err)
	}
	if !strings.Contains(out, `unknown action "delete"`) {
		t.Errorf("out = %q, want it to name the action it refused", out)
	}
}

func TestRunTaskPlanRejectsMalformedInput(t *testing.T) {
	root := t.TempDir()

	if _, err := runTaskPlan(context.Background(), root, `{"action":`); err == nil {
		t.Fatal("malformed JSON was accepted, want an error")
	}
}
