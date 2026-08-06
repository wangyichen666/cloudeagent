-- 状态面数据模型（与文档「八、核心设计：数据模型」一致）
-- 注意：模型凭证（baseURL/apiKey）永不落库，见文档 6.2。

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
