# reference-store

> **In llm-bridge-server.** This package is reference-store moved into the bridge, served under `/reference-store/`: the catalog is open, and lookups run as the verified caller from the request context. It looks up the stores the bridge runs in process, and the rest over HTTP at `reference_store.service_urls`. The type list stays in code (`types.go`); there is no table of types. Where this README says `:8343`, `X-Principal-Id` or a service token, it describes the standalone service.

What an id written in prose names, who may see it, and where its page is.

Chats and pages draw ids such as `prediction_000012`, `repo:dash` or `dash@8487d32` as chips. reference-store holds the one list of those reference types — how each id looks, which service owns it, which field names a record, a few fields worth showing, its page — and looks ids up in the stores that own them, for a caller, under that caller's access.

```bash
curl -s localhost:8343/types | jq '.types[0]'
curl -s -X POST localhost:8343/resolve -H 'X-Reference-Store-Service-Token: …' \
  -d '{"ids":["prediction_000012","repo:dash"]}'
```

It has no database. It trusts `X-Principal-Id`, so it listens on loopback and is reached through llm-bridge-server's gateway. `AGENTS.md` says how it works.

`go.mod` points at [llm-bridge](https://github.com/kayushkin/llm-bridge) in a folder beside this one, so clone both side by side before you build:

```bash
git clone https://github.com/kayushkin/llm-bridge
git clone https://github.com/kayushkin/reference-store
cd reference-store && go test ./...
```

Deployments may configure only their own services in `REFERENCE_STORE_SERVICE_URLS`. The catalog stays complete; resolving or searching an unconfigured service returns an error, never a missing-record answer. Missing page-site addresses leave page links empty. Configured addresses must be absolute HTTP or HTTPS URLs.

MIT licensed.
