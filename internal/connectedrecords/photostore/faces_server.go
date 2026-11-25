package photostore

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	jpegcodec "image/jpeg"
	"net/http"
)

func registerFaceHandlers(mux *http.ServeMux, h *handler) {
	mux.HandleFunc("POST /face-scans", h.recordFaceScan)
	mux.HandleFunc("GET /face-scans/pending", h.photosNotScanned)
	mux.HandleFunc("GET /faces", h.faces)
	mux.HandleFunc("GET /photos/{id}/faces", h.facesOfPhoto)
	mux.HandleFunc("GET /faces/{id}/image", h.faceImage)
	mux.HandleFunc("GET /photo-person-statuses", vocabulary(PhotoPersonStatuses))
	mux.HandleFunc("GET /photo-people", h.photoPeople)
	mux.HandleFunc("POST /photo-people", h.proposePhotoPerson)
	mux.HandleFunc("PATCH /photo-people/{id}", h.decidePhotoPerson)
	mux.HandleFunc("GET /people-counts", h.peopleCounts)
}

// decodeStrict reads a JSON body into out, refusing unknown keys.
func decodeStrict(w http.ResponseWriter, r *http.Request, limit int64, out any) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json: "+err.Error())
		return false
	}
	return true
}

func (h *handler) recordFaceScan(w http.ResponseWriter, r *http.Request) {
	scope, ok := readScope(w, r)
	if !ok {
		return
	}
	var scan FaceScan
	if !decodeStrict(w, r, 8<<20, &scan) {
		return
	}
	faces, err := h.s.RecordFaceScan(r.Context(), scope, scan)
	if respondError(w, err) {
		return
	}
	writeJSON(w, http.StatusCreated, faces)
}

func (h *handler) photosNotScanned(w http.ResponseWriter, r *http.Request) {
	scope, ok := readScope(w, r)
	if !ok {
		return
	}
	limit, ok := integerParameter(w, r, "limit")
	if !ok {
		return
	}
	ids, err := h.s.PhotosNotScanned(r.Context(), scope, r.URL.Query().Get("model"), int(limit))
	if respondError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, ids)
}

func (h *handler) faces(w http.ResponseWriter, r *http.Request) {
	scope, ok := readScope(w, r)
	if !ok {
		return
	}
	query := r.URL.Query()
	limit, ok := integerParameter(w, r, "limit")
	if !ok {
		return
	}
	faces, err := h.s.Faces(r.Context(), scope, FaceFilter{PhotoID: query.Get("photo_id"), Model: query.Get("model"), AfterID: query.Get("after"),
		IncludeEmbeddings: query.Get("include_embeddings") == "true", Limit: int(limit)})
	if respondError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, faces)
}

func (h *handler) facesOfPhoto(w http.ResponseWriter, r *http.Request) {
	scope, ok := readScope(w, r)
	if !ok {
		return
	}
	faces, err := h.s.Faces(r.Context(), scope, FaceFilter{PhotoID: r.PathValue("id"), Model: r.URL.Query().Get("model")})
	if respondError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, faces)
}

func (h *handler) photoPeople(w http.ResponseWriter, r *http.Request) {
	scope, ok := readScope(w, r)
	if !ok {
		return
	}
	query := r.URL.Query()
	limit, ok := integerParameter(w, r, "limit")
	if !ok {
		return
	}
	offset, ok := integerParameter(w, r, "offset")
	if !ok {
		return
	}
	links, err := h.s.PhotoPeople(r.Context(), scope, PhotoPersonFilter{PhotoID: query.Get("photo_id"), PersonID: query.Get("person_id"),
		Status: query.Get("status"), Limit: int(limit), Offset: int(offset)})
	if respondError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, links)
}

func (h *handler) proposePhotoPerson(w http.ResponseWriter, r *http.Request) {
	scope, ok := readScope(w, r)
	if !ok {
		return
	}
	var proposal PhotoPersonProposal
	if !decodeStrict(w, r, 1<<20, &proposal) {
		return
	}
	link, created, err := h.s.ProposePhotoPerson(r.Context(), scope, proposal)
	if respondError(w, err) {
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, link)
}

func (h *handler) decidePhotoPerson(w http.ResponseWriter, r *http.Request) {
	scope, ok := readScope(w, r)
	if !ok {
		return
	}
	var decision PhotoPersonDecision
	if !decodeStrict(w, r, 1<<20, &decision) {
		return
	}
	link, err := h.s.DecidePhotoPerson(r.Context(), scope, r.PathValue("id"), decision)
	if respondError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, link)
}

func (h *handler) peopleCounts(w http.ResponseWriter, r *http.Request) {
	scope, ok := readScope(w, r)
	if !ok {
		return
	}
	counts, err := h.s.PeopleCounts(r.Context(), scope)
	if respondError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, counts)
}

// faceImage answers GET /faces/{id}/image: the face cut from its photo's
// thumbnail with a third of its size around it, as a JPEG, so a page or a
// model sees the face and a little of the head.
func (h *handler) faceImage(w http.ResponseWriter, r *http.Request) {
	scope, ok := readScope(w, r)
	if !ok {
		return
	}
	face, err := h.s.FaceByID(r.Context(), scope, r.PathValue("id"))
	if respondError(w, err) {
		return
	}
	file, err := h.s.OpenThumbnail(r.Context(), scope, face.PhotoID)
	if errors.Is(err, ErrGone) {
		writeErr(w, http.StatusGone, fmt.Sprintf("photo %s was purged: its faces are gone for good", face.PhotoID))
		return
	}
	if respondError(w, err) {
		return
	}
	defer file.Close()
	picture, err := jpegcodec.Decode(file)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, fmt.Sprintf("thumbnail of %s: %v", face.PhotoID, err))
		return
	}
	crop := FaceCrop(picture.Bounds(), face.Box)
	cropper, ok := picture.(interface {
		SubImage(image.Rectangle) image.Image
	})
	if crop.Empty() || !ok {
		writeErr(w, http.StatusInternalServerError, fmt.Sprintf("%s cannot be cut from its thumbnail", face.ID))
		return
	}
	var encoded bytes.Buffer
	if err := jpegcodec.Encode(&encoded, cropper.SubImage(crop), &jpegcodec.Options{Quality: 88}); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "private, max-age=86400")
	w.Write(encoded.Bytes())
}

// FaceCrop is the rectangle of a thumbnail a face is cut from: its box with
// a third of its width and height around it, kept inside the thumbnail.
func FaceCrop(bounds image.Rectangle, box FaceBox) image.Rectangle {
	width, height := float64(bounds.Dx()), float64(bounds.Dy())
	marginX, marginY := (box.Right-box.Left)*0.3, (box.Bottom-box.Top)*0.3
	return image.Rect(bounds.Min.X+int((box.Left-marginX)*width), bounds.Min.Y+int((box.Top-marginY)*height),
		bounds.Min.X+int((box.Right+marginX)*width), bounds.Min.Y+int((box.Bottom+marginY)*height)).Intersect(bounds)
}
