ALTER TABLE outbox_events
    MODIFY COLUMN status ENUM('PENDING', 'SENT', 'DEAD') NOT NULL DEFAULT 'PENDING',
    ADD COLUMN lease_owner CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NULL AFTER lease_until,
    ADD COLUMN dead_at DATETIME(6) NULL AFTER sent_at;

UPDATE outbox_events
SET status = 'DEAD', dead_at = CURRENT_TIMESTAMP(6), lease_until = NULL,
    lease_owner = NULL, last_error = 'retry limit reached before delivery fencing migration'
WHERE status = 'PENDING' AND attempts >= 1000;
