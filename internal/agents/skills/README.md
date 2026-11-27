# skill-store

The registry of Claude Code / Agent `SKILL.md` skills. It ingests upstream git
repositories by cloning them (or reads a directory on the host for a local
source), walks them for `SKILL.md`, records each skill's metadata and text, and
copies its sibling files into the store's data directory. It installs a skill
by copying it into the install directory, and seeds installed skills to
runners.

It is a module of llm-bridge-server, mounted at `/skills/`: the same paths
as the standalone service had, behind that prefix, as operator routes. Runners
reach the four seed routes (`GET /seed/manifest`, `GET /versions/{vid}/content`,
`POST /seed/observe`, `POST /seed/drift`) through `/api/skill-store/` with their
runner token. The standalone service, its unit, deploy script and settings are
gone.

## Where the records and the bytes live

- **Postgres**, module schema `skills`, role `skills_module`, built by the
  migrations in `migrations/`: sources, skills (with each `SKILL.md`'s full
  text), installations, every seed file version with its bytes (`bytea`),
  machine seed profiles and machine seed state.
- **Disk**, under the data directory the server is configured with: a skill's
  sibling files in `skills/<id>/` and cloned sources in `repos/<source>/`.
- **Disk**, the install directory: installed skills, one folder per skill name.

The server names both directories in its settings; the package guesses
neither. Ids are plain integers, as they always were: bundle-store members and
grant-store hold skill ids, and runners hold version ids from the manifest.

## Moving a standalone store in

`ImportStandaloneDatabase` copies every row of a standalone `skill-store.db`
into an empty module, keeping every id, time, NULL and byte, in one
transaction, and numbers on after the highest id the standalone store handed
out. It copies rows only: point the server's data and install directories at
the standalone store's folders so the imported rows find their files.

## Seed sources

`DefaultSources()` is the canonical initial registry:

| Name          | Repo                                              | Tier       | License |
|---------------|---------------------------------------------------|------------|---------|
| `anthropics`  | `anthropics/skills`                               | official   | —       |
| `antigravity` | `sickn33/antigravity-awesome-skills`              | community  | MIT     |

Add more with `POST /skills/sources`.

## API

Relative to the `/skills/` prefix:

```
GET    /health
GET    /sources
POST   /sources                     {name, kind?, repo_url | local_path, branch?, subpath?, trust_tier?, license?}
GET    /sources/{id}
DELETE /sources/{id}
POST   /sources/{id}/sync           → {found, inserted, updated, pruned, commit}

GET    /skills                      ?source=NAME  ?source_id=N  ?q=text  ?tag=T  ?installed=true  ?limit=N
GET    /skills/{id}
DELETE /skills/{id}
GET    /skills/{id}/files
GET    /skills/{id}/files/{path...}
POST   /skills/{id}/install         ?force=true
POST   /skills/{id}/uninstall
GET    /installations

GET    /seed/profile                ?machine_id=M
PUT    /seed/profile                ?machine_id=M  {enabled}
GET    /seed/manifest               ?machine_id=M
GET    /versions/{vid}/content
POST   /seed/observe                {machine_id, skill_id, relpath, sha256}
POST   /seed/drift                  {machine_id, skill_id, relpath, content, note?}
```

`?q=` and `?tag=` match as the SQLite store's `LIKE` did: case-insensitive,
`%` and `_` as wildcards, no escape character. Lists sort in byte order.
