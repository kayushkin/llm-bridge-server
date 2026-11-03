package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	bundlestore "github.com/kayushkin/bundle-store"
	"github.com/kayushkin/llm-bridge/servicesettings"
)

// This file is the -check-members pre-flight. A bundle stores each member as
// the id skill-store or tool-store assigned, with the name beside it only as a
// label. Either store can delete or rename that row at any time, and nothing
// tells bundle-store. The check reads every stored bundle, asks both stores for
// every row they hold, and reports:
//
//   - a member id the owning store no longer carries. That member resolves to
//     nothing in a session, so it fails the check.
//   - a member id the store still carries under a different name than the
//     stored label. The id is what the bundle means, so this is reported and
//     does not fail the check; the label is only out of date.

// storeEntry is the shape both skill-store and tool-store list endpoints share.
type storeEntry struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// memberIndex answers "does the owning store still carry this id, and under
// what name".
type memberIndex struct {
	byKind map[bundlestore.MemberKind]map[int64]storeEntry
}

func newMemberIndex(skills, tools []storeEntry) *memberIndex {
	return &memberIndex{byKind: map[bundlestore.MemberKind]map[int64]storeEntry{
		bundlestore.MemberSkill: indexByID(skills),
		bundlestore.MemberTool:  indexByID(tools),
	}}
}

func indexByID(entries []storeEntry) map[int64]storeEntry {
	out := make(map[int64]storeEntry, len(entries))
	for _, e := range entries {
		out[e.ID] = e
	}
	return out
}

// memberCheckReport is the outcome of checking stored bundles against the two
// owning stores. Missing lines fail the check; relabeled lines do not.
type memberCheckReport struct {
	Missing   []string
	Relabeled []string
}

// checkStoredMembers checks every member of every given bundle, whatever its
// effect or the bundle's enabled flag: a denied tool whose id the store no
// longer carries denies nothing, and a disabled bundle can be enabled again
// tomorrow. It reports every problem rather than stopping at the first, so one
// deploy shows the whole list.
func checkStoredMembers(index *memberIndex, bundles []bundlestore.Bundle) memberCheckReport {
	var report memberCheckReport
	for _, b := range bundles {
		for _, m := range b.Members {
			effect := m.Effect
			if effect == "" {
				effect = bundlestore.MemberInclude
			}
			entry, carried := index.byKind[m.Kind][m.ID]
			if !carried {
				report.Missing = append(report.Missing, fmt.Sprintf(
					"bundle %s (id %d): %s %s id %d, stored as %q: %s has no %s with that id",
					b.Name, b.ID, effect, m.Kind, m.ID, m.Name, m.Kind.OwningStore(), m.Kind))
				continue
			}
			if entry.Name != m.Name {
				report.Relabeled = append(report.Relabeled, fmt.Sprintf(
					"bundle %s (id %d): %s %s id %d is stored as %q, but %s now names it %q",
					b.Name, b.ID, effect, m.Kind, m.ID, m.Name, m.Kind.OwningStore(), entry.Name))
			}
		}
	}
	return report
}

// runMemberCheck opens the store in the data directory the server would use,
// checks every stored bundle against skill-store and tool-store, logs one line
// per problem, and returns an error when any member id is missing upstream.
func runMemberCheck(settings *servicesettings.Registry) error {
	store, err := bundlestore.Open(settings.String(bundlestore.SettingDataDirectory))
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer store.Close()

	bundles, err := store.ListBundles(bundlestore.ListFilter{})
	if err != nil {
		return fmt.Errorf("list bundles: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), upstreamListTimeout)
	defer cancel()
	index, err := loadMemberIndex(ctx,
		settings.String(bundlestore.SettingSkillStoreURL),
		settings.String(bundlestore.SettingToolStoreURL))
	if err != nil {
		return fmt.Errorf("read member ids: %w", err)
	}

	report := checkStoredMembers(index, bundles)
	for _, line := range report.Relabeled {
		log.Printf("RELABELED (not a failure): %s", line)
	}
	for _, line := range report.Missing {
		log.Printf("MISSING: %s", line)
	}
	memberCount := 0
	for _, b := range bundles {
		memberCount += len(b.Members)
	}
	if len(report.Missing) > 0 {
		return fmt.Errorf("%d of %d member(s) across %d stored bundle(s) name an id their owning store no longer carries",
			len(report.Missing), memberCount, len(bundles))
	}
	log.Printf("check-members: all %d member(s) across %d stored bundle(s) are carried by their owning store (%d label(s) out of date)",
		memberCount, len(bundles), len(report.Relabeled))
	return nil
}

// loadMemberIndex reads every skill and every tool once. If either store cannot
// be reached the check fails rather than reporting members it could not check.
func loadMemberIndex(ctx context.Context, skillStoreBaseURL, toolStoreBaseURL string) (*memberIndex, error) {
	skills, err := fetchEntries(ctx, skillStoreBaseURL+"/skills")
	if err != nil {
		return nil, fmt.Errorf("skill-store: %w", err)
	}
	tools, err := fetchEntries(ctx, toolStoreBaseURL+"/tools")
	if err != nil {
		return nil, fmt.Errorf("tool-store: %w", err)
	}
	return newMemberIndex(skills, tools), nil
}

func fetchEntries(ctx context.Context, url string) ([]storeEntry, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: HTTP %d", url, resp.StatusCode)
	}
	var entries []storeEntry
	if err := json.NewDecoder(resp.Body).Decode(&entries); err != nil {
		return nil, fmt.Errorf("GET %s: decode: %w", url, err)
	}
	return entries, nil
}

// upstreamListTimeout bounds the two list calls the check makes.
const upstreamListTimeout = 15 * time.Second
