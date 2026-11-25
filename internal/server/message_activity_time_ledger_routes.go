package server

// message-store, activity-store and time-ledger-store, moved in from their
// standalone services: the same routes under /message-store/,
// /activity-store/ and /time-ledger-store/, answered from this process.
//
// They follow the other connected_records stores (connected_records_routes.go):
// every record has an owner, each store narrows every read and write to the
// records the caller owns, or every owner's for an administrator and for this
// server's own service acting as itself, and the caller reaches the store in
// the request context, never from a header. A new record is owned by the
// principal that makes it, so the service acting as nobody cannot write one;
// an importer sends the service token beside the X-Principal-Id of the person
// whose messages, activity or time it files.
//
// None of the three runs a loop of its own: what fetches from a platform
// (message-import-*, activity-import-google, time-ledger-import and the rest)
// is a separate command that calls these routes, started by the scheduler.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/activitystore"
	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/messagestore"
	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/timeledgerstore"
	"github.com/kayushkin/llm-bridge-server/internal/inprocesshttp"
)

// Where each store is mounted.
const (
	messageStoreMountPrefix    = "/message-store"
	activityStoreMountPrefix   = "/activity-store"
	timeLedgerStoreMountPrefix = "/time-ledger-store"
)

// messageActivityAndTimeLedgerStores is the three stores as this server
// serves them.
type messageActivityAndTimeLedgerStores struct {
	messageHandler    http.Handler
	activityHandler   http.Handler
	timeLedgerHandler http.Handler
}

// MessageActivityAndTimeLedgerModules is each store's Postgres pool.
type MessageActivityAndTimeLedgerModules struct {
	Messages   *sql.DB
	Activity   *sql.DB
	TimeLedger *sql.DB
}

// EnableMessageActivityAndTimeLedgerStores serves message-store,
// activity-store and time-ledger-store from their modules. main calls it once,
// after EnableConnectedRecords (the stores share its caller rules) and
// EnableFileStore (activity-store keeps its attachments' bytes in file-store),
// and before the server listens. Until then their routes answer 503.
func (s *Server) EnableMessageActivityAndTimeLedgerStores(modules MessageActivityAndTimeLedgerModules) error {
	if s.connectedRecords.Load() == nil {
		return errors.New("message-store, activity-store and time-ledger-store: EnableConnectedRecords must run first; they are served the way its stores are")
	}
	if s.fileStoreHandler.Load() == nil {
		return errors.New("activity-store: EnableFileStore must run first, since activity-store keeps its attachments' bytes in file-store")
	}
	messages := messagestore.New(modules.Messages)
	activities := activitystore.New(modules.Activity, ActivityAttachmentFilesIn(s.FileStoreModuleHandler()))
	timeLedger := timeledgerstore.New(modules.TimeLedger)
	messageMux := http.NewServeMux()
	messagestore.RegisterHandlers(messageMux, messages)
	activityMux := http.NewServeMux()
	activitystore.RegisterHandlers(activityMux, activities)
	timeLedgerMux := http.NewServeMux()
	timeledgerstore.RegisterHandlers(timeLedgerMux, timeLedger)
	s.referenceReadable.messages.Store(messages)
	s.referenceReadable.activities.Store(activities)
	s.referenceReadable.timeLedger.Store(timeLedger)
	s.messageActivityAndTimeLedger.Store(&messageActivityAndTimeLedgerStores{
		messageHandler: messageMux, activityHandler: activityMux, timeLedgerHandler: timeLedgerMux,
	})
	return nil
}

// messageActivityOrTimeLedgerRoute answers one of the three stores' routes as
// the verified caller, as connectedRecordsRoute answers the others. A store
// this server has not opened answers 503 with offCode.
func (s *Server) messageActivityOrTimeLedgerRoute(mountPrefix, storeName, offCode string, handlerOf func(*messageActivityAndTimeLedgerStores) http.Handler) http.HandlerFunc {
	return s.connectedRecordsRoute(mountPrefix, storeName, offCode, func(*connectedRecordsStores) http.Handler {
		stores := s.messageActivityAndTimeLedger.Load()
		if stores == nil {
			return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				writeJSONError(w, http.StatusServiceUnavailable, offCode, storeName+" is not open on this server: EnableMessageActivityAndTimeLedgerStores was never called")
			})
		}
		return handlerOf(stores)
	}, false)
}

func (s *Server) messageStoreRoute() http.HandlerFunc {
	return s.messageActivityOrTimeLedgerRoute(messageStoreMountPrefix, "message-store", "message_store_off",
		func(stores *messageActivityAndTimeLedgerStores) http.Handler { return stores.messageHandler })
}

func (s *Server) activityStoreRoute() http.HandlerFunc {
	return s.messageActivityOrTimeLedgerRoute(activityStoreMountPrefix, "activity-store", "activity_store_off",
		func(stores *messageActivityAndTimeLedgerStores) http.Handler { return stores.activityHandler })
}

func (s *Server) timeLedgerStoreRoute() http.HandlerFunc {
	return s.messageActivityOrTimeLedgerRoute(timeLedgerStoreMountPrefix, "time-ledger-store", "time_ledger_store_off",
		func(stores *messageActivityAndTimeLedgerStores) http.Handler { return stores.timeLedgerHandler })
}

// activityAttachmentOwnerService is the owner_service activity-store's files
// carry in file-store.
const activityAttachmentOwnerService = "activity-store"

// ActivityAttachmentFilesIn is file-store, answered by handler (the file-store
// module's own routes, in this process), as activity-store keeps attachment
// bytes: each file is owned by activity-store and the activity it came with.
// Going through file-store's routes keeps its size limit and its checks.
func ActivityAttachmentFilesIn(handler http.Handler) activitystore.AttachmentFiles {
	return activityAttachmentFiles{client: inprocesshttp.Client(handler)}
}

type activityAttachmentFiles struct {
	client *http.Client
}

func (files activityAttachmentFiles) Put(ctx context.Context, content io.Reader, file activitystore.AttachmentFile) (string, error) {
	// file-store refuses an upload over its limit by its declared length, so
	// the length is sent; attachments are voice clips and small pictures.
	body, err := io.ReadAll(content)
	if err != nil {
		return "", fmt.Errorf("reading attachment %s: %w", file.FileName, err)
	}
	query := url.Values{"filename": {file.FileName}, "owner_service": {activityAttachmentOwnerService}, "owner_ref": {file.ActivityID}}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, inprocesshttp.BaseURL+"/files?"+query.Encode(), bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	request.ContentLength = int64(len(body))
	request.Header.Set("Content-Type", file.ContentType)
	response, err := files.client.Do(request)
	if err != nil {
		return "", fmt.Errorf("file-store, keeping attachment %s: %w", file.FileName, err)
	}
	defer response.Body.Close()
	answer, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("file-store, reading its answer about attachment %s: %w", file.FileName, err)
	}
	if response.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("file-store answered %d keeping attachment %s: %s", response.StatusCode, file.FileName, bytes.TrimSpace(answer))
	}
	var kept struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(answer, &kept); err != nil || kept.ID == "" {
		return "", fmt.Errorf("file-store answered 201 with no file id for attachment %s: %s", file.FileName, bytes.TrimSpace(answer))
	}
	return kept.ID, nil
}

func (files activityAttachmentFiles) Open(ctx context.Context, fileID string) (io.ReadCloser, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, inprocesshttp.BaseURL+"/files/"+url.PathEscape(fileID)+"/content", nil)
	if err != nil {
		return nil, err
	}
	response, err := files.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("file-store, reading %s: %w", fileID, err)
	}
	if response.StatusCode != http.StatusOK {
		defer response.Body.Close()
		answer, _ := io.ReadAll(io.LimitReader(response.Body, 1<<16))
		return nil, fmt.Errorf("file-store answered %d reading %s: %s", response.StatusCode, fileID, bytes.TrimSpace(answer))
	}
	return response.Body, nil
}

func (files activityAttachmentFiles) Purge(ctx context.Context, fileID string) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodDelete, inprocesshttp.BaseURL+"/files/"+url.PathEscape(fileID)+"?hard=true", nil)
	if err != nil {
		return err
	}
	response, err := files.client.Do(request)
	if err != nil {
		return fmt.Errorf("file-store, purging %s: %w", fileID, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		answer, _ := io.ReadAll(io.LimitReader(response.Body, 1<<16))
		return fmt.Errorf("file-store answered %d purging %s: %s", response.StatusCode, fileID, bytes.TrimSpace(answer))
	}
	return nil
}

// registerMessageActivityAndTimeLedgerRoutes mounts the three stores.
func (s *Server) registerMessageActivityAndTimeLedgerRoutes() {
	for _, mounted := range []struct {
		route    http.HandlerFunc
		patterns []string
	}{
		{s.messageStoreRoute(), messageStoreRoutePatterns},
		{s.activityStoreRoute(), activityStoreRoutePatterns},
		{s.timeLedgerStoreRoute(), timeLedgerStoreRoutePatterns},
	} {
		for _, pattern := range mounted.patterns {
			s.mux.HandleFunc(pattern, mounted.route)
		}
	}
}
