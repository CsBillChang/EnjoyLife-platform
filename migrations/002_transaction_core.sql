CREATE TABLE IF NOT EXISTS coupon_activities (
    id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    shop_id BIGINT UNSIGNED NOT NULL,
    title VARCHAR(128) NOT NULL,
    price_cents BIGINT UNSIGNED NOT NULL,
    currency CHAR(3) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT 'CNY',
    starts_at DATETIME(6) NOT NULL,
    ends_at DATETIME(6) NOT NULL,
    voucher_valid_until DATETIME(6) NOT NULL,
    total_stock BIGINT UNSIGNED NOT NULL,
    available_stock BIGINT UNSIGNED NOT NULL,
    admission_state ENUM('OPEN', 'FROZEN', 'BUILDING', 'VERIFIED') NOT NULL DEFAULT 'OPEN',
    generation BIGINT UNSIGNED NOT NULL DEFAULT 1,
    created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
    PRIMARY KEY (id),
    KEY idx_coupon_activities_shop_window (shop_id, starts_at, ends_at),
    CONSTRAINT fk_coupon_activities_shop FOREIGN KEY (shop_id) REFERENCES shops(id),
    CONSTRAINT chk_coupon_activity_window CHECK (starts_at < ends_at),
    CONSTRAINT chk_coupon_activity_validity CHECK (ends_at <= voucher_valid_until),
    CONSTRAINT chk_coupon_activity_stock CHECK (available_stock <= total_stock)
) ENGINE=InnoDB;

CREATE TABLE IF NOT EXISTS orders (
    id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    request_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    idempotency_key VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    user_id BIGINT UNSIGNED NOT NULL,
    activity_id BIGINT UNSIGNED NOT NULL,
    shop_id BIGINT UNSIGNED NOT NULL,
    price_cents BIGINT UNSIGNED NOT NULL,
    currency CHAR(3) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    status ENUM('PENDING_PAYMENT', 'PAID', 'CLOSED', 'REFUNDING', 'REFUNDED', 'REDEEMED') NOT NULL,
    payment_deadline DATETIME(6) NOT NULL,
    created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
    PRIMARY KEY (id),
    UNIQUE KEY uk_orders_request_id (request_id),
    UNIQUE KEY uk_orders_user_activity (user_id, activity_id),
    UNIQUE KEY uk_orders_user_idempotency (user_id, idempotency_key),
    KEY idx_orders_status_deadline (status, payment_deadline),
    CONSTRAINT fk_orders_user FOREIGN KEY (user_id) REFERENCES users(id),
    CONSTRAINT fk_orders_activity FOREIGN KEY (activity_id) REFERENCES coupon_activities(id),
    CONSTRAINT fk_orders_shop FOREIGN KEY (shop_id) REFERENCES shops(id)
) ENGINE=InnoDB;

CREATE TABLE IF NOT EXISTS inventory_movements (
    id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    order_id BIGINT UNSIGNED NOT NULL,
    activity_id BIGINT UNSIGNED NOT NULL,
    kind ENUM('DEDUCT', 'RELEASE') NOT NULL,
    delta INT NOT NULL,
    created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    PRIMARY KEY (id),
    UNIQUE KEY uk_inventory_movement_order_kind (order_id, kind),
    KEY idx_inventory_movement_activity (activity_id, id),
    CONSTRAINT fk_inventory_movement_order FOREIGN KEY (order_id) REFERENCES orders(id),
    CONSTRAINT fk_inventory_movement_activity FOREIGN KEY (activity_id) REFERENCES coupon_activities(id),
    CONSTRAINT chk_inventory_movement_delta CHECK (
        (kind = 'DEDUCT' AND delta = -1) OR (kind = 'RELEASE' AND delta = 1)
    )
) ENGINE=InnoDB;

CREATE TABLE IF NOT EXISTS payments (
    id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    order_id BIGINT UNSIGNED NOT NULL,
    attempt_id VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    provider VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT 'LOCAL',
    provider_transaction_id VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    amount_cents BIGINT UNSIGNED NOT NULL,
    currency CHAR(3) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    status ENUM('SUCCEEDED') NOT NULL,
    settlement ENUM('APPLIED', 'LATE_PAYMENT', 'EXTRA_PAYMENT') NOT NULL,
    created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    PRIMARY KEY (id),
    UNIQUE KEY uk_payments_attempt (attempt_id),
    UNIQUE KEY uk_payments_provider_transaction (provider, provider_transaction_id),
    KEY idx_payments_order (order_id, id),
    CONSTRAINT fk_payments_order FOREIGN KEY (order_id) REFERENCES orders(id)
) ENGINE=InnoDB;

CREATE TABLE IF NOT EXISTS vouchers (
    id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    order_id BIGINT UNSIGNED NOT NULL,
    code_hash BINARY(32) NOT NULL,
    status ENUM('ACTIVE', 'REDEEMED', 'INVALID') NOT NULL,
    expires_at DATETIME(6) NOT NULL,
    created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
    PRIMARY KEY (id),
    UNIQUE KEY uk_vouchers_order (order_id),
    UNIQUE KEY uk_vouchers_code_hash (code_hash),
    KEY idx_vouchers_status_expires (status, expires_at),
    CONSTRAINT fk_vouchers_order FOREIGN KEY (order_id) REFERENCES orders(id)
) ENGINE=InnoDB;

CREATE TABLE IF NOT EXISTS redemptions (
    id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    voucher_id BIGINT UNSIGNED NOT NULL,
    order_id BIGINT UNSIGNED NOT NULL,
    merchant_user_id BIGINT UNSIGNED NOT NULL,
    redeemed_at DATETIME(6) NOT NULL,
    PRIMARY KEY (id),
    UNIQUE KEY uk_redemptions_voucher (voucher_id),
    CONSTRAINT fk_redemptions_voucher FOREIGN KEY (voucher_id) REFERENCES vouchers(id),
    CONSTRAINT fk_redemptions_order FOREIGN KEY (order_id) REFERENCES orders(id),
    CONSTRAINT fk_redemptions_merchant FOREIGN KEY (merchant_user_id) REFERENCES users(id)
) ENGINE=InnoDB;

CREATE TABLE IF NOT EXISTS refunds (
    id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    order_id BIGINT UNSIGNED NOT NULL,
    payment_id BIGINT UNSIGNED NULL,
    refund_request_id VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    purpose ENUM('NORMAL', 'LATE_PAYMENT', 'EXTRA_PAYMENT') NOT NULL,
    amount_cents BIGINT UNSIGNED NOT NULL,
    currency CHAR(3) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    status ENUM('PENDING', 'UNKNOWN', 'SUCCEEDED', 'FAILED_FINAL') NOT NULL,
    created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
    PRIMARY KEY (id),
    UNIQUE KEY uk_refunds_request (refund_request_id),
    KEY idx_refunds_order_purpose (order_id, purpose, id),
    CONSTRAINT fk_refunds_order FOREIGN KEY (order_id) REFERENCES orders(id),
    CONSTRAINT fk_refunds_payment FOREIGN KEY (payment_id) REFERENCES payments(id)
) ENGINE=InnoDB;

