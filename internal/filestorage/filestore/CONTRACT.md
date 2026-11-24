# file-store contract

`127.0.0.1:8317`. Routes are rooted at `/`. Every route but `GET /health` needs
`X-File-Store-Service-Token`; without it, or with a wrong one, the answer is
**401**. A refusal is `{"error":"…"}`.

| Method | Path | Answer |
|---|---|---|
| `GET` | `/health` | `{"status":"ok","files":…,"blobs":…,"blob_bytes":…}`. Open |
| `GET` | `/limits` | `{"maximum_file_bytes":…,"inline_content_types":[…]}` — what an upload form must not hardcode |
| `POST` | `/files?filename=…&owner_service=…&owner_ref=…[&uploaded_by_principal_id=…]` | The body is the bytes; `Content-Type` is what they are declared to be. **201** and the file |
| `GET` | `/files?owner_service=…&owner_ref=…&sha256=…&include_deleted=…&limit=…&offset=…` | Files, oldest first. `limit` 1–500, default 100 |
| `GET` | `/files/{id}` | The record. `?include_deleted=true` reads a deleted one |
| `GET` | `/files/{id}/content[?inline=true]` | The bytes. Range requests work. **404** for a deleted file |
| `DELETE` | `/files/{id}` | **204**. Reversible: the row and the bytes stay |
| `POST` | `/files/{id}/restore` | **200** and the file, as it was |
| `DELETE` | `/files/{id}?hard=true` | **204**. Destroys the row, live or deleted, and the bytes when no other row names them |
| `GET` | `/settings` | This service's configuration (`msg.ServiceSettings`); the token as set-or-not only |
| `PUT` | `/settings/{key}` | `{"value":"…"}` for `maximum_file_bytes` or `inline_content_types`; no restart |

## The record

```json
{"id":"file_000001","sha256":"…","size_bytes":81234,
 "content_type":"application/pdf","detected_content_type":"application/pdf",
 "filename":"invoice.pdf","owner_service":"kanban-store","owner_ref":"<card id>",
 "uploaded_by_principal_id":"principal_000004","created_at":1789770000}
```

- `owner_service` and `owner_ref` say who put the file here and for what of
  theirs. Both are required. They are labels, not foreign keys.
- `content_type` is the declared type, parameters dropped. `detected_content_type`
  is what the first 512 bytes look like. Neither is refused for disagreeing.
- `uploaded_by_principal_id` is checked for shape only. The caller has already
  authenticated that principal and vouches for it.

## Uploading

**400**: no `filename`, or one that is not a plain name (a path separator, a
control character, `.` or `..`, over 255 bytes); no `owner_service` or
`owner_ref`; no readable `Content-Type`; no bytes. **413**: more than
`maximum_file_bytes`, whether `Content-Length` said so or the copy found out.
After any refusal nothing is kept, on disk or in the database.

The same bytes uploaded twice are two files and one blob.

## Downloading

Every answer carries `X-Content-Type-Options: nosniff`,
`Content-Security-Policy: sandbox; default-src 'none'`,
`Cache-Control: private, no-store` and the hash as `ETag`.

A file is served as `application/octet-stream` with
`Content-Disposition: attachment` **unless** the caller asks `?inline=true` and
the file's declared type is in `inline_content_types`; then it is served as
that type, `inline`. A type a browser runs — `text/html`, `image/svg+xml`, XML,
JavaScript — cannot be put on that list: `PUT /settings/inline_content_types`
refuses it with **400**.
