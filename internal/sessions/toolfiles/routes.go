package toolfiles

import "github.com/kayushkin/llm-bridge-server/internal/httproutes"

// Mount registers the routes for what a session's tools touched on the server's mux: files its tools named, its git repositories and, with snapshot-store, the snapshots of edited files.
func (s *Service) Mount(mux httproutes.Mux) {
	mux.HandleFunc("GET /sessions/{id}/files-named-by-tools", s.HandleFilesNamedByTools)
	mux.HandleFunc("GET /sessions/{id}/files-named-by-tools/content", s.HandleFileNamedByToolsContent)
	mux.HandleFunc("GET /sessions/{id}/files-named-by-tools/image", s.HandleImageNamedByTools)
	mux.HandleFunc("GET /sessions/{id}/git/repos", s.HandleSessionGitRepos)
	mux.HandleFunc("GET /sessions/{id}/git", s.HandleSessionGit)
	if s.SnapshotStore() != nil {
		mux.HandleFunc("GET /sessions/{id}/tools/{tool_use_id}/snapshots", s.HandleGetSnapshots)
		mux.HandleFunc("GET /snapshots/blob/{sha}", s.HandleGetSnapshotBlob)
	}
}
