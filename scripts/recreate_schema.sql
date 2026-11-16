-- v0.7 重建脚本：删除旧表，让 relay 启动时按 migrations/ 重新建表 + 注入 demo。
-- 用法（对每个目标数据库各执行一次）：
--   dev : 连到 dev_db  执行本脚本，再用 CONFIG_FILE=config.aliyun.e2e.yaml  启动 relay
--   prod: 先 CREATE DATABASE prod_db; 连到 prod_db 执行本脚本(可选，新库无旧表)，
--         再用 CONFIG_FILE=config.aliyun.prod.yaml 启动 relay
--
-- 删除顺序遵循外键依赖；CASCADE 兜底。schema_migrations 一并清空，
-- 这样 relay 的 ApplyMigrations 会把新 schema 完整重跑一遍。

DROP TABLE IF EXISTS billing_ledger      CASCADE;
DROP TABLE IF EXISTS audit_log           CASCADE;
DROP TABLE IF EXISTS quota               CASCADE;
DROP TABLE IF EXISTS license             CASCADE;
DROP TABLE IF EXISTS admin_user          CASCADE;
DROP TABLE IF EXISTS ip_whitelist_global CASCADE;  -- v0.7 已废弃
DROP TABLE IF EXISTS upstream_allowlist  CASCADE;
DROP TABLE IF EXISTS system_setting      CASCADE;
DROP TABLE IF EXISTS schema_migrations   CASCADE;
