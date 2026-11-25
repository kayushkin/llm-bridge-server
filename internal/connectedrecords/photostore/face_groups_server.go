package photostore

import "net/http"

func registerFaceGroupHandlers(mux *http.ServeMux, h *handler) {
	mux.HandleFunc("GET /face-group-statuses", vocabulary(FaceGroupStatuses))
	mux.HandleFunc("GET /face-groups", h.faceGroups)
	mux.HandleFunc("POST /face-groups", h.createFaceGroup)
	mux.HandleFunc("GET /face-groups/{id}", h.faceGroup)
	mux.HandleFunc("POST /face-groups/{id}/members", h.addFaceGroupMembers)
	mux.HandleFunc("POST /face-groups/{id}/guesses", h.addFaceGroupGuess)
	mux.HandleFunc("POST /face-groups/{id}/decision", h.decideFaceGroup)
}

func (h *handler) faceGroups(w http.ResponseWriter, r *http.Request) {
	scope, ok := readScope(w, r)
	if !ok {
		return
	}
	limit, ok := integerParameter(w, r, "limit")
	if !ok {
		return
	}
	offset, ok := integerParameter(w, r, "offset")
	if !ok {
		return
	}
	groups, err := h.s.FaceGroups(r.Context(), scope, FaceGroupFilter{Status: r.URL.Query().Get("status"), PersonID: r.URL.Query().Get("person_id"),
		Limit: int(limit), Offset: int(offset)})
	if respondError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, groups)
}

func (h *handler) createFaceGroup(w http.ResponseWriter, r *http.Request) {
	scope, ok := readScope(w, r)
	if !ok {
		return
	}
	var group NewFaceGroup
	if !decodeStrict(w, r, 4<<20, &group) {
		return
	}
	created, err := h.s.CreateFaceGroup(r.Context(), scope, group)
	if respondError(w, err) {
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

func (h *handler) faceGroup(w http.ResponseWriter, r *http.Request) {
	scope, ok := readScope(w, r)
	if !ok {
		return
	}
	group, err := h.s.FaceGroupByID(r.Context(), scope, r.PathValue("id"), true)
	if respondError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, group)
}

func (h *handler) addFaceGroupMembers(w http.ResponseWriter, r *http.Request) {
	scope, ok := readScope(w, r)
	if !ok {
		return
	}
	var addition FaceGroupMembersAddition
	if !decodeStrict(w, r, 4<<20, &addition) {
		return
	}
	group, err := h.s.AddFaceGroupMembers(r.Context(), scope, r.PathValue("id"), addition)
	if respondError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, group)
}

func (h *handler) addFaceGroupGuess(w http.ResponseWriter, r *http.Request) {
	scope, ok := readScope(w, r)
	if !ok {
		return
	}
	var proposal FaceGroupGuessProposal
	if !decodeStrict(w, r, 1<<20, &proposal) {
		return
	}
	guess, err := h.s.AddFaceGroupGuess(r.Context(), scope, r.PathValue("id"), proposal)
	if respondError(w, err) {
		return
	}
	writeJSON(w, http.StatusCreated, guess)
}

func (h *handler) decideFaceGroup(w http.ResponseWriter, r *http.Request) {
	scope, ok := readScope(w, r)
	if !ok {
		return
	}
	var decision FaceGroupDecision
	if !decodeStrict(w, r, 1<<20, &decision) {
		return
	}
	result, err := h.s.DecideFaceGroup(r.Context(), scope, r.PathValue("id"), decision)
	if respondError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, result)
}
