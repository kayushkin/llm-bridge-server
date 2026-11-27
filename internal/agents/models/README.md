# model-store

Centralized model registry for AI agents. Tracks providers, models, aliases, pricing, and health — shared across the [llm-bridge](https://github.com/kayushkin/llm-bridge) ecosystem and any other consumer.

It is a module of llm-bridge-server: its records live in Postgres (schema `models`, role `models_module`, built by `migrations/`), the server uses the library in process, and the HTTP routes the standalone `ms serve` answered on `:8155` are mounted at `/models/`, the same paths behind that prefix (`/models/api/models`, `/models/api/roles`…). `llm-bridge-server import-models -sqlite <file>` copies a standalone store's SQLite file into the module. The `ms` command line opens the module directly and needs `--postgres-url`, the bridge's `database.postgres_url`; the bridge must have started on it once, since that is what makes and migrates the module.

## Features

- **Provider registry** — Anthropic, OpenAI, Google, Ollama, OpenRouter
- **Model catalog** — models per provider with aliases, short names, context window, and pricing
- **Alias resolution** — resolve short names like `sonnet` or `haiku` to full model IDs
- **Priority & failover** — enabled/disabled state and priority ordering for failover chains
- **Live sync** — fetch current model lists from Anthropic, OpenAI, and Google APIs
- **Health tracking** — record success/error metrics per model with rolling averages
- **Go library** — import directly, no network service needed
- **CLI** — `ms` binary for interactive use

## CLI

```bash
ms providers                  # list configured providers
ms models                     # list all models with pricing and aliases
ms models anthropic           # list models for a specific provider
ms resolve sonnet             # resolve alias to full model details
ms seed                       # populate default providers and models
ms sync                       # fetch live models from all provider APIs
ms sync anthropic google      # sync specific providers
ms enable claude-opus-4-6     # enable a model
ms disable gpt-4o             # disable a model
ms cost gpt-5 1.25 10         # set input/output cost per million tokens
ms priority claude-opus-4-6 3 # set failover priority (lower = preferred)
ms shortname claude-opus-4-6 opus-4.6  # set the short display nickname
ms alias add gpt-5 fast       # point an alias at a model
ms alias rm fast              # remove an alias
ms add openai gpt-6-sol --name GPT-6-Sol --max-tokens 272000  # add a model sync cannot see
ms delete gpt-4o              # delete a model and its aliases
```

A model's **short name** is the shortest nickname that still tells it apart from its
siblings — `opus-4.6`, `5.1-codex`, `2.5-pro`. It is there for the dense corners of a
UI, like a picker squeezed into a crowded top bar, where the full name would be
truncated into something ambiguous. It is only a label: nothing resolves a model by
its short name, so use an alias if you want something you can actually type at `ms
resolve`.

The mutating verbs (`enable`/`disable`/`cost`/`priority`/`shortname`/`delete`) key on the exact
model ID, not an alias — use `ms models` or `ms resolve` to find it. `ms alias add`
refuses to repoint an existing alias or shadow a model ID; remove the old alias first.

### Sync

`ms sync` fetches the current model list from each provider's API and upserts into the store. Existing models are updated (name, context window) but user-set fields (enabled, priority, cost, aliases, short name) are preserved. New models are added with priority 100 and estimated costs.

Requires API keys via environment variables (`ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, `GOOGLE_API_KEY`) or [aiauth](https://github.com/kayushkin/aiauth) profiles.

## Go library

```go
import ms "github.com/kayushkin/model-store"

store := ms.New(db)  // db: the module's pool, from postgresstorage.Open

// List providers and models
providers, _ := store.Providers()
models, _ := store.Models("anthropic")

// Resolve by ID or alias
model, _ := store.ResolveModel("sonnet")
// model.ID = "claude-sonnet-4-5-20250929", model.Provider = "anthropic"

// Look up provider for a model
provider, _ := store.ProviderFor("claude-opus-4-6")

// Failover chain (enabled models, sorted by priority)
chain, _ := store.FailoverChain()

// Enable/disable
store.SetEnabled("gpt-4o", false)

// Health tracking
store.RecordSuccess("claude-sonnet-4-5-20250929", 1200) // 1200ms
store.RecordError("gpt-4o", "rate limited")

health := store.GetHealth("claude-sonnet-4-5-20250929")
health.IsHealthy(5 * time.Minute)  // true if successful within window

// All models with health data (for dashboards)
statuses, _ := store.AllModelsWithStatus()

// Seed defaults and sync from APIs
store.Seed()
result, _ := store.SyncProvider("anthropic", apiKey)
```

## Schema

`migrations/0001_create_the_model_registry.sql` is the schema: `providers`, `models`, `model_aliases`, `model_roles`, `model_role_fallbacks`, `model_health` (unix seconds, 0 for never) and `usage`, the old daily token counts an orchestrator once wrote, kept by the import and read by nothing here. Text keys sort byte by byte, and providers and a model's aliases list in the order they were first written. Foreign keys are enforced, which the standalone SQLite store never did: a model needs its provider, and an alias, role or fallback its model.

## Part of the llm-bridge ecosystem

model-store is one of several optional stores used by [llm-bridge-server](https://github.com/kayushkin/llm-bridge-server). When loaded, it powers the `/models` endpoint and provides model resolution for session configuration. See the [llm-bridge README](https://github.com/kayushkin/llm-bridge) for the full picture.
