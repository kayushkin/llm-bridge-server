// Package serviceinventory joins healthcheck's view of the host's services to
// the processes behind them and the SQLite files those processes hold open,
// and reads those files back — schema and rows — without ever writing to one.
//
// Everything about a process is read from /proc: a systemd unit is found by
// the cgroup its processes sit in, an HTTP check by the process listening on
// the URL's port, and a database by the ".db" links in the process's fd
// table. None of it needs a DBus session, which llm-bridge.service does not
// have, and none of it needs the store to offer a schema route of its own.
package serviceinventory

import (
	"bufio"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// ProcRoot is where /proc is mounted. A test points it at a directory it
// built itself.
var ProcRoot = "/proc"

// listPIDs returns every numeric entry under ProcRoot.
func listPIDs() ([]int, error) {
	entries, err := os.ReadDir(ProcRoot)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", ProcRoot, err)
	}
	var pids []int
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || !e.IsDir() {
			continue
		}
		pids = append(pids, pid)
	}
	sort.Ints(pids)
	return pids, nil
}

// cgroupPath reads the unified-hierarchy ("0::") cgroup of one process. A
// process that exited between listing and reading is reported as an empty
// path, not an error: /proc is a moving target and a missing pid is normal.
func cgroupPath(pid int) (string, error) {
	f, err := os.Open(filepath.Join(ProcRoot, strconv.Itoa(pid), "cgroup"))
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "0::") {
			return strings.TrimPrefix(line, "0::"), nil
		}
	}
	return "", scanner.Err()
}

// CgroupBelongsToUnit reports whether a cgroup path is the unit's own cgroup
// or one under it.
//
// The user/system distinction is load-bearing: this host has run two units
// named kayushkin, one a user unit and one a failed system unit, and a match
// on the unit name alone would put the live site's pids under either.
func CgroupBelongsToUnit(cgroup, unit string, systemUnit bool) bool {
	suffix := "/" + unit + ".service"
	idx := strings.Index(cgroup, suffix)
	if idx < 0 {
		return false
	}
	rest := cgroup[idx+len(suffix):]
	if rest != "" && !strings.HasPrefix(rest, "/") {
		return false
	}
	if systemUnit {
		return strings.HasPrefix(cgroup, "/system.slice/")
	}
	return strings.Contains(cgroup, "/user@")
}

// PIDsForUnit returns every process in the unit's cgroup, lowest pid first.
func PIDsForUnit(unit string, systemUnit bool) ([]int, error) {
	pids, err := listPIDs()
	if err != nil {
		return nil, err
	}
	var found []int
	for _, pid := range pids {
		cg, err := cgroupPath(pid)
		if err != nil {
			return nil, fmt.Errorf("pid %d cgroup: %w", pid, err)
		}
		if cg != "" && CgroupBelongsToUnit(cg, unit, systemUnit) {
			found = append(found, pid)
		}
	}
	return found, nil
}

// ListeningPortOf returns the TCP port a health-check URL probes.
func ListeningPortOf(rawURL string) (int, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return 0, err
	}
	portText := u.Port()
	if portText == "" {
		switch u.Scheme {
		case "http":
			return 80, nil
		case "https":
			return 443, nil
		}
		return 0, fmt.Errorf("url %q names no port", rawURL)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		return 0, fmt.Errorf("url %q port: %w", rawURL, err)
	}
	return port, nil
}

// ListeningSocketInodes reads one /proc/net/tcp-format table and returns the
// socket inodes in LISTEN state on the port. Exported for the parser test;
// callers want PIDsListeningOn.
func ListeningSocketInodes(table string, port int) ([]uint64, error) {
	var inodes []uint64
	scanner := bufio.NewScanner(strings.NewReader(table))
	first := true
	for scanner.Scan() {
		if first { // header row
			first = false
			continue
		}
		fields := strings.Fields(scanner.Text())
		if len(fields) < 10 {
			continue
		}
		// fields[1] is local address as hex "ADDR:PORT"; fields[3] is state.
		if fields[3] != "0A" {
			continue
		}
		colon := strings.LastIndex(fields[1], ":")
		if colon < 0 {
			continue
		}
		localPort, err := strconv.ParseUint(fields[1][colon+1:], 16, 32)
		if err != nil {
			return nil, fmt.Errorf("parse local port %q: %w", fields[1], err)
		}
		if int(localPort) != port {
			continue
		}
		inode, err := strconv.ParseUint(fields[9], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("parse inode %q: %w", fields[9], err)
		}
		inodes = append(inodes, inode)
	}
	return inodes, scanner.Err()
}

// PIDsListeningOn returns every process holding a LISTEN socket on the port,
// over both IPv4 and IPv6, lowest pid first.
func PIDsListeningOn(port int) ([]int, error) {
	inodes := map[uint64]bool{}
	for _, name := range []string{"net/tcp", "net/tcp6"} {
		data, err := os.ReadFile(filepath.Join(ProcRoot, name))
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		found, err := ListeningSocketInodes(string(data), port)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		for _, inode := range found {
			inodes[inode] = true
		}
	}
	if len(inodes) == 0 {
		return nil, nil
	}
	pids, err := listPIDs()
	if err != nil {
		return nil, err
	}
	var found []int
	for _, pid := range pids {
		links, err := fdLinks(pid)
		if err != nil {
			return nil, err
		}
		for _, target := range links {
			if !strings.HasPrefix(target, "socket:[") {
				continue
			}
			inode, err := strconv.ParseUint(strings.TrimSuffix(strings.TrimPrefix(target, "socket:["), "]"), 10, 64)
			if err != nil {
				continue
			}
			if inodes[inode] {
				found = append(found, pid)
				break
			}
		}
	}
	return found, nil
}

// fdLinks returns the targets of every link in the process's fd table. A
// process we may not inspect, or one that has exited, yields nothing: both
// are normal on a shared host, and a permission failure on one pid must not
// hide the ones we can read.
func fdLinks(pid int) ([]string, error) {
	dir := filepath.Join(ProcRoot, strconv.Itoa(pid), "fd")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) || os.IsPermission(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", dir, err)
	}
	var targets []string
	for _, e := range entries {
		target, err := os.Readlink(filepath.Join(dir, e.Name()))
		if err != nil {
			continue // closed between listing and reading
		}
		targets = append(targets, target)
	}
	return targets, nil
}

// IsDatabasePath reports whether an fd target is a SQLite database file:
// by extension, and not a WAL, shm or journal sidecar, and not a file the
// process holds open after deleting.
func IsDatabasePath(target string) bool {
	if strings.HasSuffix(target, " (deleted)") {
		return false
	}
	switch strings.ToLower(filepath.Ext(target)) {
	case ".db", ".sqlite", ".sqlite3":
		return true
	}
	return false
}

// OpenDatabasePaths returns the distinct SQLite files the processes hold
// open, sorted.
func OpenDatabasePaths(pids []int) ([]string, error) {
	seen := map[string]bool{}
	for _, pid := range pids {
		links, err := fdLinks(pid)
		if err != nil {
			return nil, err
		}
		for _, target := range links {
			if IsDatabasePath(target) {
				seen[target] = true
			}
		}
	}
	paths := make([]string, 0, len(seen))
	for p := range seen {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	return paths, nil
}
