# skill-store

SQLite-backed registry for Claude Code / Agent `SKILL.md` skills. Ingests
upstream GitHub repos by cloning them, walking for `SKILL.md`, and recording
each skill's metadata plus copying its sibling files into the store's data
directory.

- **Port:** `:8301`
- **Data dir:** `~/.config/skill-store/` (`skill-store.db`, `skills/<id>/`, `repos/<source>/`)
- **Systemd unit:** `skill-store.service` (user)
- **Binary:** `~/bin/skill-store`

## Environment

- `SKILL_STORE_ADDR` — listen address, default `:8301`
- `SKILL_STORE_DATA_DIR` — default `~/.config/skill-store`; holds `skill-store.db`, `skills/<id>/` and `repos/<source>/`
- `SKILL_STORE_INSTALL_DIR` — where an installed skill is copied, default `~/.claude/skills`

All three are declared in `settings.go`, which is the source both commands read
and which `GET /settings` describes with the values in force. A set
`SKILL_STORE_ADDR…`, `SKILL_STORE_DATA_DIR…` or `SKILL_STORE_INSTALL_DIR…`
variable that is not declared there stops the process at boot. `SKILL_STORE_URL`
is not one of these: it is what other services read to find this one, so it is
deliberately outside the owned prefixes.

## Seed sources

Initial registered sources (see `cmd/seed`):

| Name          | Repo                                              | Tier       | License |
|---------------|---------------------------------------------------|------------|---------|
| `anthropics`  | `anthropics/skills`                               | official   | —       |
| `antigravity` | `sickn33/antigravity-awesome-skills`              | community  | MIT     |

Add more with `POST /sources` or by extending `cmd/seed/main.go`.

## Deploy

```bash
./deploy.sh
```

Then, on first run:

```bash
go run ./cmd/seed
```

## API

```
GET    /health
GET    /settings                    the declared settings and the values in force

GET    /sources
POST   /sources                     {name, repo_url, branch?, subpath?, trust_tier?, license?}
GET    /sources/{id}
DELETE /sources/{id}
POST   /sources/{id}/sync           → {found, inserted, updated, pruned, commit}

GET    /skills                      ?source=NAME  ?source_id=N  ?q=text  ?limit=N
GET    /skills/{id}
DELETE /skills/{id}
GET    /skills/{id}/files
GET    /skills/{id}/files/{path...}
```
