CREATE TABLE accounts (
  id UUID PRIMARY KEY,
  name TEXT NOT NULL,
  upstream_pool TEXT NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE projects (
  id UUID PRIMARY KEY,
  account_id UUID NOT NULL REFERENCES accounts(id),
  slug TEXT NOT NULL UNIQUE,
  container_name TEXT NOT NULL UNIQUE,
  disk_limit_bytes BIGINT NOT NULL,
  five_hour_limit BIGINT NOT NULL,
  seven_day_limit BIGINT NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE cdks (
  id UUID PRIMARY KEY,
  project_id UUID NOT NULL REFERENCES projects(id),
  public_id TEXT NOT NULL UNIQUE,
  secret_hash TEXT NOT NULL,
  expires_at TIMESTAMPTZ,
  disabled_at TIMESTAMPTZ
);

CREATE TABLE usage_events (
  id BIGSERIAL PRIMARY KEY,
  project_id UUID NOT NULL REFERENCES projects(id),
  occurred_at TIMESTAMPTZ NOT NULL,
  model TEXT NOT NULL,
  input_tokens BIGINT NOT NULL,
  output_tokens BIGINT NOT NULL,
  cache_read_tokens BIGINT NOT NULL,
  cache_write_tokens BIGINT NOT NULL,
  weighted_units BIGINT NOT NULL,
  source_event_id TEXT NOT NULL,
  UNIQUE (project_id, source_event_id)
);
CREATE INDEX usage_events_window ON usage_events (project_id, occurred_at);

CREATE TABLE file_index (
  project_id UUID NOT NULL REFERENCES projects(id),
  path TEXT NOT NULL,
  size_bytes BIGINT NOT NULL,
  sha256 TEXT NOT NULL,
  server_revision BIGINT NOT NULL,
  deleted BOOLEAN NOT NULL DEFAULT FALSE,
  updated_by_device TEXT NOT NULL DEFAULT '',
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (project_id, path)
);

CREATE TABLE sessions (
  id UUID PRIMARY KEY,
  project_id UUID NOT NULL REFERENCES projects(id),
  tmux_name TEXT NOT NULL,
  connected_at TIMESTAMPTZ,
  last_seen_at TIMESTAMPTZ NOT NULL,
  state TEXT NOT NULL,
  UNIQUE (project_id, tmux_name)
);

CREATE TABLE usage_offsets (
  project_id UUID NOT NULL REFERENCES projects(id),
  file_identity TEXT NOT NULL,
  path TEXT NOT NULL,
  committed_offset BIGINT NOT NULL DEFAULT 0,
  partial_line TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (project_id, file_identity)
);
