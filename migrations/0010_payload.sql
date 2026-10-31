-- 0010: 请求原始数据留存（对账出账）
-- 方言：PostgreSQL
-- 背景：此前只落了审计/逐源明细的结论列，上游返回的业务数据与返回给下游的数据
--       一律不落库。对账时需要按数据内容判定（如「近 12 个月金额全为 0 不向下游
--       计费」），故新增两张表留存原始数据，并在写入时算好判定结果。
--
-- 兼容口径：
--   - 只新建表，不改任何老表——旧版本代码回滚后照常运行。
--   - 报表以 audit_log / upstream_call 为主表 LEFT JOIN 本表：上线前的老请求照样
--     出现在账单里，本表无对应行即「历史无原始数据」，单独计数，不会漏。
--   - 写入一律 ON CONFLICT DO NOTHING：本地 spool 重放不会写重复。
--
-- 零数据判定：inv_zero / tax_zero 为 NULL 表示该维度本次没查得（无从判定）；
-- 金额单位为元。rule_version 记录判定规则版本，规则变更后可用
-- scripts/recompute_payload_flags.go 从 raw 数据重算。

-- 1. request_payload：一次请求一行
CREATE TABLE IF NOT EXISTS request_payload (
    id             BIGSERIAL     PRIMARY KEY,
    request_id     VARCHAR(64)   NOT NULL,
    app_key        VARCHAR(64)   NOT NULL DEFAULT '',
    version        VARCHAR(16)   NOT NULL DEFAULT '',
    reqid          VARCHAR(32)   NOT NULL DEFAULT '',
    credit_code    VARCHAR(32)   NOT NULL DEFAULT '',
    request_body   JSONB,                              -- 下游入参（不含签名）
    response_range JSONB,                              -- 实际返回给下游的契约数据
    inv_amt_12m    NUMERIC(24,2),
    tax_amt_12m    NUMERIC(24,2),
    inv_zero       BOOLEAN,
    tax_zero       BOOLEAN,
    rule_version   VARCHAR(16)   NOT NULL DEFAULT '',
    created_at     TIMESTAMPTZ   NOT NULL DEFAULT now(),
    CONSTRAINT uq_request_payload UNIQUE (request_id)
);
CREATE INDEX IF NOT EXISTS idx_request_payload_created_at ON request_payload (created_at);

-- 2. upstream_payload：一个查得/部分返回的源一行
CREATE TABLE IF NOT EXISTS upstream_payload (
    id             BIGSERIAL     PRIMARY KEY,
    request_id     VARCHAR(64)   NOT NULL,
    app_key        VARCHAR(64)   NOT NULL DEFAULT '',
    version        VARCHAR(16)   NOT NULL DEFAULT '',
    reqid          VARCHAR(32)   NOT NULL DEFAULT '',
    source_label   VARCHAR(32)   NOT NULL,
    source_name    VARCHAR(32)   NOT NULL DEFAULT '',
    provider       VARCHAR(32)   NOT NULL DEFAULT '',
    status         VARCHAR(16)   NOT NULL DEFAULT '',
    raw_data       JSONB,                              -- 该源返回的业务数据（解密/解包后）
    inv_amt_12m    NUMERIC(24,2),
    tax_amt_12m    NUMERIC(24,2),
    inv_zero       BOOLEAN,
    tax_zero       BOOLEAN,
    rule_version   VARCHAR(16)   NOT NULL DEFAULT '',
    created_at     TIMESTAMPTZ   NOT NULL DEFAULT now(),
    CONSTRAINT uq_upstream_payload UNIQUE (request_id, source_label)
);
CREATE INDEX IF NOT EXISTS idx_upstream_payload_created_at ON upstream_payload (created_at);

-- 3. 出账报表按时间区间扫 audit_log：补一个时间索引（只加索引，不改列，旧代码无感）。
CREATE INDEX IF NOT EXISTS idx_audit_created_at ON audit_log (created_at);
