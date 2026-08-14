-- billing_guard_logs：计费护栏（billingguard）拦截异常计费事件日志表。
--
-- 每行记录一次被拦截（blocked）或仅观察告警（observed）的异常计费事件，
-- 用于复盘「护栏是否拦截成功」。与 usage_logs 严格分离：
-- 被拦截的计费不会写入 usage_logs、不产生任何扣费，只在本表留审计痕迹。
--
-- 写入由代码在拦截时异步执行（5s 超时），失败仅告警、绝不影响计费主流程。
CREATE TABLE IF NOT EXISTS billing_guard_logs (
    id BIGSERIAL PRIMARY KEY,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    action TEXT NOT NULL DEFAULT 'blocked',  -- blocked=已拦截; observed=仅观察告警（OBSERVE_ONLY 模式）
    path TEXT NOT NULL DEFAULT '',           -- 计费来源：gateway / openai / batch_image
    request_id TEXT NOT NULL DEFAULT '',
    model TEXT NOT NULL DEFAULT '',
    user_id BIGINT NOT NULL DEFAULT 0,
    api_key_id BIGINT NOT NULL DEFAULT 0,
    account_id BIGINT NOT NULL DEFAULT 0,
    rate_multiplier DOUBLE PRECISION NOT NULL DEFAULT 0,          -- 名义 token 倍率（含高峰因子）
    account_rate_multiplier DOUBLE PRECISION NOT NULL DEFAULT 0,  -- 账号倍率
    total_cost DOUBLE PRECISION NOT NULL DEFAULT 0,               -- 倍率前成本（USD）
    actual_cost DOUBLE PRECISION NOT NULL DEFAULT 0,              -- 倍率后成本（USD）
    reason TEXT NOT NULL DEFAULT ''                               -- 拦截原因明细
);

CREATE INDEX IF NOT EXISTS idx_billing_guard_logs_created_at
    ON billing_guard_logs (created_at DESC);
CREATE INDEX IF NOT EXISTS idx_billing_guard_logs_api_key_id
    ON billing_guard_logs (api_key_id);

COMMENT ON TABLE billing_guard_logs IS
    '计费护栏拦截异常计费事件日志：仅审计用途，被拦截的计费不落 usage_logs、不扣费';
COMMENT ON COLUMN billing_guard_logs.action IS
    'blocked=已拦截（计费记录被丢弃）; observed=仅观察告警（OBSERVE_ONLY 灰度模式，计费照常）';
