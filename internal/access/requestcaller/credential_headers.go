package requestcaller

// ServiceTokenHeader carries LLMBRIDGE_SERVICE_TOKEN from an internal service.
const ServiceTokenHeader = "X-LLM-Bridge-Service-Token"

// PeerTokenHeader carries a peer token: another llm-bridge-server acting here
// as the peer principal this server issued the token for (peers.go).
const PeerTokenHeader = "X-LLM-Bridge-Peer-Token"

// PeerUserHeader names, beside a peer token, which of the calling server's
// own principals a request is for: its principal-store id there (phase 2 of
// design note d107154a). This server acts for that user as the local
// principal its administrator mapped the pair to (peer_users.go), allowed only
// what both principals' grants allow. It means nothing without a peer token,
// and the calling server sets it itself, never from a client's header.
const PeerUserHeader = "X-LLM-Bridge-Peer-User"

// PrincipalIdentityHeader is the header the internal service names a person
// in beside its service token, and the one reference-store's in-process
// lookups name their caller in. No store reads it from a client.
const PrincipalIdentityHeader = "X-Principal-Id"

// PeerConnectionCredentialHeader carries a connection's credential on
// connection calls made by https and when opening a socket.
const PeerConnectionCredentialHeader = "X-LLM-Bridge-Peer-Connection"
