package diskusage

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/kayushkin/llm-bridge/msg"
)

func writeFile(t *testing.T, path string, size int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestScanTotalsKeepsTheLargestAndDoesNotFollowLinks(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "big", "a.bin"), 5000)
	writeFile(t, filepath.Join(root, "big", "inner", "b.bin"), 3000)
	for i := 0; i < 6; i++ {
		writeFile(t, filepath.Join(root, "many", fmt.Sprintf("f%d", i)), 100*(i+1))
	}
	writeFile(t, filepath.Join(root, "top.txt"), 10)
	outside := t.TempDir()
	writeFile(t, filepath.Join(outside, "huge.bin"), 100000)
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}

	scanner := &Scanner{Keep: KeepPolicy{Largest: 3, AlwaysKept: 3}, Parallelism: 4}
	tree, err := scanner.Scan(root, `C:\`)
	if err != nil {
		t.Fatal(err)
	}
	if tree.SizeBytes != 5000+3000+2100+10 {
		t.Fatalf("root size %d, want %d (the link must count nothing)", tree.SizeBytes, 5000+3000+2100+10)
	}
	if tree.FileCount != 9 || tree.DirectoryCount != 3 {
		t.Fatalf("root counts %d files %d dirs, want 9 and 3", tree.FileCount, tree.DirectoryCount)
	}
	if tree.Children[0].Name != "big" || tree.Children[1].Name != "many" {
		t.Fatalf("children not largest first: %s, %s", tree.Children[0].Name, tree.Children[1].Name)
	}
	many, _ := tree.Find(`C:\`, `C:\many`)
	if many == nil || len(many.Children) != 4 {
		t.Fatalf("many: %+v", many)
	}
	smaller := many.Children[3]
	if smaller.Kind != msg.DiskUsageEntrySmallerItems || smaller.ItemCount != 3 || smaller.SizeBytes != 100+200+300 || smaller.FileCount != 3 {
		t.Fatalf("smaller items: %+v", smaller)
	}
	link, _ := tree.Find(`C:\`, `C:\link`)
	if link == nil || link.Kind != msg.DiskUsageEntryLink || link.SizeBytes != 0 {
		t.Fatalf("link: %+v", link)
	}

	inner, ancestors := tree.Find(`C:\`, `C:\big\inner`)
	if inner == nil || len(ancestors) != 2 {
		t.Fatalf("find inner: %+v %d", inner, len(ancestors))
	}
	RemoveDeleted(inner, ancestors)
	big, _ := tree.Find(`C:\`, `C:\big`)
	if big.SizeBytes != 5000 || big.FileCount != 1 || big.DirectoryCount != 0 || len(big.Children) != 1 {
		t.Fatalf("big after removing inner: %+v", big)
	}
	if tree.SizeBytes != 5000+2100+10 || tree.FileCount != 8 || tree.DirectoryCount != 2 {
		t.Fatalf("root after removing inner: %+v", tree)
	}

	SubtractPartlyDeleted(many, []*Node{tree}, 1000, 2)
	if !many.PartlyDeleted || many.SizeBytes != 1100 || tree.SizeBytes != 5000+1100+10 {
		t.Fatalf("after a partial deletion: many %+v root %d", many, tree.SizeBytes)
	}

	// With a size floor, entries past the always-kept ones go when small.
	floored := &Scanner{Keep: KeepPolicy{Largest: 10, AlwaysKept: 2, MinimumBytes: 450}, Parallelism: 1}
	flooredTree, err := floored.Scan(filepath.Join(root, "many"), `C:\many`)
	if err != nil {
		t.Fatal(err)
	}
	// f5 600, f4 500 kept; f3 400 is past the two always kept and under 450.
	if len(flooredTree.Children) != 3 || flooredTree.Children[2].Kind != msg.DiskUsageEntrySmallerItems || flooredTree.Children[2].ItemCount != 4 || flooredTree.SizeBytes != 2100 {
		t.Fatalf("floored: %+v", flooredTree.Children)
	}

	for _, missing := range []string{`C:\nothing`, `C:\big\`, `D:\big`, `C:\\big`, `C:\many\`} {
		if node, _ := tree.Find(`C:\`, missing); node != nil {
			t.Errorf("Find(%q) found %s", missing, node.Name)
		}
	}
}

func TestDeletionRemovesLinksButNotWhatTheyPointAt(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "delete-me")
	writeFile(t, filepath.Join(target, "a.bin"), 400)
	writeFile(t, filepath.Join(target, "sub", "b.bin"), 600)
	keep := filepath.Join(root, "keep")
	writeFile(t, filepath.Join(keep, "precious.bin"), 1000)
	if err := os.Symlink(keep, filepath.Join(target, "sub", "link-to-keep")); err != nil {
		t.Fatal(err)
	}

	var deletion Deletion
	if err := deletion.Run(target, msg.DiskUsageEntryFile); err == nil {
		t.Fatal("deleted a directory the caller called a file")
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatal("a refused deletion removed something")
	}
	if err := deletion.Run(target, msg.DiskUsageEntryDirectory); err != nil {
		t.Fatal(err)
	}
	progress := deletion.Progress()
	if progress.DeletedBytes != 1000 || progress.DeletedFileCount != 2 || progress.FailedEntryCount != 0 {
		t.Fatalf("progress %+v", progress)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("target still there: %v", err)
	}
	if _, err := os.Stat(filepath.Join(keep, "precious.bin")); err != nil {
		t.Fatalf("the link's target was touched: %v", err)
	}
}
