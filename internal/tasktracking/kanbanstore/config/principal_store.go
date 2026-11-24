package config

// PrincipalIDPattern is the shape of an id principal-store mints —
// principal_000001, never a bare uuid. It is declared once and read twice: by
// the entity-type registry, so reference resolvers know which ids to probe
// principal-store with, and by PUT /api/cards/{id}/assignments/{principal_id},
// which refuses an id of any other shape before it calls principal-store.
const PrincipalIDPattern = `principal_\d{6,}`
