-- 0009：客户级接口开关 — 每个客户单独开放 / 关闭发票、税务接口；票税 = 两项都开，不单独存。
--
-- 存量客户：ADD COLUMN 时按 DEFAULT TRUE 填充，上线后行为不变（不会被拦截）。
-- 新建客户：随后把列默认值改为 FALSE，后台新建的客户两项都关闭，需要在后台手动开通。
-- （CreateUser 会显式写入这两列；这里改默认值是兜底，防止其他写入路径漏写时被默认开通。）

ALTER TABLE license ADD COLUMN IF NOT EXISTS allow_invoice BOOLEAN NOT NULL DEFAULT TRUE;
ALTER TABLE license ADD COLUMN IF NOT EXISTS allow_tax     BOOLEAN NOT NULL DEFAULT TRUE;

ALTER TABLE license ALTER COLUMN allow_invoice SET DEFAULT FALSE;
ALTER TABLE license ALTER COLUMN allow_tax     SET DEFAULT FALSE;
