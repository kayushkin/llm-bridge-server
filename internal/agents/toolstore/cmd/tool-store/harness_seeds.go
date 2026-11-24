package main

import (
	"errors"
	"fmt"

	toolstore "github.com/kayushkin/tool-store"
)

// Tags on harness tools. A tool is tagged tagReadOnly when it only reads,
// tagEffects when it changes files, runs commands, sends, schedules or reaches
// the network, and neither when that is not plain. tagRunsCommands marks a
// tool that runs shell commands: llm-bridge-server reads it to switch those
// tools off in a session whose bundle denies read paths, since a shell reads
// any path.
const (
	tagReadOnly     = "read-only"
	tagEffects      = "effects"
	tagRunsCommands = "runs-commands"
)

// harnessToolSeed is one built-in tool of a harness, as the harness names it.
type harnessToolSeed struct {
	harnessToolName string
	description     string
	tags            []string
}

// Harness ids are llm-bridge-server's; these are the two it uses for Claude
// Code and Codex.
const (
	harnessClaudeCode = "claude_code"
	harnessCodex      = "codex"
)

// claudeCodeTools is measured from Claude Code 2.1.280's init event and the
// init tool lists llm-bridge sessions report, plus the
// tools the llm-bridge-claudecode wrapper names itself.
var claudeCodeTools = []harnessToolSeed{
	{"Read", "Read a file from the local filesystem, including images, PDFs and notebooks.", []string{tagReadOnly}},
	{"Write", "Write a file to the local filesystem, replacing any file already there.", []string{tagEffects}},
	{"Edit", "Replace an exact string in a file.", []string{tagEffects}},
	{"NotebookEdit", "Replace, insert or delete a cell in a Jupyter notebook.", []string{tagEffects}},
	{"Glob", "Find files whose paths match a glob pattern.", []string{tagReadOnly}},
	{"Grep", "Search file contents with a regular expression (ripgrep).", []string{tagReadOnly}},
	{"Bash", "Run a shell command.", []string{tagEffects, tagRunsCommands}},
	{"WebFetch", "Fetch a URL and answer a prompt about its content.", []string{tagEffects}},
	{"WebSearch", "Search the web.", []string{tagEffects}},
	{"Task", "Start a subagent to carry out a task with its own tools.", []string{tagEffects}},
	{"Skill", "Load a skill's instructions into the conversation.", nil},
	{"ToolSearch", "Fetch the schemas of deferred tools so they can be called.", []string{tagReadOnly}},
	{"TaskStop", "Stop a running background task or agent.", []string{tagEffects}},
	{"Monitor", "Run a command in the background and report each line it prints.", []string{tagEffects, tagRunsCommands}},
	{"CronCreate", "Schedule a prompt to run on a cron schedule.", []string{tagEffects}},
	{"CronDelete", "Delete a scheduled cron prompt.", []string{tagEffects}},
	{"CronList", "List scheduled cron prompts.", []string{tagReadOnly}},
	{"ScheduleWakeup", "Schedule the session to resume after a delay.", []string{tagEffects}},
	{"PushNotification", "Send a push notification to the user.", []string{tagEffects}},
	{"SendMessage", "Send a message to another running agent.", []string{tagEffects}},
	{"ListAgents", "List the agents running in this session.", []string{tagReadOnly}},
	{"RemoteTrigger", "Start or manage a remote scheduled agent.", []string{tagEffects}},
	{"Workflow", "Run a workflow script that coordinates subagents.", []string{tagEffects}},
	{"EnterWorktree", "Create a git worktree and move the session into it.", []string{tagEffects}},
	{"ExitWorktree", "Leave the current git worktree and return to the original directory.", []string{tagEffects}},
	{"AskUserQuestion", "Ask the user a multiple-choice question and wait for the answer.", nil},
	{"ExitPlanMode", "Present a plan to the user and leave plan mode once they approve it.", nil},
	{"TodoWrite", "Write the session's task list.", nil},
	// Seen in the init tool lists of llm-bridge sessions over 2026-08-25 to
	// 2026-09-24 (sessions.info in the bridge database) and missing above.
	{"TaskCreate", "Add an item to the session's task list.", nil},
	{"TaskGet", "Read one item of the session's task list.", []string{tagReadOnly}},
	{"TaskList", "List the session's task list.", []string{tagReadOnly}},
	{"TaskUpdate", "Change an item of the session's task list.", nil},
	{"TaskOutput", "Read the output of a background task the session started.", []string{tagReadOnly}},
	{"EnterPlanMode", "Enter plan mode, where the session plans before it changes anything.", nil},
	{"DesignSync", "Sync designs with an outside design service.", []string{tagEffects}},
	{"ReportFindings", "Report code-review findings as a typed list for the host UI.", nil},
	{"ShareOnboardingGuide", "Share an onboarding guide outside the session.", []string{tagEffects}},
}

// codexTools is codex-cli 0.156.1's stable tool features from
// `codex features list`, plus the web_search config key.
var codexTools = []harnessToolSeed{
	{"shell_tool", "Run a shell command.", []string{tagEffects, tagRunsCommands}},
	{"unified_exec", "Run a command in a persistent terminal session and write to its input.", []string{tagEffects, tagRunsCommands}},
	{"view_image", "Attach a local image file to the conversation.", []string{tagReadOnly}},
	{"image_generation", "Generate an image from a prompt.", []string{tagEffects}},
	{"multi_agent", "Start sub-agents and exchange messages with them.", []string{tagEffects}},
	{"browser_use", "Drive a web browser.", []string{tagEffects}},
	{"computer_use", "Control the desktop with the mouse and keyboard.", []string{tagEffects}},
	{"apps", "Call ChatGPT apps (connectors) to outside services.", []string{tagEffects}},
	{"plugins", "Call tools from installed Codex plugins.", nil},
	{"sleep_tool", "Wait for a given time before the next step.", nil},
	{"code_mode_host", "Run model-written JavaScript that calls the other tools.", []string{tagEffects}},
	{"web_search", "Search the web.", []string{tagEffects}},
}

// seedHarnessTools creates the built-in tools of Claude Code and Codex that
// are missing, as kind=harness rows named <harness>.<harness_tool_name>,
// enabled — the harness offers these tools unless told otherwise, and the
// operator chose to keep that default while making it switchable. A row that
// already exists is left exactly as it is: the database owns a tool's tags,
// enabled flag and description, and this file only fills an empty table. So
// a change to a seed here reaches a new database only; change a live row with
// PATCH /tools/{id}. A row a harness reported first (POST
// /harness-tools/observed) keeps its enabled=false and its unreviewed tag.
func seedHarnessTools(store *toolstore.Store) error {
	for _, harness := range []struct {
		id    string
		tools []harnessToolSeed
	}{
		{harnessClaudeCode, claudeCodeTools},
		{harnessCodex, codexTools},
	} {
		for _, seed := range harness.tools {
			t := toolstore.Tool{
				Name:            toolstore.HarnessToolRowName(harness.id, seed.harnessToolName),
				Description:     seed.description,
				Kind:            toolstore.KindHarness,
				Harness:         harness.id,
				HarnessToolName: seed.harnessToolName,
				Tags:            seed.tags,
				Enabled:         true,
			}
			existing, err := store.GetToolByName(t.Name)
			switch {
			case err == nil:
				if existing.Kind != toolstore.KindHarness {
					return fmt.Errorf("harness tool %s: a kind=%s row already holds that name", t.Name, existing.Kind)
				}
				continue
			case !errors.Is(err, toolstore.ErrNotFound):
				return fmt.Errorf("lookup harness tool %s: %w", t.Name, err)
			}
			if _, err := store.UpsertTool(&t); err != nil {
				return fmt.Errorf("create harness tool %s: %w", t.Name, err)
			}
		}
	}
	return nil
}
