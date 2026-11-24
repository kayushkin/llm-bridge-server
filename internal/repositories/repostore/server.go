package repostore

import (
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// RegisterHandlers wires the HTTP API onto mux.
func RegisterHandlers(mux *http.ServeMux, s *Store) {
	h := &handler{s: s}

	mux.HandleFunc("GET /health", h.health)

	mux.HandleFunc("GET /repos", h.listRepos)
	mux.HandleFunc("POST /repos", h.upsertRepo)
	mux.HandleFunc("GET /repos/{id}", h.getRepo)
	mux.HandleFunc("DELETE /repos/{id}", h.deleteRepo)
	mux.HandleFunc("POST /repos/{id}/detect", h.redetectRepo)
	mux.HandleFunc("PUT /repos/{id}/provenance", h.setProvenance)
	mux.HandleFunc("PUT /repos/{id}/identity", h.setIdentity) // rename, move path and remote; keeps the id

	mux.HandleFunc("GET /repos/by-name/{name}", h.getRepoByName)
	mux.HandleFunc("GET /repos/by-mention/{mention}", h.getRepoByMention) // repo:<name>, as a chat message writes it
	mux.HandleFunc("GET /by-path", h.getRepoByPath)                       // ?path= (paths contain slashes)

	mux.HandleFunc("POST /detect", h.detect) // stateless: {path} -> signature

	// The deploy ledger. Append-only: there is no PUT and no DELETE.
	mux.HandleFunc("POST /repos/{id}/deployments", h.recordDeployment)
	mux.HandleFunc("GET /deployments", h.listDeployments) // ?repo_id= (required); not under /repos/{id}/, which would collide with /repos/by-name/{name}
	mux.HandleFunc("GET /deployments/latest", h.latestDeployments)
}

type handler struct{ s *Store }

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func (h *handler) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, 200, map[string]string{"status": "ok"})
}

func (h *handler) listRepos(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := ListFilter{Tag: q.Get("tag"), Query: q.Get("q")}
	if q.Has("provenance") {
		p, err := parseProvenanceQuery(q.Get("provenance"))
		if err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		f.Provenance = &p
	}
	if lim := q.Get("limit"); lim != "" {
		n, err := strconv.Atoi(lim)
		if err != nil {
			writeErr(w, 400, "invalid limit")
			return
		}
		f.Limit = n
	}
	repos, err := h.s.ListRepos(f)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if repos == nil {
		repos = []Repo{}
	}
	writeJSON(w, 200, repos)
}

// upsertRepo creates or updates a repo. If a path is supplied and exists on
// disk, the signature is (re)detected and stored — the body's tags are ignored
// in favor of the canonical detection. tag_overrides from the body are kept.
func (h *handler) upsertRepo(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeErr(w, 400, "read body: "+err.Error())
		return
	}
	// A provenance here would be silently dropped — this path does not write
	// the column, so that a routine tag refresh cannot blank it. Refusing says
	// so; accepting and ignoring would hand back a repo whose provenance is
	// whatever was already stored, reading as though the request had applied.
	var probe struct {
		Provenance *string `json:"provenance"`
	}
	if err := json.Unmarshal(body, &probe); err == nil && probe.Provenance != nil {
		writeErr(w, 400, "provenance is not settable here; use PUT /repos/{id}/provenance")
		return
	}
	var repo Repo
	if err := json.Unmarshal(body, &repo); err != nil {
		writeErr(w, 400, "invalid json: "+err.Error())
		return
	}
	if repo.Path != "" {
		sig, err := Detect(repo.Path)
		if err != nil {
			writeErr(w, 400, "detect "+repo.Path+": "+err.Error())
			return
		}
		repo.Tags = sig.Tags()
	}
	inserted, err := h.s.UpsertRepo(&repo)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	status := 200
	if inserted {
		status = 201
	}
	writeJSON(w, status, repo)
}

func (h *handler) getRepo(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r.PathValue("id"))
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	repo, err := h.s.GetRepo(id)
	if err != nil {
		respondLookup(w, err)
		return
	}
	writeJSON(w, 200, repo)
}

func (h *handler) getRepoByName(w http.ResponseWriter, r *http.Request) {
	repo, err := h.s.GetRepoByName(r.PathValue("name"))
	if err != nil {
		respondLookup(w, err)
		return
	}
	writeJSON(w, 200, repo)
}

// getRepoByMention answers the chat's reference resolver for a `repo:<name>`
// mention: the same repo GET /repos/by-name/{name} returns, or 404 for a
// mention of a repo repo-store does not have. Anything that is not a mention
// is a 400, so a resolver misconfigured to send other ids here is loud.
func (h *handler) getRepoByMention(w http.ResponseWriter, r *http.Request) {
	name, ok := RepoNameOfMention(r.PathValue("mention"))
	if !ok {
		writeErr(w, 400, "a repo mention is "+RepoMentionPrefix+"<name>")
		return
	}
	repo, err := h.s.GetRepoByName(name)
	if err != nil {
		respondLookup(w, err)
		return
	}
	writeJSON(w, 200, repo)
}

func (h *handler) getRepoByPath(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if path == "" {
		writeErr(w, 400, "path query param required")
		return
	}
	repo, err := h.s.GetRepoByPath(path)
	if err != nil {
		respondLookup(w, err)
		return
	}
	writeJSON(w, 200, repo)
}

func (h *handler) deleteRepo(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r.PathValue("id"))
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if err := h.s.DeleteRepo(id); err != nil {
		respondLookup(w, err)
		return
	}
	writeJSON(w, 200, map[string]string{"status": "deleted"})
}

// redetectRepo re-runs detection on a stored repo's path and refreshes its tags.
func (h *handler) redetectRepo(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r.PathValue("id"))
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	repo, err := h.s.GetRepo(id)
	if err != nil {
		respondLookup(w, err)
		return
	}
	if repo.Path == "" {
		writeErr(w, 400, "repo has no path to detect from")
		return
	}
	sig, err := Detect(repo.Path)
	if err != nil {
		writeErr(w, 400, "detect "+repo.Path+": "+err.Error())
		return
	}
	repo.Tags = sig.Tags()
	if _, err := h.s.UpsertRepo(repo); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, repo)
}

// setProvenance records who authors a stored repository. Separate from the
// upsert path on purpose: see Store.SetProvenance.
func (h *handler) setProvenance(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r.PathValue("id"))
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	var body struct {
		Provenance *string `json:"provenance"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, "invalid json: "+err.Error())
		return
	}
	if body.Provenance == nil {
		writeErr(w, 400, "provenance required; "+acceptedProvenances())
		return
	}
	p := Provenance(*body.Provenance)
	if !p.IsKnown() {
		writeErr(w, 400, "unknown provenance "+strconv.Quote(*body.Provenance)+"; "+acceptedProvenances())
		return
	}
	repo, err := h.s.SetProvenance(id, p)
	if err != nil {
		respondLookup(w, err)
		return
	}
	writeJSON(w, 200, repo)
}

// setIdentity renames a repo and changes its path and git remote, keeping its
// id. Every field is required, so a caller states the whole identity and an
// omitted field cannot blank one by accident; send the current value to keep
// it. When the new path exists on disk the tags are re-detected from it, as
// POST /repos does; when it does not (the move has not happened yet), the
// stored tags stay. Nothing on disk is touched.
func (h *handler) setIdentity(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r.PathValue("id"))
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	var body struct {
		Name      *string `json:"name"`
		Path      *string `json:"path"`
		GitRemote *string `json:"git_remote"`
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		writeErr(w, 400, "invalid json: "+err.Error())
		return
	}
	var missing []string
	if body.Name == nil {
		missing = append(missing, "name")
	}
	if body.Path == nil {
		missing = append(missing, "path")
	}
	if body.GitRemote == nil {
		missing = append(missing, "git_remote")
	}
	if len(missing) > 0 {
		writeErr(w, 400, "required: "+strings.Join(missing, ", ")+" (send the current value to keep one)")
		return
	}
	ident := RepoIdentity{Name: *body.Name, Path: *body.Path, GitRemote: *body.GitRemote}
	if ident.Name == "" {
		writeErr(w, 400, "name must not be empty")
		return
	}
	if !filepath.IsAbs(ident.Path) {
		writeErr(w, 400, "path must be absolute: "+strconv.Quote(ident.Path))
		return
	}
	if _, err := os.Stat(ident.Path); err == nil {
		sig, err := Detect(ident.Path)
		if err != nil {
			writeErr(w, 400, "detect "+ident.Path+": "+err.Error())
			return
		}
		ident.Tags = sig.Tags()
		if ident.Tags == nil {
			ident.Tags = []string{}
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		writeErr(w, 400, "stat "+ident.Path+": "+err.Error())
		return
	}
	repo, err := h.s.SetIdentity(id, ident)
	switch {
	case errors.Is(err, ErrNameTaken), errors.Is(err, ErrPathTaken):
		writeErr(w, 409, err.Error())
	case err != nil:
		respondLookup(w, err)
	default:
		writeJSON(w, 200, repo)
	}
}

// parseProvenanceQuery reads the ?provenance= filter. The unstated class needs
// a word of its own because an empty parameter cannot be told from an absent
// one, and absent means the filter is off.
func parseProvenanceQuery(v string) (Provenance, error) {
	if v == ProvenanceUnstatedQueryValue {
		return ProvenanceUnstated, nil
	}
	p := Provenance(v)
	if p == ProvenanceUnstated || !p.IsKnown() {
		return "", errors.New("unknown provenance " + strconv.Quote(v) + "; " + acceptedProvenancesForQuery())
	}
	return p, nil
}

func acceptedProvenances() string {
	names := make([]string, 0, len(KnownProvenances()))
	for _, p := range KnownProvenances() {
		if p == ProvenanceUnstated {
			names = append(names, `"" (unstated)`)
			continue
		}
		names = append(names, strconv.Quote(string(p)))
	}
	return "accepted: " + strings.Join(names, ", ")
}

func acceptedProvenancesForQuery() string {
	names := make([]string, 0, len(KnownProvenances()))
	for _, p := range KnownProvenances() {
		if p == ProvenanceUnstated {
			names = append(names, strconv.Quote(ProvenanceUnstatedQueryValue))
			continue
		}
		names = append(names, strconv.Quote(string(p)))
	}
	return "accepted: " + strings.Join(names, ", ")
}

// detect is the stateless detection endpoint: given {path}, return the
// signature and flattened tags without persisting anything. Used by the
// resolver for repos that aren't (yet) registered.
func (h *handler) detect(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, "invalid json: "+err.Error())
		return
	}
	if body.Path == "" {
		writeErr(w, 400, "path required")
		return
	}
	sig, err := Detect(body.Path)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"signature": sig, "tags": sig.Tags()})
}

func respondLookup(w http.ResponseWriter, err error) {
	if errors.Is(err, ErrNotFound) {
		writeErr(w, 404, "repo not found")
		return
	}
	writeErr(w, 500, err.Error())
}

func parseID(s string) (int64, error) {
	id, err := strconv.ParseInt(s, 10, 64)
	if err != nil || id <= 0 {
		return 0, errors.New("invalid id")
	}
	return id, nil
}

func (h *handler) recordDeployment(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r.PathValue("id"))
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	var d Deployment
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&d); err != nil {
		writeErr(w, 400, "bad json: "+err.Error())
		return
	}
	if err := h.s.RecordDeployment(id, &d); err != nil {
		if errors.Is(err, ErrNotFound) {
			writeErr(w, 404, "repo not found")
			return
		}
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 201, d)
}

func (h *handler) listDeployments(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r.URL.Query().Get("repo_id"))
	if err != nil {
		writeErr(w, 400, "repo_id is required: the id repo-store gave the repo (GET /by-path?path=… finds it); GET /deployments/latest lists every repo")
		return
	}
	limit := 0
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if limit, err = strconv.Atoi(raw); err != nil || limit <= 0 {
			writeErr(w, 400, "limit must be a positive integer")
			return
		}
	}
	deployments, err := h.s.ListDeployments(id, limit)
	if err != nil {
		respondLookup(w, err)
		return
	}
	writeJSON(w, 200, deployments)
}

func (h *handler) latestDeployments(w http.ResponseWriter, _ *http.Request) {
	deployments, err := h.s.LatestDeployments()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, deployments)
}
