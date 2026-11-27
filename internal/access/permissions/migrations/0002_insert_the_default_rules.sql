-- The default rules a new install starts with, ids seed-v1-<slug>, so the
-- editor can tell defaults from rules people added. This file is the one place
-- they are written down. They are inserted once, when the module is first
-- migrated: a seed someone deletes stays deleted and one someone edits stays
-- edited, as the standalone store's own migration did. An import of a
-- standalone database replaces them with the rules that database held.
--
-- Priority convention: 40-50 default denies and the allows that must run
-- before them, 100 default allows, 150 reserved, 200 and up rules people add.
-- Patterns are Go RE2, which has no lookarounds, so "deny curl unless to
-- localhost" is an allow at 40 and a deny at 50.
INSERT INTO rules (id, scope, priority, tool, pattern, outcome, message, enabled, created_at, updated_at)
SELECT seed.id, 'global', seed.priority, seed.tool, seed.pattern, seed.outcome, seed.message, true, stamp.now, stamp.now
FROM (VALUES
    ('seed-v1-allow-Read', 'Read', '', 'allow', 100, 'default seed: read-only tool'),
    ('seed-v1-allow-Glob', 'Glob', '', 'allow', 100, 'default seed: read-only tool'),
    ('seed-v1-allow-Grep', 'Grep', '', 'allow', 100, 'default seed: read-only tool'),
    ('seed-v1-allow-LS', 'LS', '', 'allow', 100, 'default seed: read-only tool'),
    ('seed-v1-allow-TodoWrite', 'TodoWrite', '', 'allow', 100, 'default seed: in-process task list'),
    ('seed-v1-allow-WebSearch', 'WebSearch', '', 'allow', 100, 'default seed: read-only'),
    ('seed-v1-allow-bash-git-readonly', 'Bash', '^git (status|log|diff|branch|show)\b', 'allow', 100, 'default seed: read-only git'),
    ('seed-v1-allow-bash-ls', 'Bash', '^ls\b', 'allow', 100, 'default seed: list files'),
    ('seed-v1-allow-bash-cat', 'Bash', '^cat\b', 'allow', 100, 'default seed: read file'),
    ('seed-v1-allow-bash-rg', 'Bash', '^rg\b', 'allow', 100, 'default seed: ripgrep'),
    ('seed-v1-allow-bash-grep', 'Bash', '^grep\b', 'allow', 100, 'default seed: grep'),
    ('seed-v1-allow-bash-pwd', 'Bash', '^pwd\b', 'allow', 100, 'default seed: pwd'),
    ('seed-v1-allow-bash-which', 'Bash', '^which\b', 'allow', 100, 'default seed: which'),
    ('seed-v1-allow-bash-echo', 'Bash', '^echo\b', 'allow', 100, 'default seed: echo'),
    ('seed-v1-allow-bash-date', 'Bash', '^date\b', 'allow', 100, 'default seed: date'),
    ('seed-v1-deny-bash-rm-rf-abs', 'Bash', '^rm\s+-rf?\s+/', 'deny', 50, 'rm -rf with an absolute path is denied by default — disable this seed if you really need it'),
    ('seed-v1-allow-bash-curl-localhost', 'Bash', '^curl\b.*\b(localhost|127\.0\.0\.1)\b', 'allow', 40, 'default seed: curl to localhost is allowed'),
    ('seed-v1-deny-bash-curl-external', 'Bash', '^curl\b', 'deny', 50, 'curl to non-localhost is denied by default')
) AS seed (id, tool, pattern, outcome, priority, message),
-- The time the store has always stamped: RFC 3339 with an offset.
(SELECT to_char(now() AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"') AS now) AS stamp;
