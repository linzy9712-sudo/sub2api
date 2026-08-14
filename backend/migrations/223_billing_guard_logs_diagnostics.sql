-- billing_guard_logs 增加诊断列：
--   group_id              —— 拦截事件关联的分组（定位哪一组的倍率异常）
--   multiplier_breakdown  —— 倍率来源拆解（system_default/group_default/user_rate/
--                            peak/image_rate/video_rate/account_rate 各层实际取值），
--                            用于回答「异常倍率到底来自哪一层」以及判断缓存是否陈旧。
ALTER TABLE billing_guard_logs ADD COLUMN IF NOT EXISTS group_id BIGINT NOT NULL DEFAULT 0;
ALTER TABLE billing_guard_logs ADD COLUMN IF NOT EXISTS multiplier_breakdown TEXT NOT NULL DEFAULT '';

CREATE INDEX IF NOT EXISTS idx_billing_guard_logs_group_id
    ON billing_guard_logs (group_id);

COMMENT ON COLUMN billing_guard_logs.group_id IS
    '拦截事件关联的分组 ID（0 表示无分组）';
COMMENT ON COLUMN billing_guard_logs.multiplier_breakdown IS
    '倍率来源拆解：system_default/group_default/user_rate/peak/image_rate/video_rate/account_rate 各层实际取值';
