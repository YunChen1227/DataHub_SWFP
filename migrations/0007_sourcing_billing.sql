-- 多源寻源计费与上游对账 (v1.0)
-- 方言：PostgreSQL
-- 背景：上游调用由「并发扇出全部子源」改为「按优先级串行、命中即停」，计费由
--       固定单价改为按【实际查得维度】定档（发票+税务 / 单发票 / 单税务 / 查无不计费）。
--       随之需要三样东西落库：
--         1) license 上的三档合同价（rate_*_fen，单位分；0 = 走全局缺省费率）
--         2) 台账上的本次计费标准/应收金额/上游总成本 + 逐源汇总
--         3) 逐源明细表 upstream_call：每源一行（含未调用的 skipped），带各自的
--            上游订单号/请求号与成本——这是向上游对账的原子凭证，改造前多源的
--            N 组标识被归并成一对、其余丢弃，无法逐笔核对。
-- 金额一律用「分」的整数，不用浮点：对账要逐笔相加，不能有舍入漂移。

-- 1. license：三档合同价
ALTER TABLE license ADD COLUMN IF NOT EXISTS rate_both_fen    BIGINT NOT NULL DEFAULT 0;
ALTER TABLE license ADD COLUMN IF NOT EXISTS rate_invoice_fen BIGINT NOT NULL DEFAULT 0;
ALTER TABLE license ADD COLUMN IF NOT EXISTS rate_tax_fen     BIGINT NOT NULL DEFAULT 0;

-- 2. billing_ledger：本次计费标准 / 应收 / 上游成本 / 逐源汇总
--    upstream_cost_fen 无条件回填：即便本次对下游不计费（查无/全失败），钱也已经
--    花了，亏损单必须在库里看得见。
ALTER TABLE billing_ledger ADD COLUMN IF NOT EXISTS fee_standard      VARCHAR(16) NOT NULL DEFAULT '';
ALTER TABLE billing_ledger ADD COLUMN IF NOT EXISTS amount_fen        BIGINT      NOT NULL DEFAULT 0;
ALTER TABLE billing_ledger ADD COLUMN IF NOT EXISTS upstream_cost_fen BIGINT      NOT NULL DEFAULT 0;
ALTER TABLE billing_ledger ADD COLUMN IF NOT EXISTS source_total      INT         NOT NULL DEFAULT 0;
ALTER TABLE billing_ledger ADD COLUMN IF NOT EXISTS source_ok         INT         NOT NULL DEFAULT 0;
ALTER TABLE billing_ledger ADD COLUMN IF NOT EXISTS source_err        INT         NOT NULL DEFAULT 0;

-- 3. audit_log：原始请求维度 / 实得维度 / 计费标准 / 应收 / 上游成本
--    req_scope 与 data_scope 的差额 = 「客户要了但没给到」的部分，是计费争议时的
--    第一手证据。
ALTER TABLE audit_log ADD COLUMN IF NOT EXISTS req_scope         VARCHAR(16) NOT NULL DEFAULT '';
ALTER TABLE audit_log ADD COLUMN IF NOT EXISTS data_scope        VARCHAR(16) NOT NULL DEFAULT '';
ALTER TABLE audit_log ADD COLUMN IF NOT EXISTS fee_standard      VARCHAR(16) NOT NULL DEFAULT '';
ALTER TABLE audit_log ADD COLUMN IF NOT EXISTS amount_fen        BIGINT      NOT NULL DEFAULT 0;
ALTER TABLE audit_log ADD COLUMN IF NOT EXISTS upstream_cost_fen BIGINT      NOT NULL DEFAULT 0;

-- 4. upstream_call：逐源明细（追加写）
--    关联键与台账同构 (app_key, version, reqid)，另带 request_id 供后台按请求下钻。
--    唯一键 (app_key, version, reqid, label) 保证重放/重试幂等（写入用
--    ON CONFLICT DO NOTHING）。
CREATE TABLE IF NOT EXISTS upstream_call (
    id            BIGSERIAL   PRIMARY KEY,
    app_key       VARCHAR(64) NOT NULL,
    version       VARCHAR(16) NOT NULL DEFAULT '',
    reqid         VARCHAR(32) NOT NULL,
    request_id    VARCHAR(64) NOT NULL,
    seq            INT         NOT NULL DEFAULT 0,      -- 本次请求内的调用顺序 (1 起; skipped 为 0)
    source_name    VARCHAR(32) NOT NULL,                -- 逻辑源名（寻源优先级列表的单位）
    source_label   VARCHAR(32) NOT NULL,                -- 契约段名 invoice1/invoice2/tax1/tax2/sales
    source_alias   VARCHAR(16) NOT NULL DEFAULT '',     -- 对下游脱敏编号 源1..源5
    provider       VARCHAR(32) NOT NULL DEFAULT '',     -- 上游 kind: entcredit/salesdata
    dim_invoice    BOOLEAN     NOT NULL DEFAULT FALSE,  -- 该次调用覆盖发票维度
    dim_tax        BOOLEAN     NOT NULL DEFAULT FALSE,  -- 该次调用覆盖税务维度
    status         VARCHAR(16) NOT NULL,                -- ok|empty|error|skipped
    billable       BOOLEAN     NOT NULL DEFAULT FALSE,  -- 该源是否构成本次计费依据
    upstream_code  VARCHAR(32) NOT NULL DEFAULT '',
    upstream_msg   VARCHAR(256) NOT NULL DEFAULT '',
    upstream_uid   VARCHAR(64) NOT NULL DEFAULT '',     -- 该源自己的上游订单号
    upstream_logid VARCHAR(64) NOT NULL DEFAULT '',     -- 该源自己的上游请求号
    cost_fen       BIGINT      NOT NULL DEFAULT 0,      -- 该次调用产生的上游成本
    latency_ms     BIGINT      NOT NULL DEFAULT 0,
    skip_reason    VARCHAR(128) NOT NULL DEFAULT '',    -- status=skipped 时的原因
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT uq_upstream_call UNIQUE (app_key, version, reqid, source_label)
);

CREATE INDEX IF NOT EXISTS idx_upstream_call_request_id ON upstream_call (request_id);
CREATE INDEX IF NOT EXISTS idx_upstream_call_created_at ON upstream_call (created_at);
