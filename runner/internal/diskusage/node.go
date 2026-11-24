// Package diskusage measures what fills a drive and deletes from it with
// guards. The Windows helper (cmd/llm-bridge-disk-usage) scans and deletes;
// the runner keeps each drive's latest scan as a tree of Nodes and answers
// the bridge's questions from it.
package diskusage

import (
	"sort"
	"strings"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

// Node is one entry in a scan: a directory with the totals of everything
// under it, a file, a link, or the smaller items of a directory rolled
// into one. It is also the helper's output format.
//
// FileCount and DirectoryCount count what is under a directory, not the
// directory itself. On a smaller_items node they count what the rolled-up
// entries add to their parent: each file once, each directory once plus
// what is under it.
type Node struct {
	Name                     string                 `json:"name"`
	Kind                     msg.DiskUsageEntryKind `json:"kind"`
	SizeBytes                uint64                 `json:"size_bytes"`
	FileCount                int64                  `json:"file_count,omitempty"`
	DirectoryCount           int64                  `json:"directory_count,omitempty"`
	CloudOnlyFileCount       int64                  `json:"cloud_only_file_count,omitempty"`
	UnreadableDirectoryCount int64                  `json:"unreadable_directory_count,omitempty"`
	ModifiedAtUnixSeconds    int64                  `json:"modified_at_unix_seconds,omitempty"`
	ItemCount                int64                  `json:"item_count,omitempty"` // smaller_items only
	PartlyDeleted            bool                   `json:"partly_deleted,omitempty"`
	Children                 []*Node                `json:"children,omitempty"`
}

// contribution is what a node adds to its parent's totals.
type contribution struct {
	sizeBytes                uint64
	fileCount                int64
	directoryCount           int64
	cloudOnlyFileCount       int64
	unreadableDirectoryCount int64
}

func (n *Node) contribution() contribution {
	switch n.Kind {
	case msg.DiskUsageEntryFile:
		c := contribution{sizeBytes: n.SizeBytes, fileCount: 1}
		if n.CloudOnlyFileCount > 0 {
			c.cloudOnlyFileCount = 1
		}
		return c
	case msg.DiskUsageEntryDirectory:
		return contribution{
			sizeBytes:                n.SizeBytes,
			fileCount:                n.FileCount,
			directoryCount:           n.DirectoryCount + 1,
			cloudOnlyFileCount:       n.CloudOnlyFileCount,
			unreadableDirectoryCount: n.UnreadableDirectoryCount,
		}
	case msg.DiskUsageEntrySmallerItems:
		return contribution{
			sizeBytes:                n.SizeBytes,
			fileCount:                n.FileCount,
			directoryCount:           n.DirectoryCount,
			cloudOnlyFileCount:       n.CloudOnlyFileCount,
			unreadableDirectoryCount: n.UnreadableDirectoryCount,
		}
	}
	return contribution{} // a link counts nothing
}

func (c *contribution) add(other contribution) {
	c.sizeBytes += other.sizeBytes
	c.fileCount += other.fileCount
	c.directoryCount += other.directoryCount
	c.cloudOnlyFileCount += other.cloudOnlyFileCount
	c.unreadableDirectoryCount += other.unreadableDirectoryCount
}

// KeepPolicy is which entries of a directory a scan keeps by name: the
// largest Largest at most, of which those past the first AlwaysKept only
// when they hold at least MinimumBytes. The rest become one smaller_items
// entry. A scan of a full 4 TB drive (2.6 million entries, measured on a
// Windows PC on 2026-11-24) keeps 1.5 million entries with only the cap of
// 200, and about 320,000 with the defaults: a few dozen megabytes to hold.
type KeepPolicy struct {
	Largest      int
	AlwaysKept   int
	MinimumBytes uint64
}

// DefaultKeepPolicy keeps what matters for freeing space.
var DefaultKeepPolicy = KeepPolicy{Largest: 200, AlwaysKept: 5, MinimumBytes: 1 << 20}

func (policy KeepPolicy) valid() bool {
	return policy.Largest >= 1 && policy.AlwaysKept >= 1 && policy.AlwaysKept <= policy.Largest
}

// finishDirectory sets a directory's totals from its children, sorts them
// largest first and rolls those the policy does not keep into one
// smaller_items node. unreadableItself counts the directory itself when
// the scan could not read it.
func (n *Node) finishDirectory(children []*Node, policy KeepPolicy, unreadableItself bool) {
	var total contribution
	kept := children[:0]
	for _, child := range children {
		if child == nil {
			continue
		}
		total.add(child.contribution())
		kept = append(kept, child)
	}
	if unreadableItself {
		total.unreadableDirectoryCount++
	}
	n.SizeBytes = total.sizeBytes
	n.FileCount = total.fileCount
	n.DirectoryCount = total.directoryCount
	n.CloudOnlyFileCount = total.cloudOnlyFileCount
	n.UnreadableDirectoryCount = total.unreadableDirectoryCount

	sortLargestFirst(kept)
	cut := min(len(kept), policy.Largest)
	for cut > policy.AlwaysKept && kept[cut-1].SizeBytes < policy.MinimumBytes {
		cut--
	}
	if cut < len(kept)-1 { // one entry left over stays itself
		rest := kept[cut:]
		smaller := &Node{Kind: msg.DiskUsageEntrySmallerItems, ItemCount: int64(len(rest))}
		var restTotal contribution
		for _, child := range rest {
			restTotal.add(child.contribution())
		}
		smaller.SizeBytes = restTotal.sizeBytes
		smaller.FileCount = restTotal.fileCount
		smaller.DirectoryCount = restTotal.directoryCount
		smaller.CloudOnlyFileCount = restTotal.cloudOnlyFileCount
		smaller.UnreadableDirectoryCount = restTotal.unreadableDirectoryCount
		kept = append(kept[:cut:cut], smaller)
	}
	if len(kept) == 0 {
		kept = nil
	}
	n.Children = kept
}

func sortLargestFirst(nodes []*Node) {
	sort.SliceStable(nodes, func(i, j int) bool {
		if nodes[i].Kind == msg.DiskUsageEntrySmallerItems || nodes[j].Kind == msg.DiskUsageEntrySmallerItems {
			return nodes[j].Kind == msg.DiskUsageEntrySmallerItems && nodes[i].Kind != msg.DiskUsageEntrySmallerItems
		}
		if nodes[i].SizeBytes != nodes[j].SizeBytes {
			return nodes[i].SizeBytes > nodes[j].SizeBytes
		}
		return nodes[i].Name < nodes[j].Name
	})
}

// ChildPath is the path of a child named name inside the directory at
// parentPath, in Windows form: `C:\` + `Games` is `C:\Games`.
func ChildPath(parentPath, name string) string {
	if strings.HasSuffix(parentPath, `\`) {
		return parentPath + name
	}
	return parentPath + `\` + name
}

// Find returns the node at path in the tree whose root is at rootPath
// (`C:\`), and the chain of its ancestors from the root, or nil when the
// tree does not hold it. Names match exactly: paths come from the tree.
func (root *Node) Find(rootPath, path string) (*Node, []*Node) {
	if path == rootPath {
		return root, nil
	}
	if !strings.HasPrefix(path, rootPath) {
		return nil, nil
	}
	rest := strings.TrimPrefix(path, rootPath)
	if rest == "" || strings.HasPrefix(rest, `\`) || strings.HasSuffix(rest, `\`) {
		return nil, nil
	}
	current := root
	var ancestors []*Node
	for _, name := range strings.Split(rest, `\`) {
		var next *Node
		for _, child := range current.Children {
			if child.Kind != msg.DiskUsageEntrySmallerItems && child.Name == name {
				next = child
				break
			}
		}
		if next == nil {
			return nil, nil
		}
		ancestors = append(ancestors, current)
		current = next
	}
	return current, ancestors
}

// RemoveDeleted takes a node a deletion removed whole out of its parent and
// out of every ancestor's totals.
func RemoveDeleted(node *Node, ancestors []*Node) {
	if len(ancestors) == 0 {
		return
	}
	gone := node.contribution()
	parent := ancestors[len(ancestors)-1]
	for i, child := range parent.Children {
		if child == node {
			parent.Children = append(parent.Children[:i:i], parent.Children[i+1:]...)
			break
		}
	}
	for _, ancestor := range ancestors {
		ancestor.subtract(gone)
	}
}

// SubtractPartlyDeleted takes what a deletion removed from inside node out
// of node's totals and its ancestors', and marks node partly deleted. The
// entries inside node are left as they were scanned.
func SubtractPartlyDeleted(node *Node, ancestors []*Node, deletedBytes uint64, deletedFileCount int64) {
	gone := contribution{sizeBytes: deletedBytes, fileCount: deletedFileCount}
	node.subtract(gone)
	node.PartlyDeleted = true
	for _, ancestor := range ancestors {
		ancestor.subtract(gone)
	}
}

func (n *Node) subtract(c contribution) {
	n.SizeBytes = saturatingSubtract(n.SizeBytes, c.sizeBytes)
	n.FileCount = max(0, n.FileCount-c.fileCount)
	n.DirectoryCount = max(0, n.DirectoryCount-c.directoryCount)
	n.CloudOnlyFileCount = max(0, n.CloudOnlyFileCount-c.cloudOnlyFileCount)
	n.UnreadableDirectoryCount = max(0, n.UnreadableDirectoryCount-c.unreadableDirectoryCount)
}

func saturatingSubtract(a, b uint64) uint64 {
	if b > a {
		return 0
	}
	return a - b
}

// Entry describes n, found at path, for the bridge.
func (n *Node) Entry(path string) msg.DiskUsageEntry {
	entry := msg.DiskUsageEntry{
		Name:                     n.Name,
		Path:                     path,
		Kind:                     n.Kind,
		SizeBytes:                n.SizeBytes,
		FileCount:                n.FileCount,
		DirectoryCount:           n.DirectoryCount,
		CloudOnlyFileCount:       n.CloudOnlyFileCount,
		UnreadableDirectoryCount: n.UnreadableDirectoryCount,
		ItemCount:                n.ItemCount,
		HasChildren:              len(n.Children) > 0,
		PartlyDeleted:            n.PartlyDeleted,
	}
	if n.Kind == msg.DiskUsageEntrySmallerItems {
		entry.Path = ""
	}
	if n.ModifiedAtUnixSeconds != 0 {
		modified := time.Unix(n.ModifiedAtUnixSeconds, 0).UTC()
		entry.ModifiedAt = &modified
	}
	return entry
}
