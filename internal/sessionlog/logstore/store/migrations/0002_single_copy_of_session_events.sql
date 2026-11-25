-- The session event log as the server's only copy of session events.
--
-- message_id, harness_message_id, tool_use_id and tool_name are the event's
-- own values, kept beside its body so the readers that map a session's message
-- ids, and the reaper's look for a pending wakeup, read columns rather than
-- parse every body. The writer fills them; the import fills them from each
-- body. An event that names none holds ''.
ALTER TABLE events ADD COLUMN message_id text NOT NULL DEFAULT '';
ALTER TABLE events ADD COLUMN harness_message_id text NOT NULL DEFAULT '';
ALTER TABLE events ADD COLUMN tool_use_id text NOT NULL DEFAULT '';
ALTER TABLE events ADD COLUMN tool_name text NOT NULL DEFAULT '';

-- idempotency_key names one write, so the same write sent again stores
-- nothing and answers the row it stored the first time. A runner's numbered
-- output entry and a session status carry one; every other event holds NULL.
ALTER TABLE events ADD COLUMN idempotency_key text;
CREATE UNIQUE INDEX events_idempotency_key ON events (idempotency_key) WHERE idempotency_key IS NOT NULL;

-- One row, written the first time the server starts with this module as its
-- only copy of session events: the first event id it numbered then, above the
-- highest id the bridge database's events table had handed out, so no cursor
-- or status a client holds from that table names an event stored here.
CREATE TABLE single_copy_numbering (
    only_row                         boolean PRIMARY KEY DEFAULT true CHECK (only_row),
    first_event_id                   bigint NOT NULL,
    bridge_database_highest_event_id bigint NOT NULL,
    raised_at                        timestamptz NOT NULL DEFAULT now()
);

-- lz4 rather than pglz for the bodies and the stored reading pages: it
-- compresses and decompresses several times faster, which the event write
-- and every read of a large tool result pay, for a little more disk. It
-- applies to values written from here on, the import's included.
ALTER TABLE events ALTER COLUMN data SET COMPRESSION lz4;
ALTER TABLE turns ALTER COLUMN turn_json SET COMPRESSION lz4;
ALTER TABLE turn_entries ALTER COLUMN entry_json SET COMPRESSION lz4;
