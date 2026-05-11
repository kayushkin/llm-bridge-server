# Cache-Busting Rules

The cache key for the Anthropic prompt cache (and equivalents on other providers) is the byte content of the prompt prefix. Any change to that prefix between turns is a cache miss. These rules keep the prefix stable.

## The seven rules

1. **Deterministic composition.** Same inputs → same bytes. Sort lists alphabetically, no timestamps, no session IDs, no UUIDs in the bundle.
2. **No dynamic content in the bundle.** Current time, "today is X", per-turn recall — none of this in CLAUDE.md or in the appended system prompt. Dynamic content goes in the per-turn user message.
3. **Tools and skills set is fixed at session start.** Adding or removing either mid-session = new session by definition.
4. **Edit-during-session policy: ignored.** If a tracked file changes mid-session, the session continues with whatever it materialized at start. Next session picks up the change. Do not hot-reload.
5. **Section ordering is part of the contract.** Once you ship `identity → skills → tools → extras`, don't reorder later "for readability." Reorder = global cache bust across every running session.
6. **Section gating, not optional dynamic content.** If a section can be empty, choose between always-present-with-empty-marker vs. omitted. Pick one and stick with it. Toggling between them across turns is a cache bust.
7. **Bundle hash is the cache identity.** Compute SHA256 of the materialized bundle at session start. Log it on `session.start`, persist on the session row. When debugging cache regressions, compare hashes across sessions to spot what changed.

## What's allowed to bust cache

These are accepted cache misses, not bugs:

- **Mid-session agent-store edits** when the model itself uses `inber agent identity append` or similar. Rare, intentional, the cost is one cache miss the user opted into.
- **Out-of-band edits to `~/.claude/CLAUDE.md`** between sessions. Different session, different prefix, different cache. Expected.
- **Tool-store / skill-store enrollment changes.** Different prefix on the next session. Expected.

## What NOT to do

- **Don't watch tracked files and re-render mid-session.** That's the inverse of rule 4.
- **Don't include the `--agents` JSON in code that runs every turn.** Build it once at session start.
- **Don't make the renderer non-deterministic.** Maps in Go iterate in random order; sort keys.
- **Don't put recall in the system prompt.** It belongs in the most recent user message. CC injects its own recall via `memory_paths`; ours goes in user content.

## What's outside our control

- CC's auto-loaded `CLAUDE.md` (project + global) is part of the prefix CC composes. We don't see it in our `BundleHash`. If a user edits their global CLAUDE.md, every session of theirs gets a fresh cache key. That's CC's behavior, not ours.
- CC's own cache-breakpoint placement. We can't add `cache_control: ephemeral` markers from outside CC. CC owns the breakpoint decisions.

## Cache observability

The `BundleHash` we log captures only the bytes we inject (composed identity + sorted tools + agents JSON + skills enrollment + extras). It does NOT include:
- CC's built-in system prompt
- Auto-loaded CLAUDE.md
- The user's per-turn message

That's deliberate — those aren't ours to track. Our hash answers "did we change what we sent?" not "did the model see the same prefix?"

When an actual cache regression happens in production, the diagnostic flow is:
1. Compare `BundleHash` across the last N sessions for this agent. If it changed, the bust is on us — find the agent-store edit.
2. If `BundleHash` is stable, check whether the user edited `~/.claude/CLAUDE.md` or a project CLAUDE.md.
3. If neither, suspect a CC version bump that changed the built-in prefix.
