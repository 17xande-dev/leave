-- Run with the app stopped; safe to re-run. Both goose histories stay in place.
-- SQLite's legacy queue compares timestamps as whole-second UTC text.
BEGIN IMMEDIATE;

-- Reconcile imported jobs in place. In particular, a job delivered by the
-- module must not become due again simply because its legacy row was frozen.
WITH imported AS (
	SELECT *, max(run_at, coalesce(lease_until, run_at)) AS eligible_at
	FROM outbox_jobs WHERE unique_key GLOB 'legacy:outbox:*'
)
UPDATE outbox AS legacy
SET attempts = job.attempts,
	next_attempt_at = strftime('%Y-%m-%dT%H:%M:%SZ', substr(job.eligible_at, 1, 19),
		CASE WHEN substr(job.eligible_at, 21, 6) = '000000' THEN '+0 seconds' ELSE '+1 second' END),
	last_error = job.last_error,
	done_at = CASE WHEN job.state IN ('done', 'discarded')
		THEN substr(job.finished_at, 1, 19) || 'Z' END,
	failed_at = CASE WHEN job.state = 'dead'
		THEN substr(job.finished_at, 1, 19) || 'Z' END,
	lease_until = NULL,
	claim_token = NULL
FROM imported AS job
WHERE job.unique_key = 'legacy:outbox:' || legacy.id;

-- Jobs created after cutover have no legacy key. Leave allocates fresh legacy
-- IDs so the module's IDs cannot collide with the preserved history. Keep
-- a running job's charged attempt and respect the remainder of its lease.
WITH native AS (
	SELECT *, max(run_at, coalesce(lease_until, run_at)) AS eligible_at
	FROM outbox_jobs
	WHERE (unique_key IS NULL OR unique_key NOT GLOB 'legacy:outbox:*')
		AND state IN ('pending', 'running', 'dead')
)
INSERT INTO outbox (kind, payload, attempts, next_attempt_at, last_error, failed_at, created_at)
SELECT kind, CAST(payload AS TEXT), attempts,
	strftime('%Y-%m-%dT%H:%M:%SZ', substr(eligible_at, 1, 19),
		CASE WHEN substr(eligible_at, 21, 6) = '000000' THEN '+0 seconds' ELSE '+1 second' END),
	last_error,
	CASE WHEN state = 'dead' THEN substr(finished_at, 1, 19) || 'Z' END,
	substr(created_at, 1, 19) || 'Z'
FROM native ORDER BY id;

-- Everything unfinished now lives in `outbox`. Emptying the new queue makes a
-- second run a no-op (no native job is copied twice) and makes a later redeploy
-- re-import only what the old binary left unfinished. Done rows hold no payload.
DELETE FROM outbox_jobs;

COMMIT;
