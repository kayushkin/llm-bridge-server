-- No table here declares a foreign key. The SQLite file this came from had
-- REFERENCES clauses that SQLite never enforced (foreign_keys was off), and it
-- holds rows that break them: enrollments naming machines that are gone and a
-- credential binding of an instance that is gone. Declaring them would refuse
-- those rows and start cascading deletes that have never run. Card
-- 882d977a-dfaf-47da-9857-205285dba647 holds that decision, which needs the
-- orphans cleaned and a person's say-so first.

CREATE TABLE harness_types (
	name       text PRIMARY KEY,
	label      text NOT NULL DEFAULT '',
	emoji      text NOT NULL DEFAULT '',
	image      text NOT NULL DEFAULT '',
	updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE machines (
	id                    text PRIMARY KEY,
	name                  text NOT NULL UNIQUE,
	emoji                 text NOT NULL DEFAULT '',
	hostname              text NOT NULL DEFAULT '',
	os                    text NOT NULL DEFAULT '',
	arch                  text NOT NULL DEFAULT '',
	transport             text NOT NULL DEFAULT 'local',
	ssh_user              text NOT NULL DEFAULT '',
	ssh_key_path          text NOT NULL DEFAULT '',
	ssh_port              integer NOT NULL DEFAULT 22,
	default_working_dir   text NOT NULL DEFAULT '',
	-- The runner-side operating system user, shown only. SQLite called it
	-- user, which Postgres reserves.
	operating_system_user text NOT NULL DEFAULT '',
	notes                 text NOT NULL DEFAULT '',
	computer_use_allowed  boolean NOT NULL DEFAULT false,
	runner_token_hash     text NOT NULL DEFAULT '',
	last_seen_at          timestamptz,
	created_at            timestamptz NOT NULL DEFAULT now(),
	updated_at            timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX machines_transport ON machines (transport);
CREATE INDEX machines_runner_token_hash ON machines (runner_token_hash) WHERE runner_token_hash <> '';

CREATE TABLE runner_enrollments (
	id                  text PRIMARY KEY,
	passphrase_hash     text NOT NULL UNIQUE,
	expires_at          timestamptz NOT NULL,
	used_at             timestamptz,
	consumed_machine_id text,
	created_at          timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE instances (
	id                      text PRIMARY KEY,
	harness_type            text NOT NULL,
	name                    text NOT NULL,
	machine_id              text NOT NULL,
	working_dir             text NOT NULL DEFAULT '',
	max_concurrent_sessions integer NOT NULL DEFAULT 1,
	enabled                 boolean NOT NULL DEFAULT true,
	created_at              timestamptz NOT NULL DEFAULT now(),
	updated_at              timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX instances_harness_type ON instances (harness_type);
CREATE INDEX instances_machine ON instances (machine_id);
CREATE INDEX instances_name ON instances (name);

CREATE TABLE instance_credentials (
	instance_id    text NOT NULL,
	credential_id  text NOT NULL,
	priority       integer NOT NULL DEFAULT 0,
	-- Kept from files that have it; nothing reads it.
	max_concurrent integer NOT NULL DEFAULT 1,
	enabled        boolean NOT NULL DEFAULT true,
	PRIMARY KEY (instance_id, credential_id)
);
CREATE INDEX instance_credentials_credential ON instance_credentials (credential_id);
