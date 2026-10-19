ALTER TABLE purchase_requests
    ADD COLUMN reservation_state ENUM('NONE', 'PENDING', 'RESERVED', 'COMMITTED', 'RELEASED')
        NOT NULL DEFAULT 'NONE' AFTER status,
    ADD KEY idx_purchase_requests_reservation_recovery (reservation_state, status, id);
