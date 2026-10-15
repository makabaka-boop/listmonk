-- name: record-bounce
-- Insert a bounce if it isn't a re-delivered duplicate and return the resolved
-- subscriber, the post-insert bounce count for the type and whether a row was
-- actually inserted. The caller applies the configured bounce action in the
-- same transaction only when `inserted` is true.
WITH sub AS (
    SELECT id, status FROM subscribers WHERE CASE WHEN $1 != '' THEN uuid = $1::UUID ELSE email = $2 END
),
camp AS (
    SELECT id FROM campaigns WHERE $3 != '' AND uuid = $3::UUID
),
-- If an event_id ($9) is present, check whether the same event from the same
-- source has already been recorded for the subscriber. Re-delivered provider
-- notifications (retries, service restarts, duplicates within a batch) are
-- recorded and counted only once. An empty event_id always passes this check
-- and preserves the legacy behaviour.
dup AS (
    SELECT TRUE AS seen
    FROM bounces
    WHERE $9 <> ''
        AND source = $5
        AND event_id = $9
        AND subscriber_id = (SELECT id FROM sub)
),
-- Add a +1 to include the current insertion that is happening. For duplicate
-- events no row is inserted, so the +1 and the count are skipped.
num AS (
    SELECT COUNT(*) + 1 AS num FROM bounces
    WHERE NOT EXISTS (SELECT 1 FROM dup)
        AND subscriber_id = (SELECT id FROM sub) AND type = $4
),
-- Record the bounce if the subscriber exists, is not already blocklisted and
-- the threshold is not yet exceeded. The unique partial index on
-- (source, event_id, subscriber_id) together with ON CONFLICT guards against
-- concurrent re-deliveries that both race past the dup check. Only a row
-- actually returned here (and hence a non-duplicate event) can trigger the
-- follow-up unsubscribe / blocklist / delete action.
ins AS (
    INSERT INTO bounces (subscriber_id, campaign_id, type, source, meta, created_at, event_id)
    SELECT (SELECT id FROM sub), (SELECT id FROM camp), $4, $5, $6, $7, $9
    WHERE NOT EXISTS (SELECT 1 FROM dup)
        AND (SELECT id FROM sub) IS NOT NULL
        AND (SELECT status FROM sub) <> 'blocklisted'
        AND (SELECT num FROM num) <= $8
    ON CONFLICT (source, event_id, subscriber_id) WHERE event_id <> '' DO NOTHING
    RETURNING subscriber_id
)
SELECT COALESCE((SELECT id FROM sub), 0) AS subscriber_id,
    (SELECT num FROM num) AS num,
    EXISTS (SELECT 1 FROM ins) AS inserted;

-- name: blocklist-bounced-subscriber
UPDATE subscribers SET status='blocklisted'
    WHERE id = $1 AND status <> 'blocklisted';

-- name: unsubscribe-bounced-subscriber
UPDATE subscriber_lists SET status='unsubscribed'
    WHERE subscriber_id = $1
        AND $1 IN (SELECT id FROM subscribers WHERE status <> 'blocklisted');

-- name: delete-bounced-subscriber
DELETE FROM subscribers WHERE id = $1;

-- name: query-bounces
SELECT COUNT(*) OVER () AS total,
    bounces.id,
    bounces.type,
    bounces.source,
    bounces.meta,
    bounces.created_at,
    bounces.subscriber_id,
    subscribers.uuid AS subscriber_uuid,
    subscribers.email AS email,
    subscribers.name AS subscriber_name,
    subscribers.status as subscriber_status,
    (
        CASE WHEN bounces.campaign_id IS NOT NULL
        THEN JSON_BUILD_OBJECT('id', bounces.campaign_id, 'name', campaigns.name)
        ELSE NULL END
    ) AS campaign
FROM bounces
LEFT JOIN subscribers ON (subscribers.id = bounces.subscriber_id)
LEFT JOIN campaigns ON (campaigns.id = bounces.campaign_id)
WHERE ($1 = 0 OR bounces.id = $1)
    AND ($2 = 0 OR bounces.campaign_id = $2)
    AND ($3 = 0 OR bounces.subscriber_id = $3)
    AND ($4 = '' OR bounces.source = $4)
    AND ($5 = '' OR bounces.type = $5::bounce_type)
ORDER BY %order% OFFSET $6 LIMIT (CASE WHEN $7 < 1 THEN NULL ELSE $7 END);

-- name: delete-bounces
DELETE FROM bounces WHERE CASE WHEN $2 THEN
    ($3 = 0 OR campaign_id = $3)
    AND ($4 = '' OR source = $4)
    AND ($5 = '' OR type = $5::bounce_type)
ELSE id = ANY($1) END;

-- name: delete-bounces-by-subscriber
WITH sub AS (
    SELECT id FROM subscribers WHERE CASE WHEN $1 > 0 THEN id = $1 ELSE uuid = $2 END
)
DELETE FROM bounces WHERE subscriber_id = (SELECT id FROM sub);

-- name: blocklist-bounced-subscribers
WITH subs AS (
    SELECT subscriber_id FROM bounces
    WHERE CASE WHEN CARDINALITY($1::INT[]) > 0 THEN id = ANY($1)
    ELSE ($2 = 0 OR campaign_id = $2)
        AND ($3 = '' OR source = $3)
        AND ($4 = '' OR type = $4::bounce_type)
    END
),
b AS (
    UPDATE subscribers SET status='blocklisted', updated_at=NOW()
    WHERE id = ANY(SELECT subscriber_id FROM subs)
)
UPDATE subscriber_lists SET status='unsubscribed', updated_at=NOW()
    WHERE subscriber_id = ANY(SELECT subscriber_id FROM subs);

