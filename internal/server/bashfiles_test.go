package server

import (
	"reflect"
	"sort"
	"testing"
)

func TestExtractBashFilePaths(t *testing.T) {
	cases := []struct {
		name string
		cmd  string
		cwd  string
		want []string
	}{
		{
			name: "rm single",
			cmd:  "rm /tmp/foo",
			want: []string{"/tmp/foo"},
		},
		{
			name: "rm multiple with flags",
			cmd:  "rm -rf /tmp/a /tmp/b /tmp/c",
			want: []string{"/tmp/a", "/tmp/b", "/tmp/c"},
		},
		{
			name: "touch with relative + cwd",
			cmd:  "touch foo.txt bar.txt",
			cwd:  "/work",
			want: []string{"/work/bar.txt", "/work/foo.txt"},
		},
		{
			name: "redirect overwrite",
			cmd:  "echo hi > /tmp/out",
			want: []string{"/tmp/out"},
		},
		{
			name: "redirect append",
			cmd:  "echo hi >> /tmp/out",
			want: []string{"/tmp/out"},
		},
		{
			name: "redirect stderr-merged",
			cmd:  "make &> /tmp/build.log",
			want: []string{"/tmp/build.log"},
		},
		{
			name: "cp dest only",
			cmd:  "cp src.txt dst.txt",
			cwd:  "/work",
			want: []string{"/work/dst.txt"},
		},
		{
			name: "mv records both ends",
			cmd:  "mv /a /b",
			want: []string{"/a", "/b"},
		},
		{
			name: "ln symlink dest",
			cmd:  "ln -s /etc/hosts /tmp/link",
			want: []string{"/tmp/link"},
		},
		{
			name: "sed -i",
			cmd:  "sed -i 's/x/y/' /etc/hosts",
			want: []string{"/etc/hosts"},
		},
		{
			name: "sed without -i is read-only",
			cmd:  "sed 's/x/y/' /etc/hosts",
			want: nil,
		},
		{
			name: "tee multiple",
			cmd:  "echo hi | tee /tmp/a /tmp/b",
			want: []string{"/tmp/a", "/tmp/b"},
		},
		{
			name: "dd of=",
			cmd:  "dd if=/dev/zero of=/tmp/blob bs=1M count=1",
			want: []string{"/tmp/blob"},
		},
		{
			name: "compound &&",
			cmd:  "rm /tmp/a && touch /tmp/b",
			want: []string{"/tmp/a", "/tmp/b"},
		},
		{
			name: "ignores variable expansion",
			cmd:  "rm $TARGET",
			want: nil,
		},
		{
			name: "ignores command substitution",
			cmd:  "rm $(ls /tmp)",
			want: nil,
		},
		{
			name: "ignores glob",
			cmd:  "rm /tmp/*.txt",
			want: nil,
		},
		{
			name: "deduplicates",
			cmd:  "touch /tmp/a /tmp/a",
			want: []string{"/tmp/a"},
		},
		{
			name: "empty",
			cmd:  "",
			want: nil,
		},
		{
			name: "read-only commands ignored",
			cmd:  "cat /etc/hosts && grep foo /etc/passwd",
			want: nil,
		},
		{
			name: "quoted path",
			cmd:  `rm "/tmp/space me.txt"`,
			want: []string{"/tmp/space me.txt"},
		},
		{
			name: "absolute rm path with cwd",
			cmd:  "rm /tmp/foo",
			cwd:  "/some/cwd",
			want: []string{"/tmp/foo"},
		},
		{
			name: "full-path binary still recognized",
			cmd:  "/usr/bin/rm /tmp/foo",
			want: []string{"/tmp/foo"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := extractBashFilePaths(tc.cmd, tc.cwd)
			// Order isn't part of the contract — sort for stable comparison.
			sortCopy := append([]string(nil), got...)
			sort.Strings(sortCopy)
			wantCopy := append([]string(nil), tc.want...)
			sort.Strings(wantCopy)
			if !reflect.DeepEqual(sortCopy, wantCopy) {
				t.Fatalf("extractBashFilePaths(%q, %q):\n  got  %v\n  want %v",
					tc.cmd, tc.cwd, sortCopy, wantCopy)
			}
		})
	}
}
