# @kayushkin/kanban-store-types

TypeScript types for kanban-store's wire format, generated from the Go types in
`internal/model` with [tygo](https://github.com/gzuidhof/tygo). The Go package
is the source of truth; do not edit `model.ts` by hand. Regenerate with
`./generate-ts.sh` at the repo root.

A card's `item` is a noteboard record passed through unchanged, so `CardView`
and `EntityCardView` take that type from `@kayushkin/noteboard-types` instead
of copying it. A board's `taxonomy` is llm-bridge's `ClassificationTaxonomy`,
taken the same way from `@kayushkin/llm-bridge-types`.

Source-only for now: install with `file:../kanban-store/ts`.
