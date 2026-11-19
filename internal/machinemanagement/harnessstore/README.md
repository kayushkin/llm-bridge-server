# harness-store

SQLite registry for harness instances and credential bindings. Part of the [llm-bridge](https://github.com/kayushkin/llm-bridge) ecosystem.

Tracks which harness instances are deployed across machines (local or SSH), which credentials are bound to each instance, and display metadata for harness types. This is configuration storage only -- runtime state (active sessions, credential slot occupancy) lives in [llm-bridge-server](https://github.com/kayushkin/llm-bridge-server).

## Install

```bash
go get github.com/kayushkin/harness-store
```

## Usage

```go
import store "github.com/kayushkin/harness-store"

// Open (creates DB and runs migrations if needed)
s, err := store.Open("~/.config/harness-store/harness.db")
defer s.Close()

// Register the machine the harness runs on (local or SSH)
s.CreateMachine(&msg.Machine{
    ID:        "laptop",
    Name:      "laptop",
    Transport: msg.TransportLocal,
})

// Register a harness instance on that machine
s.CreateInstance(&msg.Instance{
    ID:          "laptop-cc",
    HarnessType: msg.HarnessClaudeCode,
    Name:        "laptop-cc",
    MachineID:   "laptop",
})

// Bind a credential to it
s.BindCredential(&msg.InstanceCredential{
    InstanceID:   "laptop-cc",
    CredentialID: "anthropic-key-1",
    Priority:     0, // primary, 1+ = fallbacks
})

// Query
instances, _ := s.ListInstancesByHarness(msg.HarnessClaudeCode)
creds, _ := s.ListInstanceCredentials("laptop-cc")
```

## Schema

### `machines`

Hosts that harness instances run on. SSH/transport config lives here (it was
lifted out of `instances` in the machine-split migration).

| Column | Type | Description |
|--------|------|-------------|
| `id` | TEXT PK | Unique machine identifier |
| `name` | TEXT | Unique human-readable name |
| `emoji` | TEXT | Optional UI accent |
| `hostname` | TEXT | Network address (Tailscale name, IP, …) |
| `os` | TEXT | `GOOS` (e.g. `linux`, `darwin`) |
| `arch` | TEXT | `GOARCH` (e.g. `amd64`, `arm64`) |
| `transport` | TEXT | `local` or `ssh` (default: `local`) |
| `ssh_user` | TEXT | SSH username |
| `ssh_key_path` | TEXT | Path to SSH private key |
| `ssh_port` | INTEGER | SSH port (default: 22) |
| `default_working_dir` | TEXT | Default working directory for instances |
| `user` | TEXT | Runner-side OS user (display only) |
| `notes` | TEXT | Free-form notes |
| `runner_token_hash` | TEXT | Hashed runner auth token |
| `last_seen_at` | DATETIME | Last runner check-in |

### `instances`

Harness deployments on a machine.

| Column | Type | Description |
|--------|------|-------------|
| `id` | TEXT PK | Unique instance identifier |
| `harness_type` | TEXT | Harness type (e.g. `claude_code`, `codex`) |
| `name` | TEXT | Human-readable name |
| `machine_id` | TEXT | References `machines.id` (cascade delete) |
| `working_dir` | TEXT | Working directory (overrides machine default) |
| `max_concurrent_sessions` | INTEGER | Max concurrent sessions (default: 1) |
| `enabled` | INTEGER | 0/1 flag (default: 1) |

### `instance_credentials`

Binds credentials to instances with priority ordering.

| Column | Type | Description |
|--------|------|-------------|
| `instance_id` | TEXT | References `instances.id` (cascade delete) |
| `credential_id` | TEXT | Credential identifier |
| `priority` | INTEGER | Priority order (0 = primary, 1+ = fallbacks) |
| `enabled` | INTEGER | 0/1 flag (default: 1) |

Primary key is `(instance_id, credential_id)`.

### `runner_enrollments`

One-time-use tokens that let a runner self-register a machine.

| Column | Type | Description |
|--------|------|-------------|
| `id` | TEXT PK | Enrollment identifier |
| `passphrase_hash` | TEXT | Hashed one-time passphrase (unique) |
| `expires_at` | DATETIME | Expiry time |
| `used_at` | DATETIME | When redeemed (null until consumed) |
| `consumed_machine_id` | TEXT | Machine that redeemed it (FK, set null on delete) |

### `harness_types`

Display metadata for each harness type.

| Column | Type | Description |
|--------|------|-------------|
| `name` | TEXT PK | Harness type identifier |
| `label` | TEXT | Human-readable label |
| `emoji` | TEXT | Unicode emoji |
| `image` | TEXT | Image URL or path |
| `updated_at` | DATETIME | Last update time |

## API

### Store lifecycle

| Function | Description |
|----------|-------------|
| `Open(dbPath)` | Open or create database, apply WAL mode, run migrations |
| `Close()` | Close database connection |
| `DB()` | Access underlying `*sql.DB` |

### Instance operations

| Method | Description |
|--------|-------------|
| `CreateInstance(inst)` | Register a new instance |
| `GetInstance(id)` | Get by ID |
| `GetInstanceByName(name)` | Get by name |
| `GetInstanceWithMachine(id)` | Get by ID with the `Machine` pointer populated |
| `ListInstances()` | All instances, ordered by name |
| `ListInstancesByHarness(type)` | Enabled instances for a harness type |
| `ListInstancesByMachine(machineID)` | Instances on a specific machine |
| `UpdateInstance(inst)` | Update configuration |
| `DeleteInstance(id)` | Remove instance (cascades to credentials) |
| `SetInstanceEnabled(id, enabled)` | Toggle enabled status |
| `CountInstances()` | Total count |
| `CountEnabledInstances()` | Enabled count |

### Credential binding operations

| Method | Description |
|--------|-------------|
| `BindCredential(ic)` | Associate credential with instance (upsert) |
| `UnbindCredential(instanceID, credentialID)` | Remove binding |
| `ListInstanceCredentials(instanceID)` | All bindings for an instance, ordered by priority |
| `ListCredentialInstances(credentialID)` | All instances using a credential |
| `GetCredentialBinding(instanceID, credentialID)` | Get specific binding |
| `SetCredentialEnabled(instanceID, credentialID, enabled)` | Toggle binding |
| `UpdateCredentialPriority(instanceID, credentialID, priority)` | Update priority |
| `ClearInstanceCredentials(instanceID)` | Remove all bindings for an instance |
| `CountCredentialBindings(instanceID)` | Count bindings |

### Machine operations

| Method | Description |
|--------|-------------|
| `CreateMachine(m)` | Register a new machine |
| `GetMachine(id)` | Get by ID |
| `GetMachineByName(name)` | Get by name |
| `ListMachines()` | All machines, ordered by name |
| `UpdateMachine(m)` | Update configuration |
| `DeleteMachine(id)` | Remove machine (cascades to instances) |
| `SetMachineRunnerTokenHash(id, hash)` | Set the runner auth token hash |
| `GetMachineByRunnerTokenHash(hash)` | Look up a machine by runner token hash |
| `TouchMachineLastSeen(id)` | Update `last_seen_at` to now |

### Runner enrollment operations

| Method | Description |
|--------|-------------|
| `MintEnrollment(ttl)` | Create a one-time enrollment, returns the passphrase |
| `ConsumeEnrollment(passphrase, machineID)` | Redeem an enrollment for a machine |
| `PurgeExpiredEnrollments()` | Delete expired enrollments, returns count |

### Harness type operations

| Method | Description |
|--------|-------------|
| `UpsertHarnessType(ht)` | Insert or update harness type metadata |
| `GetHarnessType(name)` | Get metadata |
| `ListHarnessTypes()` | All harness types, ordered by name |
| `DeleteHarnessType(name)` | Remove entry |

## Dependencies

- [`llm-bridge`](https://github.com/kayushkin/llm-bridge) -- canonical types (`msg.Instance`, `msg.InstanceCredential`, `msg.Harness`, `msg.Transport`)
- [`modernc.org/sqlite`](https://pkg.go.dev/modernc.org/sqlite) -- pure-Go SQLite driver (no CGO)

## Part of the llm-bridge ecosystem

This store is one of several optional SQLite-backed libraries that [llm-bridge-server](https://github.com/kayushkin/llm-bridge-server) can compose. See the [llm-bridge README](https://github.com/kayushkin/llm-bridge) for the full picture.
