package server

// file-store as the other modules in this process call it, in Go: kanban-store
// keeps card attachments there and mailstack keeps mail attachments there.
// Each call goes through the module's own checks and limit
// (files.Server.Accept and the rest), as a request to its routes did.
// Until EnableFiles has run each call fails naming that, as the routes
// answered 503, and kanban-store and mailstack answer it as a file-store that
// could not be reached.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/kayushkin/llm-bridge-server/internal/config"
	"github.com/kayushkin/llm-bridge-server/internal/filestorage/files"
	mailfilestore "github.com/kayushkin/llm-bridge-server/internal/mail/filestore"
	issuesfiles "github.com/kayushkin/llm-bridge-server/internal/tasktracking/issues/filestore"
)

// errFileStoreOff is a call to file-store on a server that never enabled it.
var errFileStoreOff = errors.New("file-store is off: " + config.PostgresURLEnvironmentVariable + " is not set, so this server keeps no files")

// fileStoreModule is the module the calls go to, or errFileStoreOff.
func (s *Server) fileStoreModuleForCalls() (*files.Server, error) {
	files := s.fileStoreCalls.Load()
	if files == nil {
		return nil, errFileStoreOff
	}
	return files, nil
}

// requestForFileStore is a GET carrying only the reader's Range and
// conditional headers, so nothing else the reader sent changes file-store's
// answer.
func requestForFileStore(ctx context.Context, reader *http.Request) *http.Request {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "/", nil)
	if err != nil {
		panic(fmt.Sprintf("a GET of / always builds: %v", err))
	}
	for _, name := range []string{"Range", "If-Range", "If-None-Match", "If-Modified-Since"} {
		if value := reader.Header.Get(name); value != "" {
			request.Header.Set(name, value)
		}
	}
	return request
}

// serveFileStoreContent serves a live file onto w, or answers ErrNotFound
// (as notFound) with nothing written.
func (s *Server) serveFileStoreContent(w http.ResponseWriter, reader *http.Request, fileID string, inline bool, notFound error) error {
	fileServer, err := s.fileStoreModuleForCalls()
	if err != nil {
		return err
	}
	file, err := fileServer.LiveFile(fileID)
	if errors.Is(err, files.ErrNotFound) {
		return notFound
	}
	if err != nil {
		return err
	}
	inlineQuery := "false"
	if inline {
		inlineQuery = "true"
	}
	fileServer.ServeLiveFile(w, requestForFileStore(reader.Context(), reader), file, inlineQuery)
	return nil
}

// kanbanFileStore is file-store as kanban-store's attachments call it.
type kanbanFileStore struct {
	server *Server
}

// kanbanFileRecord is file-store's record as kanban-store passes it on: the
// whole record, encoded as file-store's routes encode it.
func kanbanFileRecord(file *files.File) (*issuesfiles.File, error) {
	encoded, err := json.Marshal(file)
	if err != nil {
		return nil, err
	}
	return issuesfiles.FileOfRecord(encoded)
}

func (fileStore kanbanFileStore) Upload(content io.Reader, contentLength int64, contentType, filename, cardID, uploadedByPrincipalID string) (*issuesfiles.File, error) {
	module, err := fileStore.server.fileStoreModuleForCalls()
	if err != nil {
		return nil, err
	}
	stored, err := module.Accept(content, contentLength, contentType, files.NewFile{
		Filename: filename, OwnerService: issuesfiles.OwnerService, OwnerRef: cardID, UploadedByPrincipalID: uploadedByPrincipalID,
	})
	var refusal *files.Refusal
	if errors.As(err, &refusal) {
		return nil, &issuesfiles.RefusalError{Status: refusal.Status, Body: refusal.Body()}
	}
	if err != nil {
		return nil, err
	}
	return kanbanFileRecord(stored)
}

func (files kanbanFileStore) FilesOfCard(cardID string) (map[string]*issuesfiles.File, error) {
	return files.filesOfCard(cardID, false)
}

func (files kanbanFileStore) EveryFileOfCard(cardID string) (map[string]*issuesfiles.File, error) {
	return files.filesOfCard(cardID, true)
}

func (fileStore kanbanFileStore) filesOfCard(cardID string, includeDeleted bool) (map[string]*issuesfiles.File, error) {
	module, err := fileStore.server.fileStoreModuleForCalls()
	if err != nil {
		return nil, err
	}
	byID := map[string]*issuesfiles.File{}
	for offset := 0; ; offset += files.MaximumFilesListed {
		page, err := module.ListFiles(files.Filter{OwnerService: issuesfiles.OwnerService, OwnerRef: cardID,
			IncludeDeleted: includeDeleted, Limit: files.MaximumFilesListed, Offset: offset})
		if err != nil {
			return nil, err
		}
		for _, stored := range page {
			file, err := kanbanFileRecord(stored)
			if err != nil {
				return nil, err
			}
			byID[file.ID] = file
		}
		if len(page) < files.MaximumFilesListed {
			return byID, nil
		}
	}
}

func (files kanbanFileStore) ServeContent(w http.ResponseWriter, r *http.Request, fileID string, inline bool) error {
	return files.server.serveFileStoreContent(w, r, fileID, inline, issuesfiles.ErrNotFound)
}

func (fileStore kanbanFileStore) Delete(fileID string, purge bool) error {
	module, err := fileStore.server.fileStoreModuleForCalls()
	if err != nil {
		return err
	}
	if purge {
		err = module.PurgeFile(fileID)
	} else {
		err = module.DeleteFile(fileID)
	}
	if errors.Is(err, files.ErrNotFound) {
		return issuesfiles.ErrNotFound
	}
	return err
}

// mailstackFileStore is file-store as mailstack's attachments call it.
type mailstackFileStore struct {
	server *Server
}

func (fileStore mailstackFileStore) Upload(content []byte, contentType, filename, ownerRef string) (*mailfilestore.File, error) {
	module, err := fileStore.server.fileStoreModuleForCalls()
	if err != nil {
		return nil, err
	}
	stored, err := module.Accept(bytes.NewReader(content), int64(len(content)), contentType, files.NewFile{
		Filename: filename, OwnerService: mailfilestore.OwnerService, OwnerRef: ownerRef,
	})
	var refusal *files.Refusal
	if errors.As(err, &refusal) {
		return nil, &mailfilestore.RefusalError{Status: refusal.Status, Body: refusal.Body()}
	}
	if err != nil {
		return nil, fmt.Errorf("upload %s to file-store: %w", filename, err)
	}
	return &mailfilestore.File{ID: stored.ID, SHA256: stored.SHA256, SizeBytes: stored.SizeBytes, ContentType: stored.ContentType, Filename: stored.Filename}, nil
}

func (files mailstackFileStore) ServeContent(w http.ResponseWriter, r *http.Request, fileID string, inline bool) error {
	return files.server.serveFileStoreContent(w, r, fileID, inline, mailfilestore.ErrNotFound)
}

func (fileStore mailstackFileStore) Purge(fileID string) error {
	module, err := fileStore.server.fileStoreModuleForCalls()
	if err != nil {
		return err
	}
	err = module.PurgeFile(fileID)
	if errors.Is(err, files.ErrNotFound) {
		return mailfilestore.ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("purge %s from file-store: %w", fileID, err)
	}
	return nil
}
