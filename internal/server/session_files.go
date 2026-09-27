package server

// Files shared into a session, by the user or by the session's own agent.
//
// file-store keeps the bytes. This server keeps which session a file belongs to
// (the session_files table), copies the bytes to a directory the agent can read
// them from, and records the share as a session_file event, so every client
// draws it at the point in the conversation it happened.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/kayushkin/llm-bridge-server/internal/filestoreclient"
	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
)

// handleShareSessionFile is POST /sessions/{id}/files?filename=<name>. The
// body is the bytes and Content-Type is what they are, as file-store takes
// them. Answers 201 and the msg.SessionFile.
func (s *Server) handleShareSessionFile(w http.ResponseWriter, r *http.Request) {
	if s.fileStore == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "session_files_off", "LLMBRIDGE_FILE_STORE_URL is not set, so this server keeps no session files")
		return
	}
	sessionID := r.PathValue("id")
	session, err := s.store.GetSession(sessionID)
	if err != nil {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}
	filename := r.URL.Query().Get("filename")
	if problem := plainFilenameProblem(filename); problem != "" {
		writeJSONError(w, http.StatusBadRequest, "invalid_filename", problem)
		return
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_content_type", "Content-Type must say what the bytes are: "+err.Error())
		return
	}
	sessionDirectory, err := s.sessionFilesDirectoryOf(session.SessionID)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_session_id", err.Error())
		return
	}

	sharedBy := msg.SessionFileSharedByUser
	if caller, ok := callerOfRequest(r); ok && caller.fileSharingAgentOfSessionID != "" {
		sharedBy = msg.SessionFileSharedByAgent
	}

	// Refused before the bytes are spooled, not after: file-store would refuse
	// an oversized file anyway, but only once it had been written to disk here.
	limits, err := s.fileStore.Limits()
	if err != nil {
		writeFileStoreError(w, err)
		return
	}

	// Spooled to the session's directory first, because the same bytes go to
	// two places — file-store and the agent's copy — and a request body can be
	// read once.
	if err := os.MkdirAll(sessionDirectory, 0o700); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "session_files_directory", err.Error())
		return
	}
	spool, err := os.CreateTemp(sessionDirectory, ".incoming-*")
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "session_files_directory", err.Error())
		return
	}
	spoolPath := spool.Name()
	keepSpool := false
	defer func() {
		spool.Close()
		if !keepSpool {
			os.Remove(spoolPath)
		}
	}()
	sizeBytes, err := io.Copy(spool, http.MaxBytesReader(w, r.Body, limits.MaximumFileBytes))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeJSONError(w, http.StatusRequestEntityTooLarge, "file_too_large",
				fmt.Sprintf("a session file may be at most %d bytes (file-store's maximum_file_bytes)", limits.MaximumFileBytes))
			return
		}
		writeJSONError(w, http.StatusBadRequest, "upload_interrupted", err.Error())
		return
	}
	if _, err := spool.Seek(0, io.SeekStart); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "session_files_directory", err.Error())
		return
	}
	stored, err := s.fileStore.Upload(spool, sizeBytes, mediaType, filename, session.SessionID)
	if err != nil {
		writeFileStoreError(w, err)
		return
	}

	// <session>/<file id>/<filename>: the file id keeps two uploads of the same
	// name apart, and the name stays exactly what the sharer called it.
	fileDirectory := filepath.Join(sessionDirectory, stored.ID)
	if err := os.MkdirAll(fileDirectory, 0o700); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "session_files_directory", err.Error())
		return
	}
	agentPath := filepath.Join(fileDirectory, filename)
	if err := os.Rename(spoolPath, agentPath); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "session_files_directory", err.Error())
		return
	}
	keepSpool = true

	record := store.SessionFile{
		FileID:    stored.ID,
		SessionID: session.SessionID,
		Filename:  filename,
		MediaType: stored.ContentType,
		SizeBytes: stored.SizeBytes,
		SharedBy:  sharedBy,
		Path:      agentPath,
		CreatedAt: time.Now().UTC(),
	}
	if err := s.store.InsertSessionFile(record); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "record_session_file", err.Error())
		return
	}
	event := msg.Event{
		Type:            msg.EventSessionFile,
		BridgeSessionID: session.SessionID,
		Timestamp:       record.CreatedAt,
		SessionFile:     &record,
	}
	if _, err := s.harness.BroadcastEvent(&event); err != nil {
		// The file is kept and listed; only the timeline entry is missing, and
		// the caller has to know that rather than find it out.
		writeJSONError(w, http.StatusInternalServerError, "record_session_file_event", fmt.Sprintf(
			"%s is stored and listed at GET /sessions/%s/files, but its session_file event was not written: %v", record.FileID, session.SessionID, err))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	if err := json.NewEncoder(w).Encode(record); err != nil {
		log.Printf("[session-files] write %s answer: %v", record.FileID, err)
	}
}

// handleListSessionFiles is GET /sessions/{id}/files: every file shared into
// the session, oldest first.
func (s *Server) handleListSessionFiles(w http.ResponseWriter, r *http.Request) {
	session, err := s.store.GetSession(r.PathValue("id"))
	if err != nil {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}
	files, err := s.store.ListSessionFiles(session.SessionID)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "list_session_files", err.Error())
		return
	}
	writeJSON(w, files)
}

// handleSessionFileContent is GET /sessions/{id}/files/{file_id}/content,
// relaying file-store's answer — status, headers and bytes — unchanged. Its
// headers are what keep an upload from running as this origin
// (Content-Security-Policy: sandbox, nosniff, attachment unless ?inline=true
// and the type is on file-store's inline list), so none is dropped or added.
func (s *Server) handleSessionFileContent(w http.ResponseWriter, r *http.Request) {
	if s.fileStore == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "session_files_off", "LLMBRIDGE_FILE_STORE_URL is not set, so this server keeps no session files")
		return
	}
	session, err := s.store.GetSession(r.PathValue("id"))
	if err != nil {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}
	file, err := s.store.GetSessionFile(session.SessionID, r.PathValue("file_id"))
	if errors.Is(err, store.ErrSessionFileNotFound) {
		http.Error(w, "file not found", http.StatusNotFound)
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "read_session_file", err.Error())
		return
	}
	response, err := s.fileStore.Content(file.FileID, r.URL.Query().Get("inline") == "true", r.Header.Get("Range"))
	if err != nil {
		writeFileStoreError(w, err)
		return
	}
	defer response.Body.Close()
	for name, values := range response.Header {
		w.Header()[name] = values
	}
	w.WriteHeader(response.StatusCode)
	if _, err := io.Copy(w, response.Body); err != nil {
		log.Printf("[session-files] relay %s of %s: %v", file.FileID, session.SessionID, err)
	}
}

// sessionFilesDirectoryOf is where one session's files are copied. A session
// id is escaped to one path segment, and one that escapes to "." or ".." is
// refused rather than allowed to name the parent directory.
func (s *Server) sessionFilesDirectoryOf(sessionID string) (string, error) {
	segment := url.PathEscape(sessionID)
	if segment == "" || segment == "." || segment == ".." {
		return "", fmt.Errorf("session id %q cannot name a directory", sessionID)
	}
	return filepath.Join(s.cfg.SessionFilesDir, segment), nil
}

// plainFilenameProblem says why a filename cannot be used as one path
// segment, or "" when it can. The same rule file-store applies, checked here
// first because this server writes the name to disk itself.
func plainFilenameProblem(filename string) string {
	switch {
	case filename == "":
		return "filename is required"
	case len(filename) > 255:
		return "filename is longer than 255 bytes"
	case filename == "." || filename == "..":
		return "filename cannot be . or .."
	case strings.ContainsAny(filename, `/\`):
		return "filename cannot contain a path separator"
	case strings.IndexFunc(filename, unicode.IsControl) >= 0:
		return "filename cannot contain a control character"
	}
	return ""
}

// writeFileStoreError relays file-store's own refusal, or says it could not be
// reached.
func writeFileStoreError(w http.ResponseWriter, err error) {
	var refusal *filestoreclient.RefusalError
	if errors.As(err, &refusal) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(refusal.Status)
		w.Write(refusal.Body)
		return
	}
	writeJSONError(w, http.StatusBadGateway, "file_store_unavailable", err.Error())
}
