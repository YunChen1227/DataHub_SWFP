-- 0011: 全局上游白名单 + 运行时开关
-- 方言：PostgreSQL
-- 背景：对账单里已计过费的 (税号, 上游, 票/税) 可作为白名单；管理后台可热切换
--       「仅允许白名单组合」闸门。关时行为与改造前一致（含 sy 的 CSV 闸门）。
--
-- system_setting：通用键值（首个键 upstream_allowlist_enabled，默认 false）。
-- upstream_allowlist：主键 (credit_code, provider)；同键多源导入时维度取并集。

CREATE TABLE IF NOT EXISTS system_setting (
    key        VARCHAR(64)  PRIMARY KEY,
    value      TEXT         NOT NULL DEFAULT '',
    updated_at TIMESTAMPTZ  NOT NULL DEFAULT now()
);

INSERT INTO system_setting (key, value)
VALUES ('upstream_allowlist_enabled', 'false')
ON CONFLICT (key) DO NOTHING;

CREATE TABLE IF NOT EXISTS upstream_allowlist (
    credit_code  VARCHAR(32)  NOT NULL,
    provider     VARCHAR(32)  NOT NULL,
    tax          BOOLEAN      NOT NULL DEFAULT false,
    invoice      BOOLEAN      NOT NULL DEFAULT false,
    call_time    TIMESTAMPTZ,
    source_file  TEXT         NOT NULL DEFAULT '',
    updated_at   TIMESTAMPTZ  NOT NULL DEFAULT now(),
    PRIMARY KEY (credit_code, provider),
    CONSTRAINT chk_upstream_allowlist_provider
        CHECK (provider IN ('entcredit', 'ctax', 'sy', 'salesdata')),
    CONSTRAINT chk_upstream_allowlist_dims
        CHECK (tax OR invoice)
);

CREATE INDEX IF NOT EXISTS idx_upstream_allowlist_provider
    ON upstream_allowlist (provider);
