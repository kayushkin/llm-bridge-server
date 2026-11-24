package skillstore

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/kayushkin/llm-bridge-server/postgresstorage"

	_ "modernc.org/sqlite"
)

// standaloneColumns are the columns of each table of the SQLite database the
// standalone skill-store kept. The import refuses a file whose tables have any
// other columns: a column it does not know is data it would drop. The tables
// are listed in the order they are copied, parents before the rows that point
// at them.
var standaloneColumns = []struct {
	table   string
	columns []string
}{
	{"sources", []string{"id", "name", "kind", "local_path", "repo_url", "branch", "subpath", "trust_tier", "license",
		"last_synced_at", "last_commit", "last_skill_count", "enabled", "created_at", "updated_at"}},
	{"skills", []string{"id", "source_id", "name", "display_name", "description", "skill_md", "source_path",
		"source_commit", "content_hash", "file_count", "tags", "enabled", "created_at", "updated_at"}},
	{"installations", []string{"skill_id", "target_path", "installed_at"}},
	{"seed_file_versions", []string{"id", "skill_id", "relpath", "sha256", "size", "content_bytes", "source",
		"machine_id", "note", "created_at"}},
	{"machine_seed_profiles", []string{"machine_id", "enabled", "created_at", "updated_at"}},
	{"machine_seed_state", []string{"machine_id", "skill_id", "relpath", "observed_sha", "observed_at",
		"last_pushed_version_id", "last_pushed_at"}},
}

// ImportCounts says how many rows of each table an import copied, and the
// number each numbered table goes on from.
type ImportCounts struct {
	Sources               int   `json:"sources"`
	Skills                int   `json:"skills"`
	Installations         int   `json:"installations"`
	SeedFileVersions      int   `json:"seed_file_versions"`
	SeedFileVersionBytes  int64 `json:"seed_file_version_bytes"`
	MachineSeedProfiles   int   `json:"machine_seed_profiles"`
	MachineSeedStates     int   `json:"machine_seed_states"`
	LastSourceID          int64 `json:"last_source_id"`
	LastSkillID           int64 `json:"last_skill_id"`
	LastSeedFileVersionID int64 `json:"last_seed_file_version_id"`
}

// ImportStandaloneDatabase copies every row of the SQLite file of a standalone
// skill-store into the module behind db, as it is: ids, times, SKILL.md texts,
// seed version bytes, NULLs and all. It runs in one transaction, so it copies
// everything or nothing, and it reads seed_file_versions one row at a time, so
// a file holding gigabytes of versions never sits in memory whole.
//
// It copies rows only. A skill's sibling files, the cloned sources and the
// installed copies are files on disk, not rows: they stay in the standalone
// store's data directory (skills/<id>/, repos/<name>/) and install directory,
// and the server must be configured with those same directories for the
// imported rows to find them.
//
// It refuses a module that already holds any row, so it can never mix two
// registries or run twice, and it refuses a file it cannot copy whole: a table
// with a column it does not know, or a value the module's columns cannot hold
// as it was (an enabled flag other than 0 or 1, a NULL where the store could
// never read one). Afterwards each numbered table numbers on from the highest
// number the standalone store had handed out (its sqlite_sequence, when that is
// higher than the last row copied), so no id that bundle-store, grant-store or
// a runner already holds is handed out again.
func ImportStandaloneDatabase(ctx context.Context, db *sql.DB, sqlitePath string) (ImportCounts, error) {
	var counts ImportCounts
	if _, err := os.Stat(sqlitePath); err != nil {
		return counts, fmt.Errorf("the standalone database: %w", err)
	}
	source, err := sql.Open("sqlite", "file:"+sqlitePath+"?mode=ro")
	if err != nil {
		return counts, fmt.Errorf("opening %s: %w", sqlitePath, err)
	}
	defer source.Close()
	for _, table := range standaloneColumns {
		if err := checkStandaloneColumns(ctx, source, table.table, table.columns); err != nil {
			return counts, fmt.Errorf("%s: %w", sqlitePath, err)
		}
	}
	handedOut, err := standaloneSequences(ctx, source)
	if err != nil {
		return counts, fmt.Errorf("%s: %w", sqlitePath, err)
	}

	err = postgresstorage.InTransaction(ctx, db, func(tx *sql.Tx) error {
		for _, table := range standaloneColumns {
			var held int
			if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table.table).Scan(&held); err != nil {
				return fmt.Errorf("counting %s: %w", table.table, err)
			}
			if held > 0 {
				return fmt.Errorf("the skills module already holds %d rows in %s; the import copies into an empty module only", held, table.table)
			}
		}
		var err error
		if counts.Sources, counts.LastSourceID, err = importSources(ctx, source, tx); err != nil {
			return err
		}
		if counts.Skills, counts.LastSkillID, err = importSkills(ctx, source, tx); err != nil {
			return err
		}
		if counts.Installations, err = importInstallations(ctx, source, tx); err != nil {
			return err
		}
		if counts.SeedFileVersions, counts.SeedFileVersionBytes, counts.LastSeedFileVersionID, err = importSeedFileVersions(ctx, source, tx); err != nil {
			return err
		}
		if counts.MachineSeedProfiles, err = importMachineSeedProfiles(ctx, source, tx); err != nil {
			return err
		}
		if counts.MachineSeedStates, err = importMachineSeedStates(ctx, source, tx); err != nil {
			return err
		}
		for _, numbering := range []struct {
			table string
			last  *int64
		}{
			{"sources", &counts.LastSourceID},
			{"skills", &counts.LastSkillID},
			{"seed_file_versions", &counts.LastSeedFileVersionID},
		} {
			if handedOut[numbering.table] > *numbering.last {
				*numbering.last = handedOut[numbering.table]
			}
			if err := postgresstorage.ContinueNumberingAfter(ctx, tx, numbering.table, "id", *numbering.last); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return ImportCounts{}, err
	}
	return counts, nil
}

func checkStandaloneColumns(ctx context.Context, source *sql.DB, table string, want []string) error {
	rows, err := source.QueryContext(ctx, "SELECT name FROM pragma_table_info(?)", table)
	if err != nil {
		return fmt.Errorf("reading the columns of %s: %w", table, err)
	}
	defer rows.Close()
	var found []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return err
		}
		found = append(found, name)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(found) == 0 {
		return fmt.Errorf("it has no table %s, so it is not a skill-store database", table)
	}
	sortedWant := append([]string{}, want...)
	sort.Strings(sortedWant)
	sort.Strings(found)
	if strings.Join(found, ",") != strings.Join(sortedWant, ",") {
		return fmt.Errorf("table %s has columns %s, and the import knows %s; it copies only a table it can copy whole",
			table, strings.Join(found, ", "), strings.Join(sortedWant, ", "))
	}
	return nil
}

// standaloneSequences reads the highest number SQLite's AUTOINCREMENT handed
// out for each table, which is above the last row when the newest rows were
// deleted. A file with no sqlite_sequence table has handed out nothing past
// its rows.
func standaloneSequences(ctx context.Context, source *sql.DB) (map[string]int64, error) {
	var present int
	if err := source.QueryRowContext(ctx, "SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='sqlite_sequence'").Scan(&present); err != nil {
		return nil, fmt.Errorf("looking for sqlite_sequence: %w", err)
	}
	handedOut := map[string]int64{}
	if present == 0 {
		return handedOut, nil
	}
	rows, err := source.QueryContext(ctx, "SELECT name, seq FROM sqlite_sequence")
	if err != nil {
		return nil, fmt.Errorf("reading sqlite_sequence: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var seq int64
		if err := rows.Scan(&name, &seq); err != nil {
			return nil, fmt.Errorf("reading sqlite_sequence: %w", err)
		}
		handedOut[name] = seq
	}
	return handedOut, rows.Err()
}

// standaloneFlag turns a stored 0 or 1 into a boolean. The standalone store
// read only 1 as on, so anything but 0 or 1 has no faithful copy.
func standaloneFlag(value int64, where string) (bool, error) {
	switch value {
	case 0:
		return false, nil
	case 1:
		return true, nil
	}
	return false, fmt.Errorf("%s has enabled = %d; the import copies only 0 or 1", where, value)
}

func importSources(ctx context.Context, source *sql.DB, tx *sql.Tx) (copied int, lastID int64, err error) {
	rows, err := source.QueryContext(ctx, `
		SELECT id, name, kind, local_path, repo_url, branch, subpath, trust_tier, license,
			last_synced_at, last_commit, last_skill_count, enabled, created_at, updated_at
		FROM sources ORDER BY id`)
	if err != nil {
		return 0, 0, fmt.Errorf("reading sources: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			id, enabled, createdAt, updatedAt               int64
			name, kind, localPath, repoURL, branch, subpath string
			trustTier                                       string
			license, lastCommit                             sql.NullString
			lastSyncedAt, lastSkillCount                    sql.NullInt64
		)
		if err := rows.Scan(&id, &name, &kind, &localPath, &repoURL, &branch, &subpath, &trustTier, &license,
			&lastSyncedAt, &lastCommit, &lastSkillCount, &enabled, &createdAt, &updatedAt); err != nil {
			return 0, 0, fmt.Errorf("reading sources: %w", err)
		}
		if !lastSkillCount.Valid {
			return 0, 0, fmt.Errorf("source %d (%s) has no last_skill_count; the standalone store could not read such a row, so the import will not guess one", id, name)
		}
		isEnabled, err := standaloneFlag(enabled, fmt.Sprintf("source %d (%s)", id, name))
		if err != nil {
			return 0, 0, err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO sources (id, name, kind, local_path, repo_url, branch, subpath, trust_tier, license,
				last_synced_at, last_commit, last_skill_count, enabled, created_at, updated_at)
			OVERRIDING SYSTEM VALUE
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)`,
			id, name, kind, localPath, repoURL, branch, subpath, trustTier, license,
			lastSyncedAt, lastCommit, lastSkillCount.Int64, isEnabled, createdAt, updatedAt); err != nil {
			return 0, 0, fmt.Errorf("copying source %d (%s): %w", id, name, err)
		}
		copied++
		lastID = id
	}
	if err := rows.Err(); err != nil {
		return 0, 0, fmt.Errorf("reading sources: %w", err)
	}
	return copied, lastID, nil
}

func importSkills(ctx context.Context, source *sql.DB, tx *sql.Tx) (copied int, lastID int64, err error) {
	rows, err := source.QueryContext(ctx, `
		SELECT id, source_id, name, display_name, description, skill_md, source_path,
			source_commit, content_hash, file_count, tags, enabled, created_at, updated_at
		FROM skills ORDER BY id`)
	if err != nil {
		return 0, 0, fmt.Errorf("reading skills: %w", err)
	}
	defer rows.Close()
	insert, err := tx.PrepareContext(ctx, `
		INSERT INTO skills (id, source_id, name, display_name, description, skill_md, source_path,
			source_commit, content_hash, file_count, tags, enabled, created_at, updated_at)
		OVERRIDING SYSTEM VALUE
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)`)
	if err != nil {
		return 0, 0, fmt.Errorf("preparing the copy of skills: %w", err)
	}
	defer insert.Close()
	for rows.Next() {
		var (
			id, sourceID, fileCount, enabled, createdAt, updatedAt int64
			name, sourcePath, contentHash                          string
			skillMD                                                []byte
			displayName, description, sourceCommit, tags           sql.NullString
		)
		if err := rows.Scan(&id, &sourceID, &name, &displayName, &description, &skillMD, &sourcePath,
			&sourceCommit, &contentHash, &fileCount, &tags, &enabled, &createdAt, &updatedAt); err != nil {
			return 0, 0, fmt.Errorf("reading skills: %w", err)
		}
		isEnabled, err := standaloneFlag(enabled, fmt.Sprintf("skill %d (%s)", id, name))
		if err != nil {
			return 0, 0, err
		}
		if _, err := insert.ExecContext(ctx, id, sourceID, name, displayName, description, skillMD, sourcePath,
			sourceCommit, contentHash, fileCount, tags, isEnabled, createdAt, updatedAt); err != nil {
			return 0, 0, fmt.Errorf("copying skill %d (%s) of source %d: %w", id, name, sourceID, err)
		}
		copied++
		lastID = id
	}
	if err := rows.Err(); err != nil {
		return 0, 0, fmt.Errorf("reading skills: %w", err)
	}
	return copied, lastID, nil
}

func importInstallations(ctx context.Context, source *sql.DB, tx *sql.Tx) (copied int, err error) {
	rows, err := source.QueryContext(ctx, `SELECT skill_id, target_path, installed_at FROM installations ORDER BY skill_id`)
	if err != nil {
		return 0, fmt.Errorf("reading installations: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var skillID, installedAt int64
		var targetPath string
		if err := rows.Scan(&skillID, &targetPath, &installedAt); err != nil {
			return 0, fmt.Errorf("reading installations: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO installations (skill_id, target_path, installed_at) VALUES ($1, $2, $3)`,
			skillID, targetPath, installedAt); err != nil {
			return 0, fmt.Errorf("copying the installation of skill %d at %s: %w", skillID, targetPath, err)
		}
		copied++
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("reading installations: %w", err)
	}
	return copied, nil
}

// importSeedFileVersions copies the version history one row at a time: each
// row's bytes are read, written and dropped before the next is read.
func importSeedFileVersions(ctx context.Context, source *sql.DB, tx *sql.Tx) (copied int, totalBytes, lastID int64, err error) {
	rows, err := source.QueryContext(ctx, `
		SELECT id, skill_id, relpath, sha256, size, content_bytes, source, machine_id, note, created_at
		FROM seed_file_versions ORDER BY id`)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("reading seed_file_versions: %w", err)
	}
	defer rows.Close()
	insert, err := tx.PrepareContext(ctx, `
		INSERT INTO seed_file_versions (id, skill_id, relpath, sha256, size, content_bytes, source, machine_id, note, created_at)
		OVERRIDING SYSTEM VALUE
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("preparing the copy of seed_file_versions: %w", err)
	}
	defer insert.Close()
	for rows.Next() {
		var (
			id, skillID, size, createdAt int64
			relpath, sha, versionSource  string
			content                      []byte
			machineID, note              sql.NullString
		)
		if err := rows.Scan(&id, &skillID, &relpath, &sha, &size, &content, &versionSource, &machineID, &note, &createdAt); err != nil {
			return 0, 0, 0, fmt.Errorf("reading seed_file_versions: %w", err)
		}
		if content == nil {
			content = []byte{}
		}
		if int64(len(content)) != size {
			return 0, 0, 0, fmt.Errorf("seed file version %d (skill %d, %s) records size %d but holds %d bytes; the import copies only a version whose bytes match it", id, skillID, relpath, size, len(content))
		}
		if _, err := insert.ExecContext(ctx, id, skillID, relpath, sha, size, content, versionSource, machineID, note, createdAt); err != nil {
			return 0, 0, 0, fmt.Errorf("copying seed file version %d of skill %d (%s): %w", id, skillID, relpath, err)
		}
		copied++
		totalBytes += size
		lastID = id
	}
	if err := rows.Err(); err != nil {
		return 0, 0, 0, fmt.Errorf("reading seed_file_versions: %w", err)
	}
	return copied, totalBytes, lastID, nil
}

func importMachineSeedProfiles(ctx context.Context, source *sql.DB, tx *sql.Tx) (copied int, err error) {
	rows, err := source.QueryContext(ctx, `SELECT machine_id, enabled, created_at, updated_at FROM machine_seed_profiles ORDER BY machine_id`)
	if err != nil {
		return 0, fmt.Errorf("reading machine_seed_profiles: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var machineID string
		var enabled, createdAt, updatedAt int64
		if err := rows.Scan(&machineID, &enabled, &createdAt, &updatedAt); err != nil {
			return 0, fmt.Errorf("reading machine_seed_profiles: %w", err)
		}
		isEnabled, err := standaloneFlag(enabled, fmt.Sprintf("the seed profile of machine %s", machineID))
		if err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO machine_seed_profiles (machine_id, enabled, created_at, updated_at) VALUES ($1, $2, $3, $4)`,
			machineID, isEnabled, createdAt, updatedAt); err != nil {
			return 0, fmt.Errorf("copying the seed profile of machine %s: %w", machineID, err)
		}
		copied++
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("reading machine_seed_profiles: %w", err)
	}
	return copied, nil
}

func importMachineSeedStates(ctx context.Context, source *sql.DB, tx *sql.Tx) (copied int, err error) {
	rows, err := source.QueryContext(ctx, `
		SELECT machine_id, skill_id, relpath, observed_sha, observed_at, last_pushed_version_id, last_pushed_at
		FROM machine_seed_state ORDER BY machine_id, skill_id, relpath`)
	if err != nil {
		return 0, fmt.Errorf("reading machine_seed_state: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			machineID, relpath                            string
			skillID                                       int64
			observedSHA                                   sql.NullString
			observedAt, lastPushedVersionID, lastPushedAt sql.NullInt64
		)
		if err := rows.Scan(&machineID, &skillID, &relpath, &observedSHA, &observedAt, &lastPushedVersionID, &lastPushedAt); err != nil {
			return 0, fmt.Errorf("reading machine_seed_state: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO machine_seed_state (machine_id, skill_id, relpath, observed_sha, observed_at, last_pushed_version_id, last_pushed_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			machineID, skillID, relpath, observedSHA, observedAt, lastPushedVersionID, lastPushedAt); err != nil {
			return 0, fmt.Errorf("copying the seed state of machine %s for skill %d (%s): %w", machineID, skillID, relpath, err)
		}
		copied++
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("reading machine_seed_state: %w", err)
	}
	return copied, nil
}
