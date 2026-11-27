# Agent Rendering — moved

This doc has been consolidated into the cross-cutting design set in `llm-bridge-server`.

See: **`~/repos/llm-bridge-server/docs/AGENT-MANAGEMENT.md`**

That doc covers:
- Canonical agent shape (post `orchestrator → harness` rename)
- The rendering library (`llm-bridge/render`)
- CC subagent rendering via `--agents <json>` (no file materialization, verified 2026-05-09)
- Codex / inber / other-harness rendering
- CRUD flows
- `/agents` UI scope vs. `/files` debug surface
- Migration sequence
- Open questions

agent-store remains the canonical home for agent data (rows in `agents`, `agent_harness`, `agent_nature`, `agent_skills`, `agent_harness_tools`, `tracked_files`). This file points at the cross-cutting design.
