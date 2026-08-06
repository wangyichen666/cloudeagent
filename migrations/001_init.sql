-- 状态面初始化脚本（与 internal/store/schema.sql 保持一致，便于 DBA 审阅）。
-- 控制面在 store=postgres 启动时会自动执行等价语句。

CREATE TABLE IF NOT EXISTS agent_instances (
  user_id       TEXT PRIMARY KEY,
  instance_name TEXT NOT NULL,
  status        TEXT NOT NULL,
  workspace     TEXT NOT NULL DEFAULT '',
  endpoint      TEXT NOT NULL DEFAULT '',
  port          INT  NOT NULL DEFAULT 0,
  error         TEXT NOT NULL DEFAULT '',
  created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS agent_activity (
  user_id        TEXT PRIMARY KEY,
  last_active_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  ws_connections INT NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS code_reviews (
  review_id  TEXT PRIMARY KEY,
  user_id    TEXT NOT NULL,
  repo       TEXT NOT NULL,
  pr_number  INT  NOT NULL DEFAULT 0,
  status     TEXT NOT NULL,
  model      TEXT NOT NULL DEFAULT '',
  findings   JSONB NOT NULL DEFAULT '[]',
  error      TEXT NOT NULL DEFAULT '',
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS user_vcs_tokens (
  user_id   TEXT NOT NULL,
  platform  TEXT NOT NULL,
  token     TEXT NOT NULL,
  PRIMARY KEY (user_id, platform)
);

CREATE INDEX IF NOT EXISTS idx_code_reviews_user ON code_reviews (user_id);
