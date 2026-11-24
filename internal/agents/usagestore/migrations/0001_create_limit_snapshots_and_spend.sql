-- usage-store: subscription usage windows, and what each API key cost.
--
-- Times are unix seconds. JSON columns hold the provider's answer as text,
-- exactly as it came.

-- One row per window of one provider's reading. Rows are only ever added: a
-- provider's latest reading is the rows that share its newest snapshot_at.
CREATE TABLE limit_snapshots (
    id             bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    snapshot_at    bigint NOT NULL,
    provider       text NOT NULL,
    window_key     text NOT NULL,
    used_percent   double precision NOT NULL,
    window_minutes bigint,
    resets_at      bigint,
    plan_type      text,
    tier           text,
    source         text NOT NULL,
    raw_json       text
);
CREATE INDEX limit_snapshots_lookup ON limit_snapshots (provider, window_key, snapshot_at DESC);
CREATE INDEX limit_snapshots_at ON limit_snapshots (snapshot_at DESC);
CREATE INDEX limit_snapshots_provider_at ON limit_snapshots (provider, snapshot_at DESC);

-- What the provider last said about each API key.
CREATE TABLE spend_keys (
    provider       text NOT NULL,
    api_key_id     text NOT NULL,
    api_key_name   text NOT NULL DEFAULT '',
    api_key_hint   text NOT NULL DEFAULT '',
    api_key_status text NOT NULL DEFAULT '',
    raw_json       text,
    fetched_at     bigint NOT NULL,
    PRIMARY KEY (provider, api_key_id)
);

-- One key's spend on one UTC day (YYYY-MM-DD), replaced on each fetch.
CREATE TABLE spend_daily (
    provider    text NOT NULL,
    api_key_id  text NOT NULL,
    date        text NOT NULL,
    total_usd   double precision NOT NULL,
    fetched_at  bigint NOT NULL,
    PRIMARY KEY (provider, api_key_id, date)
);
CREATE INDEX spend_daily_provider_date ON spend_daily (provider, date);

-- Credit a person bought for a provider account.
CREATE TABLE spend_topups (
    id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    provider    text NOT NULL,
    amount_usd  double precision NOT NULL,
    occurred_at bigint NOT NULL,
    note        text NOT NULL DEFAULT '',
    created_at  bigint NOT NULL
);
CREATE INDEX spend_topups_lookup ON spend_topups (provider, occurred_at ASC);

-- Account-wide spend totals an older version of the standalone store wrote.
-- Nothing writes or reads them now; the import keeps them so no record is
-- lost.
CREATE TABLE spend_snapshots (
    id           bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    fetched_at   bigint NOT NULL,
    provider     text NOT NULL,
    "window"     text NOT NULL,
    period_start bigint NOT NULL,
    period_end   bigint NOT NULL,
    total_usd    double precision NOT NULL,
    currency     text NOT NULL,
    source       text NOT NULL,
    raw_json     text
);
