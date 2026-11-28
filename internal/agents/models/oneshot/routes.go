package oneshot

import "github.com/kayushkin/llm-bridge-server/internal/httproutes"

// Mount registers the one-shot and image routes on the server's mux. A
// one-shot call on a named instance needs harness-store and is left out
// without it.
func (s *Service) Mount(mux httproutes.Mux) {
	if s.HarnessStore() != nil {
		mux.HandleFunc("POST /instances/{id}/oneshot", s.HandleInstanceOneShot)
	}
	mux.HandleFunc("POST /oneshot", s.HandleRoutedOneShot)
	mux.HandleFunc("POST /images", s.HandleGenerateImages)
	mux.HandleFunc("POST /sessions/{id}/images", s.HandleGenerateImagesIntoSession)
}
