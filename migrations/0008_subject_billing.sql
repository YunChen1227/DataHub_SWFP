-- 主体年度计费 (v1.0) — 设计_主体年度计费.md
-- 方言：PostgreSQL
--
-- 背景：计费口径从「每次查得按实得维度定档」改为「同客户 + 同社会信用代码 + 同类目，
--       首次查得计费一次，此后一年内免费」。两个轴正交相乘，不新增定价档位：
--
--         charged  = Got − Covered            实得类目，减掉还在免费期的
--         Standard = StandardOf(charged)      复用现有 both/invoice/tax/none 四档
--         Amount   = rates.Of(Standard)       复用 license 上的三档合同价
--
--       窗口按【周年制】：expires_at = charged_at + interval '1 year'，即 2026-12-28
--       首次计费则免到 2027-12-28。用 Postgres 日历 interval 而非固定 8760h，闰年安全。
--       免费查询不延长窗口，到期后下一次查得重新计费并开新窗口。
--
-- 本迁移只建结构，不回填任何历史数据（新规则从上线日起生效）。实际上也回填不了：
-- audit_log 里的信用代码是脱敏存的，历史请求查过哪家企业已无法还原。
--
-- 关于明文存储 credit_code 的合规性（设计文档 §8.1）：统一社会信用代码是《组织机构
-- 统一社会信用代码管理办法》第三条所指、赋予法人/非法人组织/个体工商户的组织身份
-- 标识码，不属于《个人信息保护法》第四条界定的自然人个人信息，且依《企业信息公示
-- 暂行条例》本身即为法定公开信息。约束是：不要在同一张表里把它与法定代表人的身份证
-- 号/手机号并列。下面三张表只有代码 + 时间 + 计数，不含任何自然人字段。
--
-- 三张表均【不建到 license 的外键】：后台删除客户不应因计费历史而失败或级联删除，
-- 已发生的计费是对账凭证，客户注销后仍需可查。

-- 1. billing_coverage：免费窗口状态（每客户每主体每类目一行，UPSERT 维护）
--    计费主体用 license_id 而非 app_key：app_key 会因重建账号而变，一变则该客户的
--    全部免费期凭空消失，客户视角就是重复收费。
CREATE TABLE IF NOT EXISTS billing_coverage (
    license_id        VARCHAR(64)  NOT NULL,
    route             VARCHAR(16)  NOT NULL DEFAULT 'swfp',
    credit_code       VARCHAR(32)  NOT NULL,
    category          VARCHAR(8)   NOT NULL,                -- invoice | tax
    first_charged_at  TIMESTAMPTZ  NOT NULL DEFAULT now(),   -- 历史首次计费，续期不改
    charged_at        TIMESTAMPTZ  NOT NULL DEFAULT now(),   -- 本轮计费时刻 = 窗口起点
    expires_at        TIMESTAMPTZ  NOT NULL,                 -- 免费期截止（不含）
    charge_count      INT          NOT NULL DEFAULT 1,       -- 该主体该类目累计计费轮数
    free_hits         BIGINT       NOT NULL DEFAULT 0,       -- 本轮窗口内的免费命中次数
    last_hit_at       TIMESTAMPTZ,                           -- 最近一次查得（含免费）
    charge_reqid      VARCHAR(32)  NOT NULL DEFAULT '',      -- 本轮计费那次请求的 reqid
    charge_request_id VARCHAR(64)  NOT NULL DEFAULT '',
    updated_at        TIMESTAMPTZ  NOT NULL DEFAULT now(),
    PRIMARY KEY (license_id, route, credit_code, category)
);

-- 「列出我当前还在免费期的主体」：对客自查接口 + 后台免费期页
CREATE INDEX IF NOT EXISTS idx_coverage_active ON billing_coverage (license_id, route, expires_at);
-- 跨客户按主体排查（后台：这家企业都有谁查过）
CREATE INDEX IF NOT EXISTS idx_coverage_code ON billing_coverage (credit_code);

-- 2. billing_charge：计费事件流水（追加写，只记产生应收的请求）
--    一次请求一行、不是一个类目一行：both 是一个打包价 bothFen 而非两个单价之和，
--    硬拆到类目行上会产生任意分摊，对账反而说不清。用两组布尔列表达「这一笔为哪些
--    类目收了钱」，既能按类目统计又不必编造分摊金额。
--    window_to 是快照：billing_coverage 会被续期覆盖，事件表必须自己留一份，否则
--    一年后无法回答「当时开的窗口到哪天」。
CREATE TABLE IF NOT EXISTS billing_charge (
    id                BIGSERIAL    PRIMARY KEY,
    license_id        VARCHAR(64)  NOT NULL,
    app_key           VARCHAR(64)  NOT NULL DEFAULT '',      -- 冗余，出账不必 join license
    route             VARCHAR(16)  NOT NULL DEFAULT 'swfp',
    credit_code       VARCHAR(32)  NOT NULL,
    reqid             VARCHAR(32)  NOT NULL,
    request_id        VARCHAR(64)  NOT NULL DEFAULT '',
    ledger_id         BIGINT,                                -- 指向 billing_ledger.id
    charged_invoice   BOOLEAN      NOT NULL DEFAULT FALSE,   -- 本次为发票类目收了钱
    charged_tax       BOOLEAN      NOT NULL DEFAULT FALSE,   -- 本次为税务类目收了钱
    covered_invoice   BOOLEAN      NOT NULL DEFAULT FALSE,   -- 本次发票命中免费期
    covered_tax       BOOLEAN      NOT NULL DEFAULT FALSE,
    fee_standard      VARCHAR(16)  NOT NULL DEFAULT '',      -- both | invoice | tax
    amount_fen        BIGINT       NOT NULL DEFAULT 0,
    upstream_cost_fen BIGINT       NOT NULL DEFAULT 0,
    kind              VARCHAR(8)   NOT NULL DEFAULT '',      -- first | renew | mixed
    window_to         TIMESTAMPTZ,                           -- 本次开启/续期的窗口截止
    created_at        TIMESTAMPTZ  NOT NULL DEFAULT now(),
    CONSTRAINT uq_charge UNIQUE (license_id, route, reqid)
);

CREATE INDEX IF NOT EXISTS idx_charge_license_time ON billing_charge (license_id, created_at);
CREATE INDEX IF NOT EXISTS idx_charge_code ON billing_charge (credit_code);

-- 3. upstream_coverage：上游成本窗口（成本侧，本轮只建表，逻辑待配真实单价后接入）
--    上游对我们同为主体年度计费，故键【不含 license_id】：上游按主体收我们的钱，
--    不管是我们哪个客户问的。由此同一主体年度里：上游成本付 1 次、下游收入收 N 次
--    （N = 查过这家企业的客户数），毛利随客户重叠度增长。
--
--    没有这张表，配上真实 costFen 之后重复查询会虚增成本：Sourcer 目前对每个
--    status=ok 的调用无条件记 Call.CostFen，upstream_call.cost_fen 与台账的
--    upstream_cost_fen 会全部偏高，且偏得没规律。
--
--    粒度选 source_label 而非 category：五个源 1:1 对应五个上游产品码
--    （P0130081/P0130083/P0130082/P0130084 + 销项），上游按产品码结算。
CREATE TABLE IF NOT EXISTS upstream_coverage (
    provider      VARCHAR(32)  NOT NULL,                     -- entcredit | salesdata
    source_label  VARCHAR(32)  NOT NULL,                     -- invoice1/invoice2/tax1/tax2/sales
    credit_code   VARCHAR(32)  NOT NULL,
    charged_at    TIMESTAMPTZ  NOT NULL DEFAULT now(),
    expires_at    TIMESTAMPTZ  NOT NULL,
    charge_count  INT          NOT NULL DEFAULT 1,
    free_hits     BIGINT       NOT NULL DEFAULT 0,           -- 窗口内的免费复用次数 = 省下的钱
    cost_fen      BIGINT       NOT NULL DEFAULT 0,           -- 本轮实付单价快照
    charge_reqid  VARCHAR(32)  NOT NULL DEFAULT '',
    last_hit_at   TIMESTAMPTZ,
    updated_at    TIMESTAMPTZ  NOT NULL DEFAULT now(),
    PRIMARY KEY (provider, source_label, credit_code)
);

CREATE INDEX IF NOT EXISTS idx_upstream_coverage_expires ON upstream_coverage (expires_at);

-- 4. billing_ledger：把「本次为什么收/没收钱」落在台账上，无需 join 即可解释一行
ALTER TABLE billing_ledger ADD COLUMN IF NOT EXISTS credit_code   VARCHAR(32) NOT NULL DEFAULT '';
ALTER TABLE billing_ledger ADD COLUMN IF NOT EXISTS charged_scope VARCHAR(16) NOT NULL DEFAULT '';
ALTER TABLE billing_ledger ADD COLUMN IF NOT EXISTS covered_scope VARCHAR(16) NOT NULL DEFAULT '';
ALTER TABLE billing_ledger ADD COLUMN IF NOT EXISTS charge_state  VARCHAR(16) NOT NULL DEFAULT '';

CREATE INDEX IF NOT EXISTS idx_ledger_credit_code ON billing_ledger (credit_code);

-- 5. audit_log：同上四列。注意 billed 的语义随本次改动而变——它原先等于 found_data
--    （都来自 busiCode 10，完全重复），新口径下表示「本次是否真的收了钱」
--    (charge_state = CHARGED)。found_data 保持「是否查得数据」不变，于是同一行里
--    found_data=true 且 billed=false 就是一次免费期命中。这正是「计费与查得统计
--    彻底分开」的落地形态。
ALTER TABLE audit_log ADD COLUMN IF NOT EXISTS credit_code   VARCHAR(32) NOT NULL DEFAULT '';
ALTER TABLE audit_log ADD COLUMN IF NOT EXISTS charged_scope VARCHAR(16) NOT NULL DEFAULT '';
ALTER TABLE audit_log ADD COLUMN IF NOT EXISTS covered_scope VARCHAR(16) NOT NULL DEFAULT '';
ALTER TABLE audit_log ADD COLUMN IF NOT EXISTS charge_state  VARCHAR(16) NOT NULL DEFAULT '';

CREATE INDEX IF NOT EXISTS idx_audit_credit_code ON audit_log (credit_code);

-- 6. upstream_call：区分「没花钱是因为没调用」与「调了但在上游免费期内」
ALTER TABLE upstream_call ADD COLUMN IF NOT EXISTS cost_covered BOOLEAN NOT NULL DEFAULT FALSE;
