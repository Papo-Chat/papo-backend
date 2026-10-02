-- Automatic away used to be persisted in users.status by older frontends.
-- There is no source marker to distinguish those rows from a manually chosen
-- away, so clear legacy away once. From this migration onward automatic away
-- is WebSocket presence state only; manually chosen away/busy can still be
-- persisted through PUT /users/:id/status.
UPDATE users
SET status = NULL
WHERE status = 'away';
