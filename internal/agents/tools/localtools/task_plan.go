package localtools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/kayushkin/llm-bridge-server/internal/agents/tools/internal/childprocess"
	"github.com/kayushkin/llm-bridge-server/internal/agents/tools/schema"
	"github.com/kayushkin/llm-bridge-server/internal/childprocessenv"
)

const taskPlanFile = ".task.md"

// TaskStatus represents a task's state.
type TaskStatus string

const (
	TaskPending    TaskStatus = "pending"
	TaskInProgress TaskStatus = "in_progress"
	TaskDone       TaskStatus = "done"
	TaskBlocked    TaskStatus = "blocked"
)

// TaskItem represents a single task in the plan.
type TaskItem struct {
	Task        string     `json:"task"`
	Description string     `json:"description,omitempty"`
	Status      TaskStatus `json:"status"`
}

// TaskPlan represents the full plan state.
type TaskPlan struct {
	Tasks []TaskItem `json:"tasks"`
}

// TaskPlanBuildCommand is the build command to run when all tasks are done.
// Configurable per-project. Defaults to "go build ./...".
var TaskPlanBuildCommand = "go build ./..."

// TaskPlanTool returns a tool that manages a task plan file.
// The file is always loaded into context by the engine.
// repoRoot is the project directory where .task.md lives.
func TaskPlanTool(repoRoot string) Impl {
	return Impl{
		Name: "task_plan",
		Description: `Manage your task plan. This is your working memory — it persists across turns and is always visible to you.

Actions:
- "update": Replace the full task list. Use when restructuring.
- "complete": Mark a task done by index (0-based). Completed tasks are auto-removed.
- "add": Add a new task.
- "start": Mark a task as in_progress by index.

The task plan drives your work. When all tasks are complete, a build/test runs automatically.
If it fails, a fix task is added for you. Use the scratchpad tool for working notes.`,
		InputSchema: schema.Props([]string{"action"}, map[string]any{
			"action": map[string]any{
				"type":        "string",
				"enum":        []string{"update", "complete", "add", "start"},
				"description": "Action to perform",
			},
			"tasks": map[string]any{
				"type":        "array",
				"description": "Full task list for 'update' action",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"task":        map[string]any{"type": "string", "description": "Task name (imperative: 'Fix auth bug')"},
						"description": map[string]any{"type": "string", "description": "Optional details"},
						"status":      map[string]any{"type": "string", "enum": []string{"pending", "in_progress", "done", "blocked"}},
					},
					"required": []string{"task", "status"},
				},
			},
			"index": map[string]any{
				"type":        "integer",
				"description": "Task index (0-based) for 'complete'/'start' action",
			},
			"task": map[string]any{
				"type":        "string",
				"description": "Task name for 'add' action",
			},
			"description": map[string]any{
				"type":        "string",
				"description": "Task description for 'add' action",
			},
		}),
		Run: func(ctx context.Context, raw string) (string, error) {
			return runTaskPlan(ctx, repoRoot, raw)
		},
	}
}

type taskPlanInput struct {
	Action      string     `json:"action"`
	Tasks       []TaskItem `json:"tasks,omitempty"`
	Index       *int       `json:"index,omitempty"`
	Task        string     `json:"task,omitempty"`
	Description string     `json:"description,omitempty"`
}

func runTaskPlan(ctx context.Context, repoRoot, raw string) (string, error) {
	var in taskPlanInput
	if err := json.Unmarshal([]byte(raw), &in); err != nil {
		return "", fmt.Errorf("invalid input: %w", err)
	}

	plan := loadPlan(repoRoot)

	switch in.Action {
	case "update":
		plan.Tasks = in.Tasks

	case "complete":
		if in.Index == nil {
			return "error: 'index' required for complete action", nil
		}
		idx := *in.Index
		if idx < 0 || idx >= len(plan.Tasks) {
			return fmt.Sprintf("error: index %d out of range (0-%d)", idx, len(plan.Tasks)-1), nil
		}
		plan.Tasks[idx].Status = TaskDone

	case "start":
		if in.Index == nil {
			return "error: 'index' required for start action", nil
		}
		idx := *in.Index
		if idx < 0 || idx >= len(plan.Tasks) {
			return fmt.Sprintf("error: index %d out of range (0-%d)", idx, len(plan.Tasks)-1), nil
		}
		plan.Tasks[idx].Status = TaskInProgress

	case "add":
		if in.Task == "" {
			return "error: 'task' required for add action", nil
		}
		plan.Tasks = append(plan.Tasks, TaskItem{
			Task:        in.Task,
			Description: in.Description,
			Status:      TaskPending,
		})

	default:
		return fmt.Sprintf("error: unknown action %q", in.Action), nil
	}

	// Auto-remove completed tasks.
	filtered := plan.Tasks[:0]
	completed := 0
	for _, t := range plan.Tasks {
		if t.Status == TaskDone {
			completed++
			continue
		}
		filtered = append(filtered, t)
	}
	plan.Tasks = filtered

	if err := savePlan(repoRoot, plan); err != nil {
		return "", err
	}

	remaining := len(plan.Tasks)

	// Auto-build when all tasks complete.
	if remaining == 0 && completed > 0 {
		buildResult := runBuild(ctx, repoRoot)
		if buildResult.Stopped {
			return fmt.Sprintf("✓ %d task(s) completed. Auto-build %s", completed, buildResult.Output), nil
		}
		if buildResult.Success {
			return fmt.Sprintf("✓ %d task(s) completed. All done!\n\n🔨 Auto-build passed:\n%s", completed, buildResult.Output), nil
		}
		// Build failed — add fix task.
		if err := AddBuildErrorTask(repoRoot, buildResult.Output); err != nil {
			return fmt.Sprintf("✓ %d task(s) completed but auto-build failed (and couldn't save fix task):\n%s", completed, buildResult.Output), nil
		}
		// Re-read the plan to show the new task.
		return fmt.Sprintf("✓ %d task(s) completed but auto-build FAILED. Added 'Fix build error' task.\n\n🔨 Build output:\n%s", completed, buildResult.Output), nil
	}

	if completed > 0 {
		return fmt.Sprintf("✓ %d task(s) completed and removed. %d remaining.", completed, remaining), nil
	}
	return fmt.Sprintf("Plan updated. %d task(s).", remaining), nil
}

func planPath(repoRoot string) string {
	return filepath.Join(repoRoot, taskPlanFile)
}

func loadPlan(repoRoot string) *TaskPlan {
	data, err := os.ReadFile(planPath(repoRoot))
	if err != nil {
		return &TaskPlan{}
	}
	return ParsePlanMD(string(data))
}

func savePlan(repoRoot string, plan *TaskPlan) error {
	return os.WriteFile(planPath(repoRoot), []byte(renderPlanMD(plan)), 0644)
}

// ParsePlanMD parses the markdown task plan format.
func ParsePlanMD(content string) *TaskPlan {
	plan := &TaskPlan{}
	lines := strings.Split(content, "\n")

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "- [") {
			task := parseTaskLine(trimmed)
			if task != nil {
				plan.Tasks = append(plan.Tasks, *task)
			}
		}
	}
	return plan
}

func parseTaskLine(line string) *TaskItem {
	var status TaskStatus
	var rest string

	if strings.HasPrefix(line, "- [x] ") {
		status = TaskDone
		rest = strings.TrimPrefix(line, "- [x] ")
	} else if strings.HasPrefix(line, "- [~] ") {
		status = TaskInProgress
		rest = strings.TrimPrefix(line, "- [~] ")
	} else if strings.HasPrefix(line, "- [!] ") {
		status = TaskBlocked
		rest = strings.TrimPrefix(line, "- [!] ")
	} else if strings.HasPrefix(line, "- [ ] ") {
		status = TaskPending
		rest = strings.TrimPrefix(line, "- [ ] ")
	} else {
		return nil
	}

	// Split task — description on same line separated by " — "
	parts := strings.SplitN(rest, " — ", 2)
	task := &TaskItem{Task: parts[0], Status: status}
	if len(parts) > 1 {
		task.Description = parts[1]
	}
	return task
}

// renderPlanMD renders the plan as markdown.
func renderPlanMD(plan *TaskPlan) string {
	var b strings.Builder
	b.WriteString("# Task Plan\n\n")

	b.WriteString("## Tasks\n")
	for _, t := range plan.Tasks {
		var check string
		switch t.Status {
		case TaskDone:
			check = "x"
		case TaskInProgress:
			check = "~"
		case TaskBlocked:
			check = "!"
		default:
			check = " "
		}
		b.WriteString(fmt.Sprintf("- [%s] %s", check, t.Task))
		if t.Description != "" {
			b.WriteString(fmt.Sprintf(" — %s", t.Description))
		}
		b.WriteString("\n")
	}

	return b.String()
}

// BuildResult holds the outcome of an auto-build check.
type BuildResult struct {
	Success bool
	Output  string
	// Stopped reports that the build was canceled rather than finished. A
	// stopped build has decided nothing about the code, so callers must not
	// read it as a failure: doing so writes a "Fix build error" task
	// describing a build that never ran to completion.
	Stopped bool
}

type buildResult = BuildResult

func runBuild(ctx context.Context, repoRoot string) BuildResult {
	cmd := TaskPlanBuildCommand
	if cmd == "" {
		return BuildResult{Success: true, Output: "no build command configured"}
	}

	proc := childprocess.NewCommand(ctx, "bash", "-c", cmd)
	proc.Dir = repoRoot
	proc.Env = append(childprocessenv.EnvironmentWithoutServerSecrets(), "PATH="+os.Getenv("PATH"))
	out, err := proc.CombinedOutput()
	output := strings.TrimSpace(string(out))
	if len(output) > 2000 {
		output = schema.TruncateAtRuneBoundary(output, 2000) + "\n...(truncated)"
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return BuildResult{Stopped: true, Output: fmt.Sprintf("build stopped: %s", ctxErr)}
	}
	if err != nil {
		return BuildResult{Success: false, Output: output}
	}
	if output == "" {
		output = "success (no output)"
	}
	return BuildResult{Success: true, Output: output}
}

// SavePlanMD writes the task plan to disk.
func SavePlanMD(repoRoot string, plan *TaskPlan) error {
	return savePlan(repoRoot, plan)
}

// RunBuildCheck runs the build command and returns the result. Canceling ctx
// stops the build and everything it started, and is reported as
// BuildResult.Stopped rather than as a build failure.
func RunBuildCheck(ctx context.Context, repoRoot string) buildResult {
	return runBuild(ctx, repoRoot)
}

// LoadPlanContext reads the .task.md file and returns it for context injection.
// Returns empty string if no plan exists.
func LoadPlanContext(repoRoot string) string {
	data, err := os.ReadFile(filepath.Join(repoRoot, taskPlanFile))
	if err != nil {
		return ""
	}
	return string(data)
}

// AllTasksDone returns true if a plan exists and has no remaining tasks.
func AllTasksDone(repoRoot string) bool {
	plan := loadPlan(repoRoot)
	return len(plan.Tasks) == 0
}

// AddBuildErrorTask adds a task for a build failure.
func AddBuildErrorTask(repoRoot string, buildOutput string) error {
	plan := loadPlan(repoRoot)
	// Truncate build output to keep it manageable.
	if len(buildOutput) > 500 {
		buildOutput = schema.TruncateAtRuneBoundary(buildOutput, 500) + "..."
	}
	plan.Tasks = append(plan.Tasks, TaskItem{
		Task:        "Fix build error",
		Description: buildOutput,
		Status:      TaskPending,
	})
	return savePlan(repoRoot, plan)
}
