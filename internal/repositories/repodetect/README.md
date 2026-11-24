# repodetect

The repository-signature ruleset: a pure `Detect(path) → Signature` function plus tag
helpers. Part of the [llm-bridge](https://github.com/kayushkin/llm-bridge) ecosystem.

Given a repo path, it reads the manifests and reports which languages, frameworks,
build tools, and test tools that repo uses. Callers turn that into tags — which is
how [bundle-store](https://github.com/kayushkin/bundle-store) picks the skills and
tools a session should get.

## Why it is its own module

Consumers need the ruleset, not the registry. Before this split, `repodetect` lived
inside the `repo-store` service module, so anything that wanted one function had to
`require` a module whose root package carries a SQLite driver and an HTTP server.
[inber](https://github.com/kayushkin/inber) did exactly that to call `Detect` once.

A harness should depend on a library, not on a service. This module has **no
third-party dependencies** — stdlib only — so importing it costs nothing.

## Install

```bash
go get github.com/kayushkin/repodetect
```

## Usage

```go
import "github.com/kayushkin/repodetect"

sig, err := repodetect.Detect("/path/to/repo")
if err != nil {
    return err
}

sig.Languages        // ["go" "node" "typescript"]
sig.Frameworks       // ["react"]
sig.Tags()           // flattened, sorted, deduped — plus "web"/"frontend" rollups
sig.PrimaryLanguage() // "go" — for callers that need exactly one answer
```

## What it detects

Detection collects **every** applicable signal rather than stopping at the first
match, so a Go service with a React frontend reports both languages.

| Signal | Source |
|---|---|
| `go` | `go.mod` |
| `rust` | `Cargo.toml` |
| `python` | `pyproject.toml`, `requirements.txt`, `setup.py` |
| `node` | `package.json` |
| `typescript` | `tsconfig.json`, or a `typescript` dependency |
| `docker` | `Dockerfile` |
| frameworks | `package.json` deps: react, vue, svelte, next, angular, express/fastify/koa |
| build tools | vite (dep or `vite.config.*`) |
| test tools | playwright, cypress (each also implying `e2e`) |

A missing manifest is simply absent, never an error. A path that does not exist
returns the underlying `os` error.

## Known limit: detection is root-only

`Detect` reads manifests at the top level of the given path and nowhere else. A repo
whose frontend lives in a subdirectory is under-tagged — `argraphments`, a Go service
with a `web/` React app, reports `go` and nothing more.

This is a real gap for tag-driven bundle selection, not a stylistic one: the repo
gets a backend bundle and no frontend tooling. Fixing it means deciding how deep to
walk and how to weight a subdirectory signal against a root one, which is a design
question, not an oversight.

## API

| Symbol | Purpose |
|---|---|
| `Detect(path) (Signature, error)` | Inspect a repo's manifests |
| `Signature` | Languages, Frameworks, BuildTools, TestTools |
| `Signature.Tags() []string` | Flatten to a sorted, deduped tag list |
| `Signature.PrimaryLanguage() string` | One language, fixed preference: go, rust, python, node |
| `DedupeSorted([]string) []string` | Drop empties and duplicates, sort |
| `Union(a, b []string) []string` | Sorted, deduped union |
