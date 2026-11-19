package main

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"sync"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

// ServiceRegistry tracks long-lived backend services the runner deploys
// on behalf of harness instances (e.g. inber-server for the inber
// harness). Distinct from SubprocessRegistry, which handles per-session
// wrapper subprocesses; services here outlive sessions — they're
// shared across every spawn that needs them and only die on runner
// shutdown.
//
// Operations are idempotent: Ensure(svc) does nothing if the named
// service is already running and healthchecks pass, and starts it
// otherwise. The bridge side decides what services a harness needs
// per-spawn via msg.HarnessService manifests.
type ServiceRegistry struct {
	mu       sync.Mutex
	services map[string]*managedService
}

type managedService struct {
	manifest msg.HarnessService
	cmd      *exec.Cmd
	logFile  *os.File
}

// NewServiceRegistry returns an empty registry.
func NewServiceRegistry() *ServiceRegistry {
	return &ServiceRegistry{services: make(map[string]*managedService)}
}

// Ensure makes the named service running and healthy. Returns nil when
// the service already passes its healthcheck — no restart, no extra
// state churn — or after a successful start.
func (r *ServiceRegistry) Ensure(m msg.HarnessService) error {
	if m.Name == "" {
		return fmt.Errorf("service manifest missing name")
	}
	if m.HealthURL == "" {
		return fmt.Errorf("service %q manifest missing health_url", m.Name)
	}

	// Quick path: already healthy.
	if probeHealth(m.HealthURL, 1*time.Second) {
		return nil
	}

	r.mu.Lock()
	existing := r.services[m.Name]
	r.mu.Unlock()

	if existing != nil {
		// We thought it was running. Reap if it's actually dead so we
		// can restart cleanly.
		if existing.cmd.ProcessState != nil && existing.cmd.ProcessState.Exited() {
			log.Printf("[runner] service %s exited (code=%d); restarting", m.Name, existing.cmd.ProcessState.ExitCode())
			r.mu.Lock()
			delete(r.services, m.Name)
			r.mu.Unlock()
		} else {
			// Still running but unhealthy — give it a few seconds before forcing a restart.
			if probeHealth(m.HealthURL, 5*time.Second) {
				return nil
			}
		}
	}

	return r.start(m)
}

// start downloads the binary if needed, launches it as a child process
// with the manifest's args + env, and waits for the health check to
// come up green. Caller already holds no locks; we serialize start
// internally.
func (r *ServiceRegistry) start(m msg.HarnessService) error {
	binPath, err := resolveServiceBinary(m)
	if err != nil {
		return fmt.Errorf("resolve %s binary: %w", m.Name, err)
	}

	logPath := m.Stdout
	if logPath == "" {
		logPath = defaultServiceLogPath(m.Name)
	}
	if err := os.MkdirAll(filepath.Dir(logPath), 0755); err != nil {
		return fmt.Errorf("create log dir: %w", err)
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return fmt.Errorf("open log file: %w", err)
	}

	cmd := exec.Command(binPath, m.Args...)
	cmd.Env = append(os.Environ(), m.Env...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		logFile.Close()
		return fmt.Errorf("start %s: %w", m.Name, err)
	}

	r.mu.Lock()
	r.services[m.Name] = &managedService{manifest: m, cmd: cmd, logFile: logFile}
	r.mu.Unlock()

	log.Printf("[runner] service %s started (pid=%d, log=%s)", m.Name, cmd.Process.Pid, logPath)

	// Wait up to 30s for /health to come up. Most backends take a few
	// seconds; the long ceiling tolerates slow first-run init (DB
	// migration, etc.).
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if probeHealth(m.HealthURL, 2*time.Second) {
			log.Printf("[runner] service %s healthy at %s", m.Name, m.HealthURL)
			return nil
		}
		// Did the process exit during startup? Surface that.
		if cmd.ProcessState != nil {
			return fmt.Errorf("%s exited during startup (code=%d); see %s", m.Name, cmd.ProcessState.ExitCode(), logPath)
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("%s did not become healthy within 30s; see %s", m.Name, logPath)
}

// KillAll stops every managed service. Called on runner shutdown so
// orphaned daemons don't outlive their parent.
func (r *ServiceRegistry) KillAll() {
	r.mu.Lock()
	services := make([]*managedService, 0, len(r.services))
	for _, s := range r.services {
		services = append(services, s)
	}
	r.services = make(map[string]*managedService)
	r.mu.Unlock()
	for _, s := range services {
		if s.cmd.Process != nil {
			_ = s.cmd.Process.Kill()
		}
		if s.logFile != nil {
			_ = s.logFile.Close()
		}
	}
}

// resolveServiceBinary returns an absolute path to the service binary,
// downloading it from the manifest's BinaryURL when not already on
// disk. Mirrors installCurrentHarnessBinary for wrapper binaries but uses the
// service's `Name` for the install path so unrelated services don't
// collide.
func resolveServiceBinary(m msg.HarnessService) (string, error) {
	if m.BinaryPath != "" {
		if _, err := os.Stat(m.BinaryPath); err == nil {
			return m.BinaryPath, nil
		}
	}
	dest, err := serviceBinaryPath(m.Name)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(dest); err == nil {
		return dest, nil
	}
	if m.BinaryURL == "" {
		return "", fmt.Errorf("binary missing at %s and manifest has no BinaryURL", dest)
	}
	if err := downloadFile(m.BinaryURL, dest); err != nil {
		return "", fmt.Errorf("download %s from %s: %w", m.Name, m.BinaryURL, err)
	}
	return dest, nil
}

func serviceBinaryPath(name string) (string, error) {
	u, err := user.Current()
	if err != nil {
		return "", err
	}
	return filepath.Join(u.HomeDir, ".local", "bin", name), nil
}

func defaultServiceLogPath(name string) string {
	u, err := user.Current()
	if err != nil {
		return "/tmp/llm-bridge-" + name + ".log"
	}
	return filepath.Join(u.HomeDir, ".cache", "llm-bridge-runner", name+".log")
}

// downloadFile fetches a binary into dest atomically via a sibling
// .part file + rename, so an interrupted download can't leave half a
// binary on PATH.
func downloadFile(url, dest string) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
		return err
	}
	httpClient := &http.Client{Timeout: 60 * time.Second}
	resp, err := httpClient.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	tmp, err := os.CreateTemp(filepath.Dir(dest), filepath.Base(dest)+".part-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	if _, err := io.Copy(tmp, resp.Body); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return err
	}
	tmp.Close()
	if err := os.Chmod(tmpPath, 0755); err != nil {
		os.Remove(tmpPath)
		return err
	}
	if err := os.Rename(tmpPath, dest); err != nil {
		os.Remove(tmpPath)
		return err
	}
	return nil
}

// probeHealth runs a short GET against url. Returns true on 2xx.
// Used both as the "is it already up?" gate and as the post-start
// readiness check.
func probeHealth(url string, timeout time.Duration) bool {
	client := &http.Client{Timeout: timeout}
	resp, err := client.Get(url)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode/100 == 2
}
