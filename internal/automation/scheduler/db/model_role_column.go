package db

import "fmt"

// migrateModelToModelRole renames jobs.model to jobs.model_role.
//
// jobs.model was stored, returned by GET and taken by PATCH, and never reached
// a session: the agent executor did not read it, so every agent job ran on the
// bridge's per-harness default whatever the column said. The column now holds
// a model-store role that the executor sends, so it carries the name of what
// it holds. A value left over in it is not checked here; the executor refuses
// a role model-store does not know when the job next runs, so a stale value
// fails that run with model-store's answer rather than being dropped.
//
// A database made before the rename has model and not model_role; one made
// after has model_role only. A database with neither is a jobs table older
// than the model column, and gets model_role added.
func (s *Store) migrateModelToModelRole() error {
	hasModel, err := s.jobsColumnExists("model")
	if err != nil {
		return err
	}
	hasModelRole, err := s.jobsColumnExists("model_role")
	if err != nil {
		return err
	}
	switch {
	case hasModelRole && hasModel:
		return fmt.Errorf("migrate jobs.model to jobs.model_role: the table has both columns; drop the one that is not in use by hand")
	case hasModelRole:
		return nil
	case hasModel:
		if _, err := s.db.Exec(`ALTER TABLE jobs RENAME COLUMN model TO model_role`); err != nil {
			return fmt.Errorf("migrate jobs.model to jobs.model_role: %w", err)
		}
		return nil
	default:
		if _, err := s.db.Exec(`ALTER TABLE jobs ADD COLUMN model_role TEXT NOT NULL DEFAULT ''`); err != nil {
			return fmt.Errorf("add jobs.model_role: %w", err)
		}
		return nil
	}
}

func (s *Store) jobsColumnExists(column string) (bool, error) {
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('jobs') WHERE name = ?`, column).Scan(&count); err != nil {
		return false, fmt.Errorf("read the jobs table's columns: %w", err)
	}
	return count > 0, nil
}
