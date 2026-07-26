-- Console独立库的初始schema（console-fleet-design §10，照录）。
-- 这是Console库迁移的唯一一份源；节点库的迁移在internal/store/migrations/，
-- 两者schema无交集（CLAUDE.md：每个库的迁移各有唯一一份源）。

CREATE TABLE admin_users (
  id UUID PRIMARY KEY,
  username TEXT NOT NULL UNIQUE,
  password_hash TEXT NOT NULL,           -- Argon2id
  totp_secret_enc BYTEA NOT NULL,        -- AES-256-GCM
  totp_nonce BYTEA NOT NULL,
  disabled_at TIMESTAMPTZ,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE admin_sessions (
  id UUID PRIMARY KEY,
  user_id UUID NOT NULL REFERENCES admin_users(id),
  token_hash TEXT NOT NULL UNIQUE,       -- 只存哈希，cookie里是明文
  client_ip TEXT NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  last_seen_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  expires_at TIMESTAMPTZ NOT NULL,
  revoked_at TIMESTAMPTZ
);

-- DNS zone：支持多个域名（设计§6.2）
CREATE TABLE dns_zones (
  id UUID PRIMARY KEY,
  domain TEXT NOT NULL UNIQUE,           -- example.com
  provider TEXT NOT NULL,                -- manual|route53
  provider_ref TEXT NOT NULL DEFAULT '', -- route53的hosted zone id
  credential_enc BYTEA,                  -- AES-256-GCM；manual模式为NULL
  credential_nonce BYTEA,
  subdomain_prefix TEXT NOT NULL DEFAULT 'api',  -- api-01 里的 "api"
  next_seq INT NOT NULL DEFAULT 1,       -- 单调递增，永不回收（设计§6.2）
  caa_ok BOOLEAN,                        -- CAA预检结果（设计§6.5）
  caa_checked_at TIMESTAMPTZ,
  accepting_new BOOLEAN NOT NULL DEFAULT TRUE,  -- 证书预算超限时置false
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE nodes (
  id UUID PRIMARY KEY,
  name TEXT NOT NULL UNIQUE,
  host TEXT NOT NULL,                    -- 公网IP
  ssh_port INT NOT NULL DEFAULT 22,
  ssh_user TEXT NOT NULL,
  host_key_fp TEXT,                      -- TOFU固定
  status TEXT NOT NULL,                  -- new|provisioning|ready|degraded|unreachable|host_key_changed
  os_release TEXT, arch TEXT,
  stack_version TEXT,                    -- 已部署的产物版本
  last_seen_at TIMESTAMPTZ,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE node_credentials (
  node_id UUID PRIMARY KEY REFERENCES nodes(id) ON DELETE CASCADE,
  private_key_enc BYTEA NOT NULL,        -- AES-256-GCM；密码永不出现在本表
  nonce BYTEA NOT NULL,
  public_key TEXT NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  rotated_at TIMESTAMPTZ
);

-- 子域名分配：seq永不回收，退役后记录保留并标记（设计§6.2、§6.4）
CREATE TABLE node_domains (
  id UUID PRIMARY KEY,
  zone_id UUID NOT NULL REFERENCES dns_zones(id),
  seq INT NOT NULL,
  fqdn TEXT NOT NULL UNIQUE,             -- api-03.example.com
  node_id UUID REFERENCES nodes(id) ON DELETE SET NULL,  -- 退役后置NULL但保留行
  target_ip TEXT NOT NULL,
  record_state TEXT NOT NULL,            -- pending|insync|removed|orphaned
  dns_verified_at TIMESTAMPTZ,
  cert_issuer TEXT,
  cert_expires_at TIMESTAMPTZ,
  released_at TIMESTAMPTZ,               -- 退役时间；seq仍不可复用
  UNIQUE (zone_id, seq)
);

-- 证书签发记账，用于预算水位（设计§6.5）
CREATE TABLE cert_issuances (
  id BIGSERIAL PRIMARY KEY,
  zone_id UUID NOT NULL REFERENCES dns_zones(id),
  fqdn TEXT NOT NULL,
  issuer TEXT NOT NULL,
  observed_at TIMESTAMPTZ NOT NULL DEFAULT now(),  -- 巡检发现证书序列号变化时记一笔
  serial TEXT NOT NULL
);
CREATE INDEX cert_issuances_window ON cert_issuances (zone_id, observed_at);

CREATE TABLE provision_runs (
  id UUID PRIMARY KEY,
  node_id UUID NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
  kind TEXT NOT NULL,                    -- bootstrap|redeploy|domain|rotate-key|decommission|diagnose
  status TEXT NOT NULL,                  -- running|succeeded|failed|cancelled
  triggered_by UUID REFERENCES admin_users(id),
  started_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  finished_at TIMESTAMPTZ
);

CREATE TABLE provision_steps (
  run_id UUID NOT NULL REFERENCES provision_runs(id) ON DELETE CASCADE,
  seq INT NOT NULL,
  name TEXT NOT NULL,
  status TEXT NOT NULL,                  -- pending|running|succeeded|skipped|failed
  exit_code INT,
  log_path TEXT,
  started_at TIMESTAMPTZ,
  finished_at TIMESTAMPTZ,
  PRIMARY KEY (run_id, seq)
);

-- 节点上项目的镜像副本，非权威（权威在节点自己的库）
CREATE TABLE node_projects (
  id UUID PRIMARY KEY,
  node_id UUID NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
  slug TEXT NOT NULL,
  remote_project_id TEXT NOT NULL,       -- 节点库里的UUID
  disk_limit_bytes BIGINT NOT NULL,
  five_hour_limit BIGINT NOT NULL,
  seven_day_limit BIGINT NOT NULL,
  synced_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (node_id, slug)
);

-- 只记签发事件，绝不存CDK明文、哈希或secret部分
CREATE TABLE cdk_issues (
  id UUID PRIMARY KEY,
  node_project_id UUID NOT NULL REFERENCES node_projects(id) ON DELETE CASCADE,
  public_id TEXT NOT NULL UNIQUE,        -- 可公开部分；供 /connect 查询与对账（设计§6.6）
  issued_by UUID REFERENCES admin_users(id),
  issued_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  revoked_at TIMESTAMPTZ
);

CREATE TABLE releases (
  version TEXT PRIMARY KEY,
  notes TEXT NOT NULL DEFAULT '',
  published_at TIMESTAMPTZ,              -- NULL=已构建未发布，下载页不展示
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE release_artifacts (
  version TEXT NOT NULL REFERENCES releases(version) ON DELETE CASCADE,
  os TEXT NOT NULL, arch TEXT NOT NULL,
  filename TEXT NOT NULL,
  size_bytes BIGINT NOT NULL,
  sha256 TEXT NOT NULL,
  PRIMARY KEY (version, os, arch)
);

CREATE TABLE audit_log (
  id BIGSERIAL PRIMARY KEY,
  actor UUID REFERENCES admin_users(id),
  action TEXT NOT NULL,
  target TEXT NOT NULL,
  result TEXT NOT NULL,                  -- ok|denied|error
  detail JSONB NOT NULL DEFAULT '{}',    -- 写入前必经redact
  client_ip TEXT NOT NULL,
  at TIMESTAMPTZ NOT NULL DEFAULT now()
);
