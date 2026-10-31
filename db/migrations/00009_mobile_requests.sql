-- +goose Up
-- Request ids the mobile app sends with recording actions, so a retried
-- request isn't recorded twice (legacy logs/request_ids.txt). An id is
-- "<unix time>_<random>" and must be from today, so rows older than a
-- couple of days are dropped as new ones arrive.
CREATE TABLE mobile_request_ids (
    request_id text PRIMARY KEY,
    seen_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX mobile_request_ids_seen_at ON mobile_request_ids (seen_at);

-- +goose Down
DROP TABLE mobile_request_ids;
