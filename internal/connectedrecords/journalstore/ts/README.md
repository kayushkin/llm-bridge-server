# @kayushkin/journal-store-types

TypeScript types for journal-store's wire format, generated from the Go types
in the `journalstore` package (`entry.go`, `vocabulary.go`) with
[tygo](https://github.com/gzuidhof/tygo). The Go package is the source of
truth; do not edit `model.ts` by hand. Regenerate with `./generate-ts.sh` in
the package directory; `tygo.yaml` names the files it reads.

Source-only for now: install with `file:../journal-store/ts`.
