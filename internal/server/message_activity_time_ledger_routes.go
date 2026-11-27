package server

// message-store, activity-store and time-ledger-store, moved in from their
// standalone services: the same routes under /messages/,
// /online-activity/ and /time-ledger/, answered from this process.
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
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/messages"
	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/onlineactivity"
	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/timeledger"
	"github.com/kayushkin/llm-bridge-server/internal/filestorage/files"
)

// Where each store is mounted.
const (
	messagesMountPrefix       = "/messages"
	onlineActivityMountPrefix = "/online-activity"
	timeLedgerMountPrefix     = "/time-ledger"
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
	files := s.fileStoreCalls.Load()
	if files == nil {
		return errors.New("activity-store: EnableFileStore must run first, since activity-store keeps its attachments' bytes in file-store")
	}
	messageStore := messages.New(modules.Messages)
	activities := onlineactivity.New(modules.Activity, ActivityAttachmentFilesIn(files))
	timeLedger := timeledger.New(modules.TimeLedger)
	messageMux := http.NewServeMux()
	messages.RegisterHandlers(messageMux, messageStore)
	activityMux := http.NewServeMux()
	onlineactivity.RegisterHandlers(activityMux, activities)
	timeLedgerMux := http.NewServeMux()
	timeledger.RegisterHandlers(timeLedgerMux, timeLedger)
	s.referenceReadable.messages.Store(messageStore)
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
	return s.messageActivityOrTimeLedgerRoute(messagesMountPrefix, "message-store", "message_store_off",
		func(stores *messageActivityAndTimeLedgerStores) http.Handler { return stores.messageHandler })
}

func (s *Server) activityStoreRoute() http.HandlerFunc {
	return s.messageActivityOrTimeLedgerRoute(onlineActivityMountPrefix, "activity-store", "activity_store_off",
		func(stores *messageActivityAndTimeLedgerStores) http.Handler { return stores.activityHandler })
}

func (s *Server) timeLedgerStoreRoute() http.HandlerFunc {
	return s.messageActivityOrTimeLedgerRoute(timeLedgerMountPrefix, "time-ledger-store", "time_ledger_store_off",
		func(stores *messageActivityAndTimeLedgerStores) http.Handler { return stores.timeLedgerHandler })
}

// activityAttachmentOwnerService is the owner_service activity-store's files
// carry in file-store.
const activityAttachmentOwnerService = "activity-store"

// ActivityAttachmentFilesIn is file-store, the module in this process called
// in Go, as activity-store keeps attachment bytes: each file is owned by
// activity-store and the activity it came with. Each call goes through the
// same checks and size limit as file-store's own routes.
func ActivityAttachmentFilesIn(files *files.Server) onlineactivity.AttachmentFiles {
	return activityAttachmentFiles{files: files}
}

type activityAttachmentFiles struct {
	files *files.Server
}

func (attachments activityAttachmentFiles) Put(_ context.Context, content io.Reader, file onlineactivity.AttachmentFile) (string, error) {
	// file-store refuses an upload over its limit by its declared length, so
	// the length is given; attachments are voice clips and small pictures.
	body, err := io.ReadAll(content)
	if err != nil {
		return "", fmt.Errorf("reading attachment %s: %w", file.FileName, err)
	}
	kept, err := attachments.files.Accept(bytes.NewReader(body), int64(len(body)), file.ContentType,
		files.NewFile{Filename: file.FileName, OwnerService: activityAttachmentOwnerService, OwnerRef: file.ActivityID})
	var refusal *files.Refusal
	if errors.As(err, &refusal) {
		return "", fmt.Errorf("file-store answered %d keeping attachment %s: %s", refusal.Status, file.FileName, refusal.Message)
	}
	if err != nil {
		return "", fmt.Errorf("file-store, keeping attachment %s: %w", file.FileName, err)
	}
	return kept.ID, nil
}

func (attachments activityAttachmentFiles) Open(_ context.Context, fileID string) (io.ReadCloser, error) {
	_, blob, err := attachments.files.OpenLiveContent(fileID)
	if err != nil {
		return nil, fmt.Errorf("file-store, reading %s: %w", fileID, err)
	}
	return blob, nil
}

func (attachments activityAttachmentFiles) Purge(_ context.Context, fileID string) error {
	if err := attachments.files.PurgeFile(fileID); err != nil {
		return fmt.Errorf("file-store, purging %s: %w", fileID, err)
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
