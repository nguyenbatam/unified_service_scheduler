-- Unified Service Scheduler: the whole schema in one migration.
--
-- Conventions
--   * Enum-like columns are SMALLINT + CHECK (col BETWEEN 1 AND n), never Postgres ENUM or text
--     codes. Codes start at 1 so 0 (Go's zero value) is never a valid stored value. The Go side
--     mirrors each list in internal/domain; external representations (JSON, Kafka, metrics) keep
--     their names.
--   * Times are timestamptz; a dealership's opening hours, lunch and shifts are local `time`.
--   * Appointment windows are half-open: tstzrange(start_at, end_at, '[)').
--   * The double-booking guarantee lives here: GiST exclusion on bay, technician and vehicle over
--     the holding statuses (1 confirmed, 2 checked_in, 3 in_progress).
--   * Constraint and index names are referenced by the code (error mapping, EXPLAIN tests,
--     ON CONFLICT targets); rename them only together with internal/store.

CREATE EXTENSION IF NOT EXISTS btree_gist;
CREATE EXTENSION IF NOT EXISTS pg_trgm;

-- ---------------------------------------------------------------------------------------------
-- Workshop configuration
-- ---------------------------------------------------------------------------------------------

CREATE TABLE dealerships (
    id                             uuid PRIMARY KEY,
    name                           text NOT NULL,
    timezone                       text NOT NULL,
    open_time                      time NOT NULL,
    close_time                     time NOT NULL,
    lunch_start                    time,
    lunch_end                      time,
    -- At most staging_capacity arrivals per staging_window_minutes window (aligned to local
    -- midnight); 0 = unlimited.
    staging_capacity               smallint NOT NULL DEFAULT 0,
    staging_window_minutes         smallint NOT NULL DEFAULT 60,
    -- Booking policy (customers only; staff are bound by none of it).
    customer_lead_minutes          smallint NOT NULL DEFAULT 60,
    customer_change_cutoff_minutes smallint NOT NULL DEFAULT 60,
    booking_horizon_days           smallint NOT NULL DEFAULT 90,
    no_show_grace_minutes          smallint NOT NULL DEFAULT 30,
    express_max_minutes            smallint NOT NULL DEFAULT 60,
    version                        integer NOT NULL DEFAULT 1,
    CONSTRAINT dealerships_hours_order CHECK (close_time > open_time),
    CONSTRAINT dealerships_lunch_check CHECK (
        (lunch_start IS NULL AND lunch_end IS NULL)
        OR (lunch_start IS NOT NULL AND lunch_end IS NOT NULL AND lunch_end > lunch_start)),
    CONSTRAINT dealerships_lunch_within_hours CHECK (
        lunch_start IS NULL OR (lunch_start >= open_time AND lunch_end <= close_time)),
    CONSTRAINT dealerships_staging_capacity_check CHECK (staging_capacity >= 0),
    CONSTRAINT dealerships_staging_window_minutes_check CHECK (staging_window_minutes IN (15, 30, 60)),
    CONSTRAINT dealerships_customer_lead_minutes_check CHECK (customer_lead_minutes BETWEEN 0 AND 10080),
    CONSTRAINT dealerships_customer_change_cutoff_minutes_check CHECK (customer_change_cutoff_minutes BETWEEN 0 AND 10080),
    CONSTRAINT dealerships_booking_horizon_days_check CHECK (booking_horizon_days BETWEEN 1 AND 365),
    CONSTRAINT dealerships_no_show_grace_minutes_check CHECK (no_show_grace_minutes BETWEEN 5 AND 240),
    CONSTRAINT dealerships_express_max_minutes_check CHECK (express_max_minutes BETWEEN 0 AND 600),
    CONSTRAINT dealerships_version_check CHECK (version >= 1)
);
CREATE INDEX dealerships_name_idx ON dealerships (name, id);

-- Per ISO weekday (1 Monday … 7 Sunday) override of the dealership's default hours.
CREATE TABLE dealership_hours (
    dealership_id uuid NOT NULL REFERENCES dealerships (id),
    weekday       smallint NOT NULL,
    closed        boolean NOT NULL DEFAULT false,
    open_time     time,
    close_time    time,
    lunch_start   time,
    lunch_end     time,
    PRIMARY KEY (dealership_id, weekday),
    CONSTRAINT dealership_hours_weekday_check CHECK (weekday BETWEEN 1 AND 7),
    CONSTRAINT dealership_hours_shape CHECK (
        (closed AND open_time IS NULL AND close_time IS NULL AND lunch_start IS NULL AND lunch_end IS NULL)
        OR (NOT closed AND open_time IS NOT NULL AND close_time IS NOT NULL AND close_time > open_time)),
    CONSTRAINT dealership_hours_lunch CHECK (
        (lunch_start IS NULL AND lunch_end IS NULL)
        OR (lunch_start IS NOT NULL AND lunch_end IS NOT NULL AND lunch_end > lunch_start
            AND lunch_start >= open_time AND lunch_end <= close_time))
);

-- Dated closures (public holidays).
CREATE TABLE dealership_closures (
    dealership_id uuid NOT NULL REFERENCES dealerships (id),
    day           date NOT NULL,
    reason        text NOT NULL DEFAULT '',
    PRIMARY KEY (dealership_id, day),
    CONSTRAINT dealership_closures_reason_check CHECK (length(reason) <= 120)
);

-- The shared service catalog. required_skill: 1 lubrication, 2 brakes, 3 general.
-- required_bay_type: 1 express, 2 general, 3 diagnostic, 4 alignment.
CREATE TABLE service_types (
    id                uuid PRIMARY KEY,
    code              text NOT NULL UNIQUE,
    name              text NOT NULL,
    duration_minutes  integer NOT NULL,
    required_skill    smallint NOT NULL,
    required_bay_type smallint NOT NULL,
    min_skill_level   smallint NOT NULL DEFAULT 1,
    version           integer NOT NULL DEFAULT 1,
    -- On the 30-minute slot grid, at most 10 hours.
    CONSTRAINT service_types_duration_minutes_check CHECK (duration_minutes BETWEEN 30 AND 600 AND duration_minutes % 30 = 0),
    CONSTRAINT service_types_skill_check CHECK (required_skill BETWEEN 1 AND 3),
    CONSTRAINT service_types_bay_check CHECK (required_bay_type BETWEEN 1 AND 4),
    CONSTRAINT service_types_min_skill_level_check CHECK (min_skill_level BETWEEN 1 AND 4),
    CONSTRAINT service_types_version_check CHECK (version >= 1)
);
CREATE INDEX service_types_catalog_order_idx ON service_types (duration_minutes, name, id);

-- bay_type: 1 express, 2 general, 3 diagnostic, 4 alignment.
CREATE TABLE service_bays (
    id            uuid PRIMARY KEY,
    dealership_id uuid NOT NULL REFERENCES dealerships (id),
    name          text NOT NULL,
    bay_type      smallint NOT NULL,
    active        boolean NOT NULL DEFAULT true,
    UNIQUE (dealership_id, name),
    -- Target of appointments_bay_dealership_fkey.
    CONSTRAINT service_bays_id_dealership_key UNIQUE (id, dealership_id),
    CONSTRAINT service_bays_bay_type_check CHECK (bay_type BETWEEN 1 AND 4)
);

CREATE TABLE technicians (
    id            uuid PRIMARY KEY,
    dealership_id uuid NOT NULL REFERENCES dealerships (id),
    name          text NOT NULL,
    active        boolean NOT NULL DEFAULT true,
    skill_level   smallint NOT NULL DEFAULT 2,
    version       integer NOT NULL DEFAULT 1,
    -- Target of appointments_technician_dealership_fkey.
    CONSTRAINT technicians_id_dealership_key UNIQUE (id, dealership_id),
    CONSTRAINT technicians_skill_level_check CHECK (skill_level BETWEEN 1 AND 4),
    CONSTRAINT technicians_version_check CHECK (version >= 1)
);
-- Leads with dealership_id, so it also serves the foreign key and per-dealership lookups.
CREATE INDEX technicians_dealership_name_idx ON technicians (dealership_id, name, id);
CREATE INDEX technicians_name_idx ON technicians (name, id);

-- skill: 1 lubrication, 2 brakes, 3 general.
CREATE TABLE technician_skills (
    technician_id uuid NOT NULL REFERENCES technicians (id),
    skill         smallint NOT NULL,
    PRIMARY KEY (technician_id, skill),
    CONSTRAINT technician_skills_skill_check CHECK (skill BETWEEN 1 AND 3)
);

-- Weekly pattern; a technician without one works the dealership's opening hours.
CREATE TABLE technician_shifts (
    technician_id uuid NOT NULL REFERENCES technicians (id),
    weekday       smallint NOT NULL,
    start_time    time NOT NULL,
    end_time      time NOT NULL,
    break_start   time,
    break_end     time,
    PRIMARY KEY (technician_id, weekday),
    CONSTRAINT technician_shifts_weekday_check CHECK (weekday BETWEEN 1 AND 7),
    CONSTRAINT technician_shifts_window CHECK (end_time > start_time),
    CONSTRAINT technician_shifts_break CHECK (
        (break_start IS NULL AND break_end IS NULL)
        OR (break_start IS NOT NULL AND break_end IS NOT NULL AND break_end > break_start
            AND break_start >= start_time AND break_end <= end_time))
);

-- Dated absences (leave, training); never overlapping per technician.
CREATE TABLE technician_absences (
    id            uuid PRIMARY KEY,
    technician_id uuid NOT NULL REFERENCES technicians (id),
    starts_at     timestamptz NOT NULL,
    ends_at       timestamptz NOT NULL,
    reason        text NOT NULL DEFAULT '',
    created_at    timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT technician_absences_window CHECK (ends_at > starts_at),
    CONSTRAINT technician_absences_reason_check CHECK (length(reason) <= 120),
    CONSTRAINT technician_absences_no_overlap EXCLUDE USING gist (
        technician_id WITH =,
        tstzrange(starts_at, ends_at, '[)') WITH &&)
);

-- ---------------------------------------------------------------------------------------------
-- People
-- ---------------------------------------------------------------------------------------------

CREATE TABLE customers (
    id    uuid PRIMARY KEY,
    name  text NOT NULL,
    phone text NOT NULL,
    email text NOT NULL,
    CONSTRAINT customers_name_not_blank CHECK (name ~ '[^[:space:]]')
);
CREATE INDEX customers_name_idx ON customers (name, id);
CREATE INDEX customers_name_trgm_idx ON customers USING gin (name gin_trgm_ops);
CREATE INDEX customers_phone_trgm_idx ON customers USING gin (phone gin_trgm_ops);
CREATE INDEX customers_email_trgm_idx ON customers USING gin (email gin_trgm_ops);

CREATE TABLE vehicles (
    id          uuid PRIMARY KEY,
    customer_id uuid NOT NULL REFERENCES customers (id),
    vin         text NOT NULL UNIQUE,
    make        text NOT NULL,
    model       text NOT NULL,
    year        integer NOT NULL,
    CONSTRAINT vehicles_year_check CHECK (year BETWEEN 1980 AND 2100),
    -- ISO 3779: 17 characters, digits and capital letters except I, O and Q (booking.ValidVIN).
    CONSTRAINT vehicles_vin_format CHECK (vin ~ '^[A-HJ-NPR-Z0-9]{17}$')
);
CREATE INDEX vehicles_customer_idx ON vehicles (customer_id);
CREATE INDEX vehicles_vin_trgm_idx ON vehicles USING gin (vin gin_trgm_ops);
CREATE INDEX vehicles_make_trgm_idx ON vehicles USING gin (make gin_trgm_ops);
CREATE INDEX vehicles_model_trgm_idx ON vehicles USING gin (model gin_trgm_ops);

-- role: 1 admin, 2 dispatcher, 3 customer. A customer account links its customer record; a
-- dispatcher belongs to one dealership; an admin to one or (group admin) none.
CREATE TABLE users (
    id            uuid PRIMARY KEY,
    email         text NOT NULL UNIQUE,
    password_hash text NOT NULL,
    name          text NOT NULL,
    role          smallint NOT NULL,
    active        boolean NOT NULL DEFAULT true,
    customer_id   uuid UNIQUE REFERENCES customers (id),
    dealership_id uuid REFERENCES dealerships (id),
    token_version integer NOT NULL DEFAULT 0,
    created_at    timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT users_role_check CHECK (role BETWEEN 1 AND 3),
    CONSTRAINT users_customer_link_check CHECK ((role = 3) = (customer_id IS NOT NULL)),
    CONSTRAINT users_dealership_scope_check CHECK (
        (role = 3 AND dealership_id IS NULL) OR (role = 2 AND dealership_id IS NOT NULL) OR role = 1),
    CONSTRAINT users_name_not_blank CHECK (name ~ '[^[:space:]]'),
    CONSTRAINT users_token_version_check CHECK (token_version >= 0)
);
CREATE INDEX users_email_lower_idx ON users (lower(email));

-- Refresh-token sessions: one row per opaque token (only its SHA-256 is stored), grouped in
-- families that end at family_expires_at however often they are refreshed.
CREATE TABLE auth_sessions (
    id                uuid PRIMARY KEY,
    family_id         uuid NOT NULL,
    user_id           uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    token_hash        bytea NOT NULL UNIQUE,
    created_at        timestamptz NOT NULL DEFAULT now(),
    expires_at        timestamptz NOT NULL,
    family_expires_at timestamptz NOT NULL,
    rotated_at        timestamptz,
    revoked_at        timestamptz,
    user_agent        text,
    CONSTRAINT auth_sessions_token_hash_len CHECK (octet_length(token_hash) = 32),
    CONSTRAINT auth_sessions_expiry_within_family CHECK (expires_at <= family_expires_at),
    CONSTRAINT auth_sessions_user_agent_len CHECK (char_length(user_agent) <= 200)
) WITH (autovacuum_vacuum_scale_factor = 0.02, autovacuum_analyze_scale_factor = 0.02,
        autovacuum_vacuum_insert_scale_factor = 0.02);
CREATE INDEX auth_sessions_family_idx ON auth_sessions (family_id);
CREATE INDEX auth_sessions_user_live_idx ON auth_sessions (user_id) WHERE revoked_at IS NULL;
CREATE INDEX auth_sessions_expires_idx ON auth_sessions (expires_at);

-- ---------------------------------------------------------------------------------------------
-- Bookings
-- ---------------------------------------------------------------------------------------------

-- status: 1 confirmed, 2 checked_in, 3 in_progress, 4 completed, 5 cancelled, 6 no_show.
-- 1–3 hold the bay, the technician and the vehicle; 4–6 release them.
-- The booked snapshot (booked_duration_minutes, required_bay_type, required_skills,
-- min_skill_level) is fixed at booking time; catalog edits never rewrite it.
CREATE TABLE appointments (
    id                      uuid PRIMARY KEY,
    dealership_id           uuid NOT NULL REFERENCES dealerships (id),
    customer_id             uuid NOT NULL REFERENCES customers (id),
    vehicle_id              uuid NOT NULL REFERENCES vehicles (id),
    service_type_id         uuid NOT NULL REFERENCES service_types (id),
    service_bay_id          uuid NOT NULL, -- appointments_bay_dealership_fkey
    technician_id           uuid NOT NULL, -- appointments_technician_dealership_fkey
    start_at                timestamptz NOT NULL,
    end_at                  timestamptz NOT NULL,
    status                  smallint NOT NULL,
    is_waiter               boolean NOT NULL DEFAULT false,
    booked_duration_minutes smallint NOT NULL,
    required_bay_type       smallint NOT NULL,
    required_skills         smallint[] NOT NULL,
    min_skill_level         smallint NOT NULL,
    idempotency_key         text,
    idempotency_fingerprint text,
    created_by              uuid REFERENCES users (id),
    version                 integer NOT NULL DEFAULT 1,
    created_at              timestamptz NOT NULL DEFAULT now(),
    updated_at              timestamptz NOT NULL DEFAULT now(),
    -- Reset on book and on every move: reminders are keyed on it, so a reassign never re-reminds.
    scheduled_at            timestamptz NOT NULL DEFAULT now(),
    checked_in_at           timestamptz,
    started_at              timestamptz,
    completed_at            timestamptz,
    -- Set when in-progress work runs past what the overrun sweep could extend.
    overrun_at              timestamptz,
    CONSTRAINT appointments_window CHECK (end_at > start_at),
    CONSTRAINT appointments_max_span CHECK (end_at - start_at <= interval '24 hours'),
    CONSTRAINT appointments_status_check CHECK (status BETWEEN 1 AND 6),
    CONSTRAINT appointments_booked_duration_check CHECK (booked_duration_minutes > 0),
    CONSTRAINT appointments_required_bay_check CHECK (required_bay_type BETWEEN 1 AND 4),
    CONSTRAINT appointments_required_skills_check CHECK (cardinality(required_skills) >= 1),
    CONSTRAINT appointments_min_skill_check CHECK (min_skill_level BETWEEN 1 AND 4),
    CONSTRAINT appointments_version_check CHECK (version >= 1),
    -- The bay and the technician belong to the appointment's dealership.
    CONSTRAINT appointments_bay_dealership_fkey
        FOREIGN KEY (service_bay_id, dealership_id) REFERENCES service_bays (id, dealership_id),
    CONSTRAINT appointments_technician_dealership_fkey
        FOREIGN KEY (technician_id, dealership_id) REFERENCES technicians (id, dealership_id),
    CONSTRAINT appointments_bay_no_overlap EXCLUDE USING gist (
        service_bay_id WITH =, tstzrange(start_at, end_at, '[)') WITH &&) WHERE (status IN (1, 2, 3)),
    CONSTRAINT appointments_technician_no_overlap EXCLUDE USING gist (
        technician_id WITH =, tstzrange(start_at, end_at, '[)') WITH &&) WHERE (status IN (1, 2, 3)),
    CONSTRAINT appointments_vehicle_no_overlap EXCLUDE USING gist (
        vehicle_id WITH =, tstzrange(start_at, end_at, '[)') WITH &&) WHERE (status IN (1, 2, 3))
);
-- Idempotency keys are scoped per creating user.
CREATE UNIQUE INDEX appointments_idempotency_scope_uniq ON appointments (created_by, idempotency_key)
    NULLS NOT DISTINCT WHERE idempotency_key IS NOT NULL;
-- Day board, availability and keyset lists.
CREATE INDEX appointments_start_idx ON appointments (start_at);
CREATE INDEX appointments_dealership_start_idx ON appointments (dealership_id, start_at);
CREATE INDEX appointments_customer_start_idx ON appointments (customer_id, start_at);
CREATE INDEX appointments_vehicle_start_idx ON appointments (vehicle_id, start_at);
CREATE INDEX appointments_technician_start_idx ON appointments (technician_id, start_at) WHERE status IN (1, 2, 3);
-- Work on site (2 checked_in, 3 in_progress) lists as upcoming until it ends, without a start bound.
CREATE INDEX appointments_on_site_start_idx ON appointments (start_at, id) WHERE status IN (2, 3);
CREATE INDEX appointments_on_site_dealership_idx ON appointments (dealership_id, start_at, id) WHERE status IN (2, 3);
CREATE INDEX appointments_on_site_customer_idx ON appointments (customer_id, start_at, id) WHERE status IN (2, 3);
CREATE INDEX appointments_on_site_vehicle_idx ON appointments (vehicle_id, start_at, id) WHERE status IN (2, 3);
-- Sweeps. The in-progress index also serves the flagged-overrun hold checks (store.overrunHeldSQL):
-- only the few jobs on a lift right now are status 3.
CREATE INDEX appointments_no_show_sweep_idx ON appointments (start_at) WHERE status = 1;
CREATE INDEX appointments_overrun_sweep_idx ON appointments (end_at) WHERE status = 3;

-- The job lines of an appointment (1–5, booking.MaxJobLines), done back to back by one technician in one bay.
CREATE TABLE appointment_lines (
    appointment_id   uuid NOT NULL REFERENCES appointments (id),
    line_no          smallint NOT NULL,
    service_type_id  uuid NOT NULL REFERENCES service_types (id),
    service_code     text NOT NULL,
    service_name     text NOT NULL,
    duration_minutes smallint NOT NULL,
    PRIMARY KEY (appointment_id, line_no),
    CONSTRAINT appointment_lines_line_no_check CHECK (line_no BETWEEN 1 AND 5),
    CONSTRAINT appointment_lines_duration_minutes_check CHECK (duration_minutes > 0)
);

-- Audit: one row per appointment change, written in the change's transaction.
-- event_type: see outbox_events (appointment events 1–11 only).
CREATE TABLE appointment_history (
    id             bigint GENERATED BY DEFAULT AS IDENTITY PRIMARY KEY,
    appointment_id uuid NOT NULL REFERENCES appointments (id),
    version        integer NOT NULL,
    event_type     smallint NOT NULL,
    actor_id       uuid REFERENCES users (id),
    at             timestamptz NOT NULL DEFAULT now(),
    status         smallint NOT NULL,
    start_at       timestamptz NOT NULL,
    end_at         timestamptz NOT NULL,
    service_bay_id uuid NOT NULL,
    technician_id  uuid NOT NULL,
    CONSTRAINT appointment_history_event_type_check CHECK (event_type BETWEEN 1 AND 11),
    CONSTRAINT appointment_history_status_check CHECK (status BETWEEN 1 AND 6)
);
CREATE INDEX appointment_history_appointment_idx ON appointment_history (appointment_id, id);

-- ---------------------------------------------------------------------------------------------
-- Queues
-- ---------------------------------------------------------------------------------------------

-- Transactional outbox, published to Kafka after commit.
-- aggregate_type: 1 appointment, 2 dealership, 3 service_type, 4 technician, 5 user.
-- event_type: 1 APPOINTMENT_CREATED, 2 APPOINTMENT_RESCHEDULED, 3 APPOINTMENT_CANCELLED,
--   4 APPOINTMENT_CHECKED_IN, 5 APPOINTMENT_STARTED, 6 APPOINTMENT_COMPLETED, 7 APPOINTMENT_NO_SHOW,
--   8 APPOINTMENT_REINSTATED, 9 APPOINTMENT_REASSIGNED, 10 APPOINTMENT_EXTENDED,
--   11 APPOINTMENT_OVERRUNNING, 12 SESSION_REVOKED, 13 CONFIG_UPDATED.
-- status: 1 pending, 2 processing, 3 published, 4 failed.
-- seq is drawn at INSERT, after the aggregate's row lock, so one aggregate's events are numbered
-- in commit order; the worker claims in seq order with one event in flight per aggregate.
CREATE TABLE outbox_events (
    id              uuid PRIMARY KEY,
    seq             bigint GENERATED BY DEFAULT AS IDENTITY,
    aggregate_type  smallint NOT NULL,
    aggregate_id    uuid NOT NULL,
    event_type      smallint NOT NULL,
    payload         jsonb NOT NULL,
    status          smallint NOT NULL DEFAULT 1,
    attempts        integer NOT NULL DEFAULT 0,
    -- Per-message content rejections (too large, corrupt); only these lead to 4 failed.
    rejections      integer NOT NULL DEFAULT 0,
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    claimed_at      timestamptz,
    published_at    timestamptz,
    last_error      text,
    created_at      timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT outbox_events_aggregate_type_check CHECK (aggregate_type BETWEEN 1 AND 5),
    CONSTRAINT outbox_events_event_type_check CHECK (event_type BETWEEN 1 AND 13),
    CONSTRAINT outbox_events_status_check CHECK (status BETWEEN 1 AND 4),
    CONSTRAINT outbox_events_attempts_check CHECK (attempts >= 0),
    CONSTRAINT outbox_events_rejections_check CHECK (rejections >= 0)
) WITH (autovacuum_vacuum_scale_factor = 0.02, autovacuum_analyze_scale_factor = 0.02,
        autovacuum_vacuum_insert_scale_factor = 0.02);
CREATE INDEX outbox_events_ready_idx ON outbox_events (next_attempt_at, seq) WHERE status = 1;
CREATE INDEX outbox_events_inflight_idx ON outbox_events (aggregate_id, seq) WHERE status IN (1, 2);
CREATE INDEX outbox_events_processing_agg_idx ON outbox_events (aggregate_id, claimed_at) WHERE status = 2;
CREATE INDEX outbox_events_published_idx ON outbox_events (published_at) WHERE status = 3;
CREATE INDEX outbox_events_failed_page_idx ON outbox_events (created_at, id) WHERE status = 4;

-- Customer messages, enqueued idempotently (dedupe_key) and sent by a leased sender.
-- kind: 1 confirmation, 2 rescheduled, 3 cancelled, 4 no_show, 5 reminder_24h, 6 reminder_2h.
-- channel: 1 email, 2 sms.
-- status: 1 pending, 2 sending, 3 sent, 4 failed, 5 skipped.
CREATE TABLE notifications (
    id                  uuid PRIMARY KEY,
    appointment_id      uuid NOT NULL REFERENCES appointments (id),
    appointment_version integer,
    kind                smallint NOT NULL,
    channel             smallint NOT NULL,
    -- The appointment start the notice is about; reminders are deduplicated and skipped by slot.
    start_at            timestamptz NOT NULL,
    recipient           text NOT NULL,
    subject             text NOT NULL,
    body                text NOT NULL,
    dedupe_key          text NOT NULL UNIQUE,
    status              smallint NOT NULL DEFAULT 1,
    attempts            integer NOT NULL DEFAULT 0,
    next_attempt_at     timestamptz NOT NULL DEFAULT now(),
    claimed_at          timestamptz,
    sent_at             timestamptz,
    last_error          text,
    created_at          timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT notifications_kind_check CHECK (kind BETWEEN 1 AND 6),
    CONSTRAINT notifications_channel_check CHECK (channel BETWEEN 1 AND 2),
    CONSTRAINT notifications_status_check CHECK (status BETWEEN 1 AND 5),
    CONSTRAINT notifications_attempts_check CHECK (attempts >= 0)
) WITH (autovacuum_vacuum_scale_factor = 0.02, autovacuum_analyze_scale_factor = 0.02,
        autovacuum_vacuum_insert_scale_factor = 0.02);
CREATE INDEX notifications_ready_idx ON notifications (next_attempt_at) WHERE status = 1;
CREATE INDEX notifications_sending_idx ON notifications (claimed_at) WHERE status = 2;
CREATE INDEX notifications_done_idx ON notifications (status, created_at) WHERE status IN (3, 4, 5);
CREATE INDEX notifications_appointment_channel_idx ON notifications (appointment_id, channel, appointment_version);
-- One live reminder (5 reminder_24h, 6 reminder_2h) per appointment, kind, slot and channel.
CREATE UNIQUE INDEX notifications_reminder_slot_uniq ON notifications (appointment_id, kind, start_at, channel)
    WHERE kind IN (5, 6) AND status <> 5;
