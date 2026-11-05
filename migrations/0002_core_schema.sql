-- Core schema: loyalty programs, restaurants, customers, webhook inbox,
-- orders and the loyalty ledger.
--
-- Conventions:
--   * Money is bigint minor units (kuruş for TRY), never floating point.
--   * Statuses are text + CHECK rather than ENUM types: adding a value is a
--     plain constraint change, without ALTER TYPE restrictions.
--   * External identifiers are scoped by (provider, tenant_ref). Providers
--     without tenant information use one consistent provider-level tenant_ref.

-- ---------------------------------------------------------------------------
-- Loyalty programs
-- ---------------------------------------------------------------------------
-- The earn rule lives on the program. Restaurants map to programs, so
-- "shared across restaurants" vs "per restaurant" is configuration, not code:
-- many restaurants -> one program, or one program per restaurant.
CREATE TABLE loyalty_programs (
    id              bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name            text        NOT NULL CHECK (name <> ''),
    earn_currency   char(3)     NOT NULL CHECK (earn_currency ~ '^[A-Z]{3}$'),
    earn_unit_minor bigint      NOT NULL CHECK (earn_unit_minor > 0), -- 1000 = 10.00 TRY
    points_per_unit bigint      NOT NULL CHECK (points_per_unit > 0),
    created_at      timestamptz NOT NULL DEFAULT now()
);

-- ---------------------------------------------------------------------------
-- Restaurants
-- ---------------------------------------------------------------------------
CREATE TABLE restaurants (
    id                     bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    provider               text        NOT NULL CHECK (provider <> ''),
    tenant_ref             text        NOT NULL CHECK (tenant_ref <> ''),
    external_restaurant_id text        NOT NULL CHECK (external_restaurant_id <> ''),
    loyalty_program_id     bigint      NOT NULL REFERENCES loyalty_programs (id),
    created_at             timestamptz NOT NULL DEFAULT now(),
    UNIQUE (provider, tenant_ref, external_restaurant_id)
);

-- ---------------------------------------------------------------------------
-- Customers
-- ---------------------------------------------------------------------------
-- Internal customer, separate from provider identities, so several provider
-- identities can later be linked to one customer without a schema change.
CREATE TABLE customers (
    id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE customer_identities (
    provider             text        NOT NULL CHECK (provider <> ''),
    tenant_ref           text        NOT NULL CHECK (tenant_ref <> ''),
    external_customer_id text        NOT NULL CHECK (external_customer_id <> ''),
    customer_id          bigint      NOT NULL REFERENCES customers (id),
    created_at           timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (provider, tenant_ref, external_customer_id)
);
CREATE INDEX customer_identities_customer_idx ON customer_identities (customer_id);

-- ---------------------------------------------------------------------------
-- Webhook inbox
-- ---------------------------------------------------------------------------
-- Verified events are stored as 'pending' and processed asynchronously.
-- There is no 'processing' status: the worker locks the row with
-- FOR UPDATE SKIP LOCKED inside the same transaction as the business writes,
-- so a crash rolls back and the event is simply pending again.
--
-- Failure handling:
--   transient -> stays 'pending', attempts/next_attempt_at advanced (backoff)
--   permanent -> 'failed', stays failed until explicitly resolved
CREATE TABLE webhook_events (
    id                bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    provider          text        NOT NULL CHECK (provider <> ''),
    tenant_ref        text        NOT NULL CHECK (tenant_ref <> ''),
    external_event_id text        NOT NULL CHECK (external_event_id <> ''),
    event_type        text        NOT NULL CHECK (event_type <> ''),
    payload           jsonb       NOT NULL,
    status            text        NOT NULL DEFAULT 'pending'
                      CHECK (status IN ('pending', 'processed', 'failed')),
    attempts          integer     NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    next_attempt_at   timestamptz NOT NULL DEFAULT now(),
    last_error        text,
    last_error_kind   text        CHECK (last_error_kind IN ('transient', 'permanent')),
    received_at       timestamptz NOT NULL DEFAULT now(),
    processed_at      timestamptz,

    -- Duplicate deliveries hit this constraint and become no-ops.
    UNIQUE (provider, tenant_ref, external_event_id),

    CONSTRAINT webhook_events_processed_at_matches_status
        CHECK ((status = 'processed') = (processed_at IS NOT NULL)),
    CONSTRAINT webhook_events_error_has_kind
        CHECK ((last_error IS NULL) = (last_error_kind IS NULL)),
    CONSTRAINT webhook_events_failed_has_error
        CHECK (status <> 'failed' OR last_error_kind IS NOT NULL),
    -- A permanent error is never retried, so it cannot sit in 'pending'.
    CONSTRAINT webhook_events_permanent_is_failed
        CHECK (last_error_kind IS DISTINCT FROM 'permanent' OR status = 'failed')
);
-- Worker poll: only due pending events, oldest first.
CREATE INDEX webhook_events_due_idx ON webhook_events (next_attempt_at, id)
    WHERE status = 'pending';

-- ---------------------------------------------------------------------------
-- Orders
-- ---------------------------------------------------------------------------
-- loyalty_status precedence (decided by the processor):
--   no_customer > unsupported_currency > below_minimum > awarded
CREATE TABLE orders (
    id                    bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    restaurant_id         bigint      NOT NULL REFERENCES restaurants (id),
    -- Snapshot of the restaurant's program when the order was processed, so a
    -- later program change does not rewrite history. Checked by trigger below.
    loyalty_program_id    bigint      NOT NULL REFERENCES loyalty_programs (id),
    external_order_id     text        NOT NULL CHECK (external_order_id <> ''),
    customer_id           bigint      REFERENCES customers (id), -- NULL = guest order
    -- Subtotal after discounts, excluding tax and tips; mapped by the provider adapter.
    eligible_amount_minor bigint      NOT NULL CHECK (eligible_amount_minor >= 0),
    currency              char(3)     NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    completed_at          timestamptz NOT NULL,
    loyalty_status        text        NOT NULL CHECK (loyalty_status IN
                          ('awarded', 'no_customer', 'below_minimum', 'unsupported_currency')),
    source_event_id       bigint      NOT NULL REFERENCES webhook_events (id),
    created_at            timestamptz NOT NULL DEFAULT now(),

    -- POS order numbers are typically unique per store, not per provider.
    -- The same order delivered under a different event ID cannot create a second row.
    UNIQUE (restaurant_id, external_order_id),

    -- Target for the ledger's composite FK (program and customer must match the order).
    UNIQUE (id, loyalty_program_id, customer_id),

    CONSTRAINT orders_no_customer_matches_status
        CHECK ((customer_id IS NULL) = (loyalty_status = 'no_customer'))
);
CREATE INDEX orders_customer_history_idx ON orders (customer_id, completed_at DESC, id DESC)
    WHERE customer_id IS NOT NULL;
CREATE INDEX orders_source_event_idx ON orders (source_event_id);

CREATE FUNCTION orders_program_matches_restaurant() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    restaurant_program bigint;
BEGIN
    SELECT loyalty_program_id INTO restaurant_program
    FROM restaurants WHERE id = NEW.restaurant_id;

    IF restaurant_program IS DISTINCT FROM NEW.loyalty_program_id THEN
        RAISE EXCEPTION 'order loyalty_program_id % does not match restaurant % program %',
            NEW.loyalty_program_id, NEW.restaurant_id, restaurant_program
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END $$;

CREATE TRIGGER orders_program_matches_restaurant
    BEFORE INSERT ON orders
    FOR EACH ROW EXECUTE FUNCTION orders_program_matches_restaurant();

-- ---------------------------------------------------------------------------
-- Loyalty accounts (read-optimized balance)
-- ---------------------------------------------------------------------------
-- The ledger is the source of truth; balance is updated in the same
-- transaction as each ledger insert.
CREATE TABLE loyalty_accounts (
    loyalty_program_id bigint      NOT NULL REFERENCES loyalty_programs (id),
    customer_id        bigint      NOT NULL REFERENCES customers (id),
    balance            bigint      NOT NULL DEFAULT 0 CHECK (balance >= 0),
    updated_at         timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (loyalty_program_id, customer_id)
);
CREATE INDEX loyalty_accounts_customer_idx ON loyalty_accounts (customer_id);

-- ---------------------------------------------------------------------------
-- Loyalty ledger (append-only)
-- ---------------------------------------------------------------------------
CREATE TABLE loyalty_transactions (
    id                 bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    loyalty_program_id bigint      NOT NULL,
    customer_id        bigint      NOT NULL,
    type               text        NOT NULL CHECK (type IN ('earn')),
    points             bigint      NOT NULL CHECK (points > 0),
    order_id           bigint,
    -- Earn rule snapshot: history stays auditable when a program's rule changes.
    earn_unit_minor    bigint      CHECK (earn_unit_minor > 0),
    points_per_unit    bigint      CHECK (points_per_unit > 0),
    created_at         timestamptz NOT NULL DEFAULT now(),

    FOREIGN KEY (loyalty_program_id, customer_id)
        REFERENCES loyalty_accounts (loyalty_program_id, customer_id),
    -- The ledger row must use the same program and customer as its order.
    FOREIGN KEY (order_id, loyalty_program_id, customer_id)
        REFERENCES orders (id, loyalty_program_id, customer_id),

    CONSTRAINT loyalty_transactions_earn_has_order_and_rule
        CHECK (type <> 'earn'
               OR (order_id IS NOT NULL AND earn_unit_minor IS NOT NULL AND points_per_unit IS NOT NULL))
);
-- One earn per order. Partial, so a future 'reversal' row for the same order
-- does not require a constraint change.
CREATE UNIQUE INDEX loyalty_transactions_one_earn_per_order
    ON loyalty_transactions (order_id) WHERE type = 'earn';
CREATE INDEX loyalty_transactions_account_idx
    ON loyalty_transactions (loyalty_program_id, customer_id, created_at DESC, id DESC);

CREATE FUNCTION loyalty_transactions_append_only() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'loyalty_transactions is append-only'
        USING ERRCODE = 'restrict_violation';
END $$;

CREATE TRIGGER loyalty_transactions_append_only
    BEFORE UPDATE OR DELETE ON loyalty_transactions
    FOR EACH ROW EXECUTE FUNCTION loyalty_transactions_append_only();

-- TRUNCATE bypasses row-level triggers, so it needs its own statement trigger.
CREATE TRIGGER loyalty_transactions_no_truncate
    BEFORE TRUNCATE ON loyalty_transactions
    FOR EACH STATEMENT EXECUTE FUNCTION loyalty_transactions_append_only();
