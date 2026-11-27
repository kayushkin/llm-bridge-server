# harness-store

Registry for harness instances, machines and credential bindings, kept in the Postgres module `harnesses` (role `harnesses_module`, migrations in `migrations/`). Part of the [llm-bridge](https://github.com/kayushkin/llm-bridge) ecosystem.

Tracks which harness instances are deployed across machines (local or SSH), which credentials are bound to each instance, and display metadata for harness types. This is configuration storage only -- runtime state (active sessions, credential slot occupancy) lives in [llm-bridge-server](https://github.com/kayushkin/llm-bridge-server).

## Where it lives

This package is internal to llm-bridge-server, so only code in this repo can import it.

## Usage

```go
import store "github.com/kayushkin/llm-bridge-server/internal/machines"

// db is the module's pool: the server provisions, migrates and opens the
// module (cmd/llm-bridge-server), then hands it here.
s := store.New(db)

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

`migrations/0001_create_machines_instances_credentials_enrollments_and_harness_types.sql` is the schema: `machines`, `instances`, `instance_credentials`, `runner_enrollments` and `harness_types`, with times as `timestamptz` (kept to the microsecond, handed back in UTC) and switches as `boolean`. Two things differ from the SQLite file it came from:

- `machines.user` is `operating_system_user`, since Postgres reserves `user`. `msg.Machine.User` is unchanged.
- No table declares a foreign key. SQLite never enforced the ones its file declared, and the live file holds rows that break them, so deleting a machine leaves its instances, and deleting an instance leaves its bindings, as before. Card 882d977a-dfaf-47da-9857-205285dba647 holds the decision to change that.

`llm-bridge-server import-harness-store -sqlite <copy of harness.db>` copies every row of the SQLite file into the module, keeping every id (`m_…`, `inst_…`, `enr_…`), and refuses a module that already holds one.

## API

### Store lifecycle

| Function | Description |
|----------|-------------|
| `Module()` | The schema, role and migrations the server provisions and migrates |
| `New(db)` | The store over the module's pool |
| `DB()` | The module's pool, for tests |

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
| `DeleteInstance(id)` | Remove instance (its bindings stay; call `ClearInstanceCredentials` first) |
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
| `DeleteMachine(id)` | Remove machine (its instances stay) |
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
- `postgresstorage` -- the module's schema, role and migrations; `modernc.org/sqlite` only to read the SQLite file an import copies

## Part of the llm-bridge ecosystem

This store is one of the Postgres modules that [llm-bridge-server](https://github.com/kayushkin/llm-bridge-server) can compose. See the [llm-bridge README](https://github.com/kayushkin/llm-bridge) for the full picture.
