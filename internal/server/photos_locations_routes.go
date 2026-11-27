package server

// photo-store and location-store moved in from the standalone services: the
// same routes under /photo-store/ and /location-store/, answered from this
// process. They are connected records stores like people-store: every record
// has an owner, and each store narrows every read and write to the records
// the caller owns, or every owner's for an administrator and for this
// server's own service acting as itself. The caller reaches the store in the
// request context, set by connectedRecordsRoute from the credential request
// authorization verified.
//
// photo-store keeps its thumbnails' bytes in the file-store module, and
// checks every person it is told is on a photo in the photo owner's
// people-store records, both in this process.

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/locations"
	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/photos"
	"github.com/kayushkin/llm-bridge-server/internal/filestorage/files"
)

// Where each store is mounted.
const (
	photoStoreMountPrefix    = "/photo-store"
	locationStoreMountPrefix = "/location-store"
)

// photosAndLocationsStores is the two stores as this server serves them.
type photosAndLocationsStores struct {
	photos          *photos.Store
	photoHandler    http.Handler
	locations       *locations.Store
	locationHandler http.Handler
}

// PhotosAndLocationsModules is each store's Postgres pool, and the directory
// location-store reads GeoNames' town dump from.
type PhotosAndLocationsModules struct {
	Photos            *sql.DB
	Locations         *sql.DB
	GeonamesDirectory string
}

// EnablePhotosAndLocations serves photo-store and location-store from their
// modules. main calls it once, after EnableFileStore (photo-store keeps its
// thumbnails there) and EnableConnectedRecords (it checks people in
// people-store), and before the server listens. Until then their routes
// answer 503.
func (s *Server) EnablePhotosAndLocations(modules PhotosAndLocationsModules) error {
	files := s.fileStoreModule.Load()
	if files == nil {
		return errors.New("photo-store: EnableFileStore must run first, since photo-store keeps its thumbnails in file-store")
	}
	connected := s.connectedRecords.Load()
	if connected == nil {
		return errors.New("photo-store: EnableConnectedRecords must run first, since photo-store checks every person it is told is on a photo in people-store")
	}
	if modules.Photos == nil || modules.Locations == nil {
		return errors.New("photo-store and location-store: both modules' pools are required")
	}
	if modules.GeonamesDirectory == "" {
		return errors.New("location-store: the GeoNames directory is required")
	}
	photoStore := photos.New(modules.Photos, PhotoThumbnailFiles(files, s.fileStoreLimits), calendarPeople{people: connected.people})
	photoMux := http.NewServeMux()
	photos.RegisterHandlers(photoMux, photoStore)

	locationStore := locations.New(modules.Locations, modules.GeonamesDirectory)
	locationMux := http.NewServeMux()
	locations.RegisterHandlers(locationMux, locationStore)

	s.photosAndLocations.Store(&photosAndLocationsStores{
		photos: photoStore, photoHandler: photoMux,
		locations: locationStore, locationHandler: locationMux,
	})
	return nil
}

// PhotoThumbnailFiles is file-store, in this process, as photo-store keeps
// its thumbnails: one file per photo, owner service photo-store, owner ref
// the photo's id, uploaded by the photo's owner. limits is file-store's size
// limit in force.
func PhotoThumbnailFiles(fileStore *files.Store, limits func() files.Limits) photos.ThumbnailFiles {
	return photoThumbnailFiles{files: fileStore, limits: limits, carry: files.CopyBlobs}
}

// PhotoThumbnailFilesCarrying is PhotoThumbnailFiles with KeepCopyOf putting
// each file in by carry: an import passes files.LinkBlobs to bring a
// standalone store's thumbnails in as hard links, which take no room.
func PhotoThumbnailFilesCarrying(files *files.Store, limits func() files.Limits, carry files.BlobCarry) photos.ThumbnailFiles {
	return photoThumbnailFiles{files: files, limits: limits, carry: carry}
}

type photoThumbnailFiles struct {
	files  *files.Store
	limits func() files.Limits
	carry  files.BlobCarry
}

// photoThumbnailOwnerService is the owner_service of every thumbnail file.
const photoThumbnailOwnerService = "photo-store"

func (thumbnails photoThumbnailFiles) newFile(ownerPrincipalID, photoID string) files.NewFile {
	return files.NewFile{ContentType: "image/jpeg", Filename: photoID + ".jpg",
		OwnerService: photoThumbnailOwnerService, OwnerRef: photoID, UploadedByPrincipalID: ownerPrincipalID}
}

func (thumbnails photoThumbnailFiles) Keep(_ context.Context, ownerPrincipalID, photoID string, jpeg []byte) (string, error) {
	file, err := thumbnails.files.Put(bytes.NewReader(jpeg), thumbnails.limits().MaximumFileBytes, thumbnails.newFile(ownerPrincipalID, photoID))
	if err != nil {
		return "", fmt.Errorf("file-store, keeping the thumbnail of %s: %w", photoID, err)
	}
	return file.ID, nil
}

func (thumbnails photoThumbnailFiles) KeepCopyOf(_ context.Context, ownerPrincipalID, photoID, path string) (string, error) {
	file, err := thumbnails.files.PutCarried(path, thumbnails.carry, thumbnails.newFile(ownerPrincipalID, photoID))
	if err != nil {
		return "", fmt.Errorf("file-store, carrying the thumbnail of %s: %w", photoID, err)
	}
	return file.ID, nil
}

func (thumbnails photoThumbnailFiles) Open(_ context.Context, fileID string) (io.ReadCloser, error) {
	file, err := thumbnails.files.Get(fileID, false)
	if err != nil {
		return nil, fmt.Errorf("file-store, thumbnail %s: %w", fileID, err)
	}
	if file.OwnerService != photoThumbnailOwnerService {
		return nil, fmt.Errorf("file-store's %s belongs to %s, not to photo-store", fileID, file.OwnerService)
	}
	content, err := thumbnails.files.OpenContent(file)
	if err != nil {
		return nil, fmt.Errorf("file-store, thumbnail %s: %w", fileID, err)
	}
	return content, nil
}

func (thumbnails photoThumbnailFiles) Destroy(_ context.Context, fileID string) error {
	if err := thumbnails.files.Purge(fileID); err != nil {
		return fmt.Errorf("file-store, destroying thumbnail %s: %w", fileID, err)
	}
	return nil
}

// photosAndLocationsOff answers a route of a store this server has not
// opened.
func photosAndLocationsOff(storeName, offCode string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSONError(w, http.StatusServiceUnavailable, offCode, storeName+" is not open on this server: EnablePhotosAndLocations was never called")
	})
}

func (s *Server) photoStoreRoute() http.HandlerFunc {
	return s.connectedRecordsRoute(photoStoreMountPrefix, "photo-store", "photo_store_off",
		func(*connectedRecordsStores) http.Handler {
			stores := s.photosAndLocations.Load()
			if stores == nil {
				return photosAndLocationsOff("photo-store", "photo_store_off")
			}
			return stores.photoHandler
		}, false)
}

func (s *Server) locationStoreRoute() http.HandlerFunc {
	return s.connectedRecordsRoute(locationStoreMountPrefix, "location-store", "location_store_off",
		func(*connectedRecordsStores) http.Handler {
			stores := s.photosAndLocations.Load()
			if stores == nil {
				return photosAndLocationsOff("location-store", "location_store_off")
			}
			return stores.locationHandler
		}, false)
}

// registerPhotosAndLocationsRoutes mounts the two stores.
func (s *Server) registerPhotosAndLocationsRoutes() {
	photos := s.photoStoreRoute()
	s.mux.HandleFunc("GET /photo-store/health", photos)
	s.mux.HandleFunc("GET /photo-store/date-sources", photos)
	s.mux.HandleFunc("GET /photo-store/tags", photos)
	s.mux.HandleFunc("GET /photo-store/photos", photos)
	s.mux.HandleFunc("GET /photo-store/sightings", photos)
	s.mux.HandleFunc("GET /photo-store/taken-activity", photos)
	s.mux.HandleFunc("POST /photo-store/imports", photos)
	s.mux.HandleFunc("GET /photo-store/photos/{id}", photos)
	s.mux.HandleFunc("PATCH /photo-store/photos/{id}", photos)
	s.mux.HandleFunc("DELETE /photo-store/photos/{id}", photos)
	s.mux.HandleFunc("POST /photo-store/photos/{id}/restore", photos)
	s.mux.HandleFunc("POST /photo-store/photos/{id}/purge", photos)
	s.mux.HandleFunc("GET /photo-store/photos/{id}/revisions", photos)
	s.mux.HandleFunc("GET /photo-store/photos/{id}/thumbnail", photos)
	s.mux.HandleFunc("GET /photo-store/face-group-statuses", photos)
	s.mux.HandleFunc("GET /photo-store/face-groups", photos)
	s.mux.HandleFunc("POST /photo-store/face-groups", photos)
	s.mux.HandleFunc("GET /photo-store/face-groups/{id}", photos)
	s.mux.HandleFunc("POST /photo-store/face-groups/{id}/members", photos)
	s.mux.HandleFunc("POST /photo-store/face-groups/{id}/guesses", photos)
	s.mux.HandleFunc("POST /photo-store/face-groups/{id}/decision", photos)
	s.mux.HandleFunc("POST /photo-store/face-scans", photos)
	s.mux.HandleFunc("GET /photo-store/face-scans/pending", photos)
	s.mux.HandleFunc("GET /photo-store/faces", photos)
	s.mux.HandleFunc("GET /photo-store/photos/{id}/faces", photos)
	s.mux.HandleFunc("GET /photo-store/faces/{id}/image", photos)
	s.mux.HandleFunc("GET /photo-store/photo-person-statuses", photos)
	s.mux.HandleFunc("GET /photo-store/photo-people", photos)
	s.mux.HandleFunc("POST /photo-store/photo-people", photos)
	s.mux.HandleFunc("PATCH /photo-store/photo-people/{id}", photos)
	s.mux.HandleFunc("GET /photo-store/people-counts", photos)

	locations := s.locationStoreRoute()
	s.mux.HandleFunc("GET /location-store/health", locations)
	s.mux.HandleFunc("GET /location-store/vocabularies", locations)
	s.mux.HandleFunc("GET /location-store/places", locations)
	s.mux.HandleFunc("POST /location-store/places", locations)
	s.mux.HandleFunc("GET /location-store/places/{id}", locations)
	s.mux.HandleFunc("PATCH /location-store/places/{id}", locations)
	s.mux.HandleFunc("POST /location-store/places/{id}/merge", locations)
	s.mux.HandleFunc("DELETE /location-store/places/{id}", locations)
	s.mux.HandleFunc("POST /location-store/places/{id}/restore", locations)
	s.mux.HandleFunc("GET /location-store/places/{id}/revisions", locations)
	s.mux.HandleFunc("GET /location-store/visits", locations)
	s.mux.HandleFunc("POST /location-store/visits", locations)
	s.mux.HandleFunc("GET /location-store/visits/{id}", locations)
	s.mux.HandleFunc("PATCH /location-store/visits/{id}", locations)
	s.mux.HandleFunc("DELETE /location-store/visits/{id}", locations)
	s.mux.HandleFunc("POST /location-store/visits/{id}/restore", locations)
	s.mux.HandleFunc("GET /location-store/visits/{id}/revisions", locations)
	s.mux.HandleFunc("GET /location-store/trips", locations)
	s.mux.HandleFunc("POST /location-store/trips", locations)
	s.mux.HandleFunc("GET /location-store/trips/{id}", locations)
	s.mux.HandleFunc("PATCH /location-store/trips/{id}", locations)
	s.mux.HandleFunc("GET /location-store/trips/{id}/path", locations)
	s.mux.HandleFunc("POST /location-store/trips/{id}/children", locations)
	s.mux.HandleFunc("DELETE /location-store/trips/{id}", locations)
	s.mux.HandleFunc("POST /location-store/trips/{id}/restore", locations)
	s.mux.HandleFunc("GET /location-store/trips/{id}/revisions", locations)
	s.mux.HandleFunc("POST /location-store/localities/fill", locations)
	s.mux.HandleFunc("GET /location-store/home-periods", locations)
	s.mux.HandleFunc("GET /location-store/home-periods/{id}", locations)
	s.mux.HandleFunc("POST /location-store/journeys/group", locations)
	s.mux.HandleFunc("GET /location-store/journeys", locations)
	s.mux.HandleFunc("GET /location-store/journeys/{id}", locations)
	s.mux.HandleFunc("POST /location-store/calendar/match", locations)
	s.mux.HandleFunc("GET /location-store/calendar-event-links", locations)
	s.mux.HandleFunc("POST /location-store/calendar-event-links", locations)
	s.mux.HandleFunc("GET /location-store/calendar-event-links/{id}", locations)
	s.mux.HandleFunc("DELETE /location-store/calendar-event-links/{id}", locations)
	s.mux.HandleFunc("GET /location-store/calendar-series-places", locations)
	s.mux.HandleFunc("GET /location-store/calendar-series-places/{id}", locations)
	s.mux.HandleFunc("GET /location-store/unnamed-places", locations)
	s.mux.HandleFunc("POST /location-store/place-suggestions/proposals", locations)
	s.mux.HandleFunc("GET /location-store/place-suggestions", locations)
	s.mux.HandleFunc("GET /location-store/place-suggestions/{id}", locations)
	s.mux.HandleFunc("POST /location-store/place-suggestions/{id}/accept", locations)
	s.mux.HandleFunc("POST /location-store/place-suggestions/{id}/reject", locations)
	s.mux.HandleFunc("GET /location-store/timeline", locations)
	s.mux.HandleFunc("GET /location-store/points", locations)
	s.mux.HandleFunc("POST /location-store/points/live", locations)
	s.mux.HandleFunc("POST /location-store/phone-points/segment", locations)
	s.mux.HandleFunc("GET /location-store/imports", locations)
	s.mux.HandleFunc("POST /location-store/imports", locations)
	s.mux.HandleFunc("GET /location-store/imports/{id}", locations)
	s.mux.HandleFunc("POST /location-store/imports/{id}/batches", locations)
	s.mux.HandleFunc("POST /location-store/imports/{id}/finish", locations)
}
