package permissiongate

import "github.com/kayushkin/llm-bridge-server/internal/httproutes"

// Mount registers the permission gate's routes on the server's mux: the
// prehooks and the sidecar that call it, the parked asks and their answers,
// the permission mode, and the hook registry, which needs hook-store and is
// left out without it.
func (s *Service) Mount(mux httproutes.Mux) {
	mux.HandleFunc("GET /sessions/{id}/hooks/pending", s.HandleListPendingHooks)
	mux.HandleFunc("POST /sessions/{id}/hooks/{request_id}/resolve", s.HandleResolveHook)
	mux.HandleFunc("POST /permission/cc-prehook/{bridge_id}", s.HandleCCPermissionPrehook)
	mux.HandleFunc("POST /permission/codex-prehook/{bridge_id}", s.HandleCCPermissionPrehook)
	mux.HandleFunc("POST /permission/devin-prehook/{bridge_id}", s.HandleCCPermissionPrehook)
	mux.HandleFunc("POST /sidecar/event/{bridge_id}", s.HandleSidecarEvent)
	mux.HandleFunc("POST /bridge/permission-mode", s.HandleSetGlobalPermissionMode)
	mux.HandleFunc("PUT /sessions/{id}/permission-mode", s.HandleSetSessionPermissionMode)
	mux.HandleFunc("POST /bridge/bypass-permissions", s.HandleSetBypassPermissions)
	mux.HandleFunc("PUT /sessions/{id}/bypass-permissions", s.HandleSetSessionBypass)
	if s.HookStore != nil {
		mux.HandleFunc("POST /hooks", s.HandleCreateHook)
		mux.HandleFunc("GET /hooks", s.HandleListHooks)
		mux.HandleFunc("GET /hook-options", s.HandleHookOptions)
		mux.HandleFunc("GET /hooks/{id}", s.HandleGetHook)
		mux.HandleFunc("PATCH /hooks/{id}", s.HandleUpdateHook)
		mux.HandleFunc("DELETE /hooks/{id}", s.HandleDeleteHook)
		mux.HandleFunc("POST /hooks/exec/{id}", s.HandleExecHook)
	}
}
