package permstore

// SeedV1Rules returns the initial default rule set inserted on first boot
// (by migrateV2). Each rule has a deterministic id of the form
// "seed-v1-<slug>" so callers can recognize defaults vs user-added rules
// in the editor UI.
//
// Priority convention:
//
//	  50  default deny patterns (run before allows of the same shape)
//	 100  default allow patterns
//	 150  reserved for built-in escalations / future defaults
//	 200+ user-added rules (preempt defaults if user wants)
//
// Pattern syntax is Go RE2 — no lookarounds. The "deny X unless Y" idiom
// is therefore expressed as a pair: a low-priority allow for the safe
// case, plus a higher-priority deny for the broad case.
func SeedV1Rules() []Rule {
	return []Rule{
		// --- Read-only tools — auto-allow ---
		seed("seed-v1-allow-Read", "Read", "", OutcomeAllow, 100, "default seed: read-only tool"),
		seed("seed-v1-allow-Glob", "Glob", "", OutcomeAllow, 100, "default seed: read-only tool"),
		seed("seed-v1-allow-Grep", "Grep", "", OutcomeAllow, 100, "default seed: read-only tool"),
		seed("seed-v1-allow-LS", "LS", "", OutcomeAllow, 100, "default seed: read-only tool"),
		seed("seed-v1-allow-TodoWrite", "TodoWrite", "", OutcomeAllow, 100, "default seed: in-process task list"),
		seed("seed-v1-allow-AskUserQuestion", "AskUserQuestion", "", OutcomeAllow, 100, "default seed: prompts the user"),
		seed("seed-v1-allow-WebSearch", "WebSearch", "", OutcomeAllow, 100, "default seed: read-only"),

		// --- Bash atom safelist — common harmless commands ---
		seed("seed-v1-allow-bash-git-readonly", "Bash", `^git (status|log|diff|branch|show)\b`, OutcomeAllow, 100, "default seed: read-only git"),
		seed("seed-v1-allow-bash-ls", "Bash", `^ls\b`, OutcomeAllow, 100, "default seed: list files"),
		seed("seed-v1-allow-bash-cat", "Bash", `^cat\b`, OutcomeAllow, 100, "default seed: read file"),
		seed("seed-v1-allow-bash-rg", "Bash", `^rg\b`, OutcomeAllow, 100, "default seed: ripgrep"),
		seed("seed-v1-allow-bash-grep", "Bash", `^grep\b`, OutcomeAllow, 100, "default seed: grep"),
		seed("seed-v1-allow-bash-pwd", "Bash", `^pwd\b`, OutcomeAllow, 100, "default seed: pwd"),
		seed("seed-v1-allow-bash-which", "Bash", `^which\b`, OutcomeAllow, 100, "default seed: which"),
		seed("seed-v1-allow-bash-echo", "Bash", `^echo\b`, OutcomeAllow, 100, "default seed: echo"),
		seed("seed-v1-allow-bash-date", "Bash", `^date\b`, OutcomeAllow, 100, "default seed: date"),

		// --- Bash atom denylist — explicitly dangerous patterns ---
		// "deny rm -rf <absolute path>" — refuses any rm that targets an
		// absolute path with -r/-rf. Relative-path rm calls fall through
		// to the default ask.
		seed("seed-v1-deny-bash-rm-rf-abs", "Bash", `^rm\s+-rf?\s+/`, OutcomeDeny, 50, "rm -rf with an absolute path is denied by default — disable this seed if you really need it"),

		// "deny curl unless to localhost" — RE2 has no lookarounds, so
		// expressed as two rules. Priority 40 allow fires first for the
		// safe case; priority 50 deny catches everything else.
		seed("seed-v1-allow-bash-curl-localhost", "Bash", `^curl\b.*\b(localhost|127\.0\.0\.1)\b`, OutcomeAllow, 40, "default seed: curl to localhost is allowed"),
		seed("seed-v1-deny-bash-curl-external", "Bash", `^curl\b`, OutcomeDeny, 50, "curl to non-localhost is denied by default"),
	}
}

func seed(id, tool, pattern, outcome string, priority int, message string) Rule {
	return Rule{
		ID:       id,
		Scope:    ScopeGlobal,
		Priority: priority,
		Tool:     tool,
		Pattern:  pattern,
		Outcome:  outcome,
		Message:  message,
		Enabled:  true,
	}
}
