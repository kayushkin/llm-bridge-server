package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

// SeedReconciler keeps a single seed source (agent-store or skill-store) in
// sync from the bridge to this runner's $HOME. Three triggers fire a
// reconcile pass:
//
//  1. A SeedSnapshot WS message arrives (server pushed after a save/scan).
//  2. The periodic tick elapses (backstop in case a snapshot was missed).
//  3. The Client opens a fresh WS connection (handled by Client.runOnce).
//
// Reconcile is non-destructive: any locally-modified file whose hash diverges
// from what the bridge most recently pushed is uploaded to the source's
// /seed/drift endpoint and folded into the file's history *before* the new
// content overwrites it. Files we never wrote are left untouched — we only
// manage paths that appear in the bridge's manifest plus paths recorded in
// our local sidecar manifest.
type SeedReconciler struct {
	source     msg.SeedSource
	strategy   seedStrategy
	cfg        *Config
	machineID  func() string

	mu          sync.Mutex
	sidecarPath string
	sidecar     *SidecarManifest

	trigger chan struct{}
}

// seedStrategy hides the per-source HTTP shape behind a uniform interface
// so the reconciler loop can be source-agnostic. Each strategy knows its
// API prefix on the bridge and how to build the drift/observe payloads
// that source expects.
type seedStrategy interface {
	source() msg.SeedSource
	fetchManifest(machineID string) ([]seedEntry, error)
	fetchContent(e seedEntry) ([]byte, error)
	postDrift(machineID string, e seedEntry, content []byte, note string) error
	postObserve(machineID string, e seedEntry, sha string) error
}

// seedEntry is the generalized manifest entry used inside the reconciler.
// Strategies translate their source-specific manifest payload into this
// uniform shape.
type seedEntry struct {
	// Stable identity: install_path is the runner-side write target,
	// $HOME-relative.
	InstallPath string
	SHA256      string
	Size        int64
	VersionID   int64

	// SourceKey holds the fields the source requires for drift/observe
	// addressing (e.g. {tracked_file_id} for agent-store, {skill_id, relpath}
	// for skill-store). Treated as opaque by the reconciler — strategies
	// merge it into request bodies.
	SourceKey map[string]any
}

// SidecarManifest is what we persist on the runner's disk so a future
// reconcile knows which files we wrote (and thus may delete on removal) and
// what hash we last pushed (so we can detect drift on the next tick without
// re-fetching the manifest first). Stored at
// ~/.config/llm-bridge-runner/seed-<source>.json.
type SidecarManifest struct {
	Source    msg.SeedSource          `json:"source"`
	UpdatedAt int64                   `json:"updated_at"`
	Entries   map[string]SidecarEntry `json:"entries"` // keyed by install_path
}

// SidecarEntry records one file we manage on disk.
type SidecarEntry struct {
	InstallPath  string         `json:"install_path"`
	LastPushSHA  string         `json:"last_push_sha"`
	LastPushTime int64          `json:"last_push_time"`
	VersionID    int64          `json:"version_id,omitempty"`
	SourceKey    map[string]any `json:"source_key"`
}

// NewSeedReconciler builds a reconciler bound to one source.
func NewSeedReconciler(source msg.SeedSource, cfg *Config, machineIDFn func() string) (*SeedReconciler, error) {
	strat, err := newStrategy(source, cfg)
	if err != nil {
		return nil, err
	}
	sidecarDir, err := sidecarDir()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(sidecarDir, 0755); err != nil {
		return nil, fmt.Errorf("create sidecar dir: %w", err)
	}
	sidecarPath := filepath.Join(sidecarDir, fmt.Sprintf("seed-%s.json", source))
	r := &SeedReconciler{
		source:      source,
		strategy:    strat,
		cfg:         cfg,
		machineID:   machineIDFn,
		sidecarPath: sidecarPath,
		trigger:     make(chan struct{}, 1),
	}
	r.sidecar = r.loadSidecar()
	return r, nil
}

func newStrategy(source msg.SeedSource, cfg *Config) (seedStrategy, error) {
	httpClient := &http.Client{Timeout: 60 * time.Second}
	switch source {
	case msg.SeedSourceAgentStore:
		return &agentSeedStrategy{cfg: cfg, http: httpClient, prefix: "/api/agent-store"}, nil
	case msg.SeedSourceSkillStore:
		return &skillSeedStrategy{cfg: cfg, http: httpClient, prefix: "/api/skill-store"}, nil
	}
	return nil, fmt.Errorf("unknown seed source: %s", source)
}

// sidecarDir returns the directory the seed sidecar manifests live in. It is
// DERIVED from configPath() rather than re-resolved from the user's home,
// because it is the very same directory — and two independent resolutions of one
// path is how a sandboxed run leaks into the live tree.
//
// They did not agree. configPath() honors $LLMBRIDGE_RUNNER_CONFIG and
// $XDG_CONFIG_HOME; this function used user.Current().HomeDir, which reads
// /etc/passwd and therefore ignores $HOME entirely. So a test or a smoke could
// redirect the config file into a sandbox, watch the runner honor it, and still
// have the seed reconciler create and write ~/.config/llm-bridge-runner in the
// real home — silently, on every NewClient().
func sidecarDir() (string, error) {
	return filepath.Dir(configPath()), nil
}

// Trigger asks the reconciler to run a pass soon. Coalesces multiple
// triggers into one — if a pass is already pending, additional Trigger
// calls are no-ops.
func (r *SeedReconciler) Trigger() {
	select {
	case r.trigger <- struct{}{}:
	default:
	}
}

// Run blocks until stop fires, reconciling on every trigger and on every
// tick of the backstop interval.
func (r *SeedReconciler) Run(stop <-chan struct{}, tick time.Duration) {
	timer := time.NewTimer(tick)
	defer timer.Stop()
	for {
		select {
		case <-stop:
			return
		case <-r.trigger:
			r.reconcile()
			resetTimer(timer, tick)
		case <-timer.C:
			r.reconcile()
			resetTimer(timer, tick)
		}
	}
}

func resetTimer(t *time.Timer, d time.Duration) {
	if !t.Stop() {
		select {
		case <-t.C:
		default:
		}
	}
	t.Reset(d)
}

// reconcile pulls the current manifest, diffs against the local disk, and
// applies upserts/deletes. Errors are logged; the pass returns early without
// touching anything else when the manifest fetch fails (no manifest, no
// reliable diff).
func (r *SeedReconciler) reconcile() {
	machine := r.machineID()
	if machine == "" {
		// Pre-Welcome — no machine_id yet. Next trigger will retry.
		return
	}
	entries, err := r.strategy.fetchManifest(machine)
	if err != nil {
		log.Printf("[seed:%s] fetch manifest: %v", r.source, err)
		return
	}

	home, err := os.UserHomeDir()
	if err != nil {
		log.Printf("[seed:%s] resolve home: %v", r.source, err)
		return
	}

	want := make(map[string]struct{}, len(entries))
	for _, e := range entries {
		want[e.InstallPath] = struct{}{}
		if err := r.applyUpsert(home, machine, e); err != nil {
			log.Printf("[seed:%s] upsert %s: %v", r.source, e.InstallPath, err)
		}
	}

	// Files we previously wrote that the bridge no longer wants → remove,
	// after a final drift check. Files we never wrote stay untouched.
	r.mu.Lock()
	stale := make([]SidecarEntry, 0)
	for path, ent := range r.sidecar.Entries {
		if _, keep := want[path]; !keep {
			stale = append(stale, ent)
		}
	}
	r.mu.Unlock()
	for _, ent := range stale {
		if err := r.applyDelete(home, machine, ent); err != nil {
			log.Printf("[seed:%s] delete %s: %v", r.source, ent.InstallPath, err)
		}
	}

	r.persistSidecar()
}

// applyUpsert ensures the local file has the SHA the bridge wants. Drift is
// uploaded before overwrite. Already-matching files are skipped.
func (r *SeedReconciler) applyUpsert(home, machine string, e seedEntry) error {
	abs := filepath.Join(home, e.InstallPath)

	localSHA, localBytes, exists := r.readLocal(abs)
	if exists && localSHA == e.SHA256 {
		_ = r.strategy.postObserve(machine, e, localSHA)
		r.recordSidecar(e.InstallPath, SidecarEntry{
			InstallPath:  e.InstallPath,
			LastPushSHA:  e.SHA256,
			LastPushTime: time.Now().Unix(),
			VersionID:    e.VersionID,
			SourceKey:    e.SourceKey,
		})
		return nil
	}

	// Drift capture: if the local content isn't what the bridge thinks it
	// last pushed, send it over before overwriting. Non-destructive even
	// when our sidecar is wrong (note in the version row makes this
	// distinguishable from intentional saves).
	if exists {
		expected := r.expectedPriorSHA(e.InstallPath)
		if localSHA != expected {
			if err := r.strategy.postDrift(machine, e, localBytes,
				fmt.Sprintf("captured by runner %s before overwrite", r.cfg.MachineName)); err != nil {
				return fmt.Errorf("upload drift: %w", err)
			}
		}
	}

	content, err := r.strategy.fetchContent(e)
	if err != nil {
		return fmt.Errorf("fetch content: %w", err)
	}
	if err := atomicWrite(abs, content); err != nil {
		return fmt.Errorf("write %s: %w", abs, err)
	}
	_ = r.strategy.postObserve(machine, e, e.SHA256)
	r.recordSidecar(e.InstallPath, SidecarEntry{
		InstallPath:  e.InstallPath,
		LastPushSHA:  e.SHA256,
		LastPushTime: time.Now().Unix(),
		VersionID:    e.VersionID,
		SourceKey:    e.SourceKey,
	})
	return nil
}

// applyDelete removes a file the bridge no longer wants. If the local
// content has drifted from the last known push, it is uploaded first so
// the deletion is also non-destructive.
func (r *SeedReconciler) applyDelete(home, machine string, ent SidecarEntry) error {
	abs := filepath.Join(home, ent.InstallPath)
	localSHA, localBytes, exists := r.readLocal(abs)
	if exists && localSHA != ent.LastPushSHA {
		// Reconstruct an entry just enough for the strategy to address the drift POST.
		stub := seedEntry{InstallPath: ent.InstallPath, SourceKey: ent.SourceKey}
		if err := r.strategy.postDrift(machine, stub, localBytes,
			fmt.Sprintf("captured by runner %s before delete", r.cfg.MachineName)); err != nil {
			return fmt.Errorf("upload drift before delete: %w", err)
		}
	}
	if exists {
		if err := os.Remove(abs); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	r.forgetSidecar(ent.InstallPath)
	return nil
}

// readLocal reads abs and returns its sha + bytes. Returns ("", nil, false)
// if the file does not exist.
func (r *SeedReconciler) readLocal(abs string) (string, []byte, bool) {
	data, err := os.ReadFile(abs)
	if err != nil {
		return "", nil, false
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), data, true
}

// expectedPriorSHA returns the sha the bridge most recently pushed for this
// install path on this runner, per our sidecar.
func (r *SeedReconciler) expectedPriorSHA(installPath string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.sidecar == nil {
		return ""
	}
	if e, ok := r.sidecar.Entries[installPath]; ok {
		return e.LastPushSHA
	}
	return ""
}

// atomicWrite writes content to abs via tmp+rename. Creates parent dirs.
func atomicWrite(abs string, content []byte) error {
	if err := os.MkdirAll(filepath.Dir(abs), 0755); err != nil {
		return err
	}
	tmp := abs + ".seed.tmp"
	if err := os.WriteFile(tmp, content, 0644); err != nil {
		return err
	}
	if err := os.Rename(tmp, abs); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// ============================================
// Sidecar manifest persistence
// ============================================

func (r *SeedReconciler) loadSidecar() *SidecarManifest {
	data, err := os.ReadFile(r.sidecarPath)
	if err != nil {
		return &SidecarManifest{Source: r.source, Entries: map[string]SidecarEntry{}}
	}
	var m SidecarManifest
	if err := json.Unmarshal(data, &m); err != nil {
		log.Printf("[seed:%s] sidecar parse error: %v (starting fresh)", r.source, err)
		return &SidecarManifest{Source: r.source, Entries: map[string]SidecarEntry{}}
	}
	if m.Entries == nil {
		m.Entries = map[string]SidecarEntry{}
	}
	return &m
}

func (r *SeedReconciler) recordSidecar(path string, entry SidecarEntry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sidecar.Entries[path] = entry
}

func (r *SeedReconciler) forgetSidecar(path string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.sidecar.Entries, path)
}

func (r *SeedReconciler) persistSidecar() {
	r.mu.Lock()
	r.sidecar.UpdatedAt = time.Now().Unix()
	data, err := json.MarshalIndent(r.sidecar, "", "  ")
	r.mu.Unlock()
	if err != nil {
		log.Printf("[seed:%s] sidecar marshal: %v", r.source, err)
		return
	}
	tmp := r.sidecarPath + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		log.Printf("[seed:%s] sidecar write: %v", r.source, err)
		return
	}
	if err := os.Rename(tmp, r.sidecarPath); err != nil {
		log.Printf("[seed:%s] sidecar rename: %v", r.source, err)
	}
}

// ============================================
// agent-store strategy
// ============================================

type agentSeedStrategy struct {
	cfg    *Config
	http   *http.Client
	prefix string
}

func (s *agentSeedStrategy) source() msg.SeedSource { return msg.SeedSourceAgentStore }

type agentManifest struct {
	Entries []agentEntry `json:"entries"`
}

type agentEntry struct {
	TrackedFileID int64  `json:"tracked_file_id"`
	InstallPath   string `json:"install_path"`
	Scope         string `json:"scope"`
	SHA256        string `json:"sha256"`
	Size          int64  `json:"size"`
	VersionID     int64  `json:"version_id"`
}

func (s *agentSeedStrategy) fetchManifest(machineID string) ([]seedEntry, error) {
	u, err := buildURL(s.cfg.ServerURL, s.prefix+"/seed/manifest")
	if err != nil {
		return nil, err
	}
	q := u.Query()
	q.Set("machine_id", machineID)
	u.RawQuery = q.Encode()
	var mf agentManifest
	if err := getJSON(s.http, s.cfg.RunnerToken, u.String(), &mf); err != nil {
		return nil, err
	}
	out := make([]seedEntry, 0, len(mf.Entries))
	for _, e := range mf.Entries {
		out = append(out, seedEntry{
			InstallPath: e.InstallPath,
			SHA256:      e.SHA256,
			Size:        e.Size,
			VersionID:   e.VersionID,
			SourceKey:   map[string]any{"tracked_file_id": e.TrackedFileID},
		})
	}
	return out, nil
}

func (s *agentSeedStrategy) fetchContent(e seedEntry) ([]byte, error) {
	if e.VersionID == 0 {
		return nil, fmt.Errorf("missing version_id")
	}
	u, err := buildURL(s.cfg.ServerURL, fmt.Sprintf("%s/versions/%d/content", s.prefix, e.VersionID))
	if err != nil {
		return nil, err
	}
	var payload struct {
		Content string `json:"content"`
	}
	if err := getJSON(s.http, s.cfg.RunnerToken, u.String(), &payload); err != nil {
		return nil, err
	}
	return []byte(payload.Content), nil
}

func (s *agentSeedStrategy) postDrift(machineID string, e seedEntry, content []byte, note string) error {
	body := map[string]any{
		"machine_id": machineID,
		"content":    string(content),
		"note":       note,
	}
	for k, v := range e.SourceKey {
		body[k] = v
	}
	return postJSON(s.http, s.cfg.RunnerToken, mustURL(s.cfg.ServerURL, s.prefix+"/seed/drift"), body)
}

func (s *agentSeedStrategy) postObserve(machineID string, e seedEntry, sha string) error {
	body := map[string]any{
		"machine_id": machineID,
		"sha256":     sha,
	}
	for k, v := range e.SourceKey {
		body[k] = v
	}
	return postJSON(s.http, s.cfg.RunnerToken, mustURL(s.cfg.ServerURL, s.prefix+"/seed/observe"), body)
}

// ============================================
// skill-store strategy
// ============================================

type skillSeedStrategy struct {
	cfg    *Config
	http   *http.Client
	prefix string
}

func (s *skillSeedStrategy) source() msg.SeedSource { return msg.SeedSourceSkillStore }

type skillManifest struct {
	Enabled bool         `json:"enabled"`
	Entries []skillEntry `json:"entries"`
}

type skillEntry struct {
	SkillID     int64  `json:"skill_id"`
	Relpath     string `json:"relpath"`
	InstallPath string `json:"install_path"`
	SHA256      string `json:"sha256"`
	Size        int64  `json:"size"`
	VersionID   int64  `json:"version_id"`
}

func (s *skillSeedStrategy) fetchManifest(machineID string) ([]seedEntry, error) {
	u, err := buildURL(s.cfg.ServerURL, s.prefix+"/seed/manifest")
	if err != nil {
		return nil, err
	}
	q := u.Query()
	q.Set("machine_id", machineID)
	u.RawQuery = q.Encode()
	var mf skillManifest
	if err := getJSON(s.http, s.cfg.RunnerToken, u.String(), &mf); err != nil {
		return nil, err
	}
	if !mf.Enabled {
		return nil, nil
	}
	out := make([]seedEntry, 0, len(mf.Entries))
	for _, e := range mf.Entries {
		out = append(out, seedEntry{
			InstallPath: e.InstallPath,
			SHA256:      e.SHA256,
			Size:        e.Size,
			VersionID:   e.VersionID,
			SourceKey: map[string]any{
				"skill_id": e.SkillID,
				"relpath":  e.Relpath,
			},
		})
	}
	return out, nil
}

func (s *skillSeedStrategy) fetchContent(e seedEntry) ([]byte, error) {
	if e.VersionID == 0 {
		return nil, fmt.Errorf("missing version_id")
	}
	u, err := buildURL(s.cfg.ServerURL, fmt.Sprintf("%s/versions/%d/content", s.prefix, e.VersionID))
	if err != nil {
		return nil, err
	}
	var payload struct {
		Content string `json:"content"`
	}
	if err := getJSON(s.http, s.cfg.RunnerToken, u.String(), &payload); err != nil {
		return nil, err
	}
	return []byte(payload.Content), nil
}

func (s *skillSeedStrategy) postDrift(machineID string, e seedEntry, content []byte, note string) error {
	body := map[string]any{
		"machine_id": machineID,
		"content":    string(content),
		"note":       note,
	}
	for k, v := range e.SourceKey {
		body[k] = v
	}
	return postJSON(s.http, s.cfg.RunnerToken, mustURL(s.cfg.ServerURL, s.prefix+"/seed/drift"), body)
}

func (s *skillSeedStrategy) postObserve(machineID string, e seedEntry, sha string) error {
	body := map[string]any{
		"machine_id": machineID,
		"sha256":     sha,
	}
	for k, v := range e.SourceKey {
		body[k] = v
	}
	return postJSON(s.http, s.cfg.RunnerToken, mustURL(s.cfg.ServerURL, s.prefix+"/seed/observe"), body)
}

// ============================================
// HTTP helpers
// ============================================

func buildURL(base, path string) (*url.URL, error) {
	u, err := url.Parse(base)
	if err != nil {
		return nil, err
	}
	switch strings.ToLower(u.Scheme) {
	case "ws":
		u.Scheme = "http"
	case "wss":
		u.Scheme = "https"
	}
	u.Path = strings.TrimRight(u.Path, "/") + path
	return u, nil
}

func mustURL(base, path string) string {
	u, err := buildURL(base, path)
	if err != nil {
		return base + path
	}
	return u.String()
}

func getJSON(c *http.Client, token, urlStr string, dst any) error {
	req, err := http.NewRequest(http.MethodGet, urlStr, nil)
	if err != nil {
		return err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return json.NewDecoder(resp.Body).Decode(dst)
}

func postJSON(c *http.Client, token, urlStr string, body any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, urlStr, strings.NewReader(string(raw)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	return nil
}
