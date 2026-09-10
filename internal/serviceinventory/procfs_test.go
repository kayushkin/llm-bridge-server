package serviceinventory

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func TestCgroupBelongsToUnit(t *testing.T) {
	user := "/user.slice/user-1000.slice/user@1000.service/app.slice/auth-store.service"
	system := "/system.slice/llm-bridge.service"
	cases := []struct {
		cgroup, unit string
		systemUnit   bool
		want         bool
	}{
		{user, "auth-store", false, true},
		{user, "auth-store", true, false}, // the same name as a SYSTEM unit is another unit
		{system, "llm-bridge", true, true},
		{system, "llm-bridge", false, false},
		{user, "auth", false, false},  // prefix of the unit name is not the unit
		{user, "store", false, false}, // nor is a suffix
		{system + "/child", "llm-bridge", true, true},
		{"/system.slice/llm-bridge.service.mount", "llm-bridge", true, false},
	}
	for _, c := range cases {
		if got := CgroupBelongsToUnit(c.cgroup, c.unit, c.systemUnit); got != c.want {
			t.Errorf("CgroupBelongsToUnit(%q, %q, %v) = %v, want %v", c.cgroup, c.unit, c.systemUnit, got, c.want)
		}
	}
}

func TestListeningSocketInodes(t *testing.T) {
	table := `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 0100007F:21C2 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 15502 1 0000000000000000 100 0 0 10 0
   1: 00000000:1FE0 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 99001 1 0000000000000000 100 0 0 10 0
   2: 0100007F:1FE0 0100007F:C350 01 00000000:00000000 00:00000000 00000000  1000        0 99002 1 0000000000000000 100 0 0 10 0
`
	got, err := ListeningSocketInodes(table, 8160)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != 99001 {
		t.Fatalf("port 8160 listeners = %v, want [99001] (the ESTABLISHED row on the same port must not count)", got)
	}
	got, err = ListeningSocketInodes(table, 8642)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != 15502 {
		t.Fatalf("port 8642 listeners = %v, want [15502]", got)
	}
}

func TestListeningPortOf(t *testing.T) {
	for raw, want := range map[string]int{"http://localhost:8192/health": 8192, "http://example.com/": 80, "https://example.com/x": 443} {
		got, err := ListeningPortOf(raw)
		if err != nil || got != want {
			t.Errorf("ListeningPortOf(%q) = %d, %v; want %d", raw, got, err, want)
		}
	}
}

// A fake /proc: two pids in a user unit's cgroup, one holding a .db and its
// -wal sidecar, one holding a deleted .db and a socket.
func TestPIDsForUnitAndOpenDatabasePaths(t *testing.T) {
	root := t.TempDir()
	ProcRoot = root
	t.Cleanup(func() { ProcRoot = "/proc" })
	dbFile := filepath.Join(root, "real.db")
	if err := os.WriteFile(dbFile, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	mkpid := func(pid int, cgroup string, links map[string]string) {
		dir := filepath.Join(root, strconv.Itoa(pid))
		if err := os.MkdirAll(filepath.Join(dir, "fd"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "cgroup"), []byte("0::"+cgroup+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		for name, target := range links {
			if err := os.Symlink(target, filepath.Join(dir, "fd", name)); err != nil {
				t.Fatal(err)
			}
		}
	}
	mkpid(10, "/user.slice/user-1000.slice/user@1000.service/app.slice/quote-store.service",
		map[string]string{"3": dbFile, "4": dbFile + "-wal", "5": "socket:[123]"})
	mkpid(11, "/user.slice/user-1000.slice/user@1000.service/app.slice/quote-store.service",
		map[string]string{"3": "/gone/old.db (deleted)"})
	mkpid(12, "/system.slice/other.service", map[string]string{"3": "/elsewhere/x.db"})
	if err := os.WriteFile(filepath.Join(root, "self"), nil, 0o644); err != nil { // a non-numeric entry, like the real /proc
		t.Fatal(err)
	}

	pids, err := PIDsForUnit("quote-store", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(pids) != 2 || pids[0] != 10 || pids[1] != 11 {
		t.Fatalf("pids = %v, want [10 11]", pids)
	}
	paths, err := OpenDatabasePaths(pids)
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 1 || paths[0] != dbFile {
		t.Fatalf("paths = %v, want [%s]: the -wal sidecar, the socket and the deleted file must not count", paths, dbFile)
	}
}
