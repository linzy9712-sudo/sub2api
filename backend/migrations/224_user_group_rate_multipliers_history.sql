-- user_group_rate_multipliers_history：用户专属倍率表变更审计（触发器驱动的追加日志表）。
--
-- 目的：抓住「写入→立刻删除」这类瞬态写入与绕过应用的直连 SQL。
--   - 每次 INSERT / UPDATE / DELETE 自动追加一行（含级联删除，如删用户/删分组连带清行）；
--   - 记录新旧值、操作类型、数据库会话用户（session_user）、应用名（application_name）
--     与客户端来源 IP（inet_client_addr），直接定位写入方；
--   - 只增不删（append-only），行被删后审计依然在。
CREATE TABLE IF NOT EXISTS user_group_rate_multipliers_history (
    id BIGSERIAL PRIMARY KEY,
    changed_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    operation TEXT NOT NULL,                     -- INSERT / UPDATE / DELETE
    user_id BIGINT,
    group_id BIGINT,
    old_rate_multiplier DECIMAL(10,4),
    new_rate_multiplier DECIMAL(10,4),
    session_user TEXT,
    application_name TEXT,
    client_addr TEXT
);

CREATE INDEX IF NOT EXISTS idx_user_group_rate_multipliers_history_changed_at
    ON user_group_rate_multipliers_history (changed_at DESC);
CREATE INDEX IF NOT EXISTS idx_user_group_rate_multipliers_history_user_group
    ON user_group_rate_multipliers_history (user_id, group_id);

CREATE OR REPLACE FUNCTION audit_user_group_rate_multipliers() RETURNS trigger AS $$
BEGIN
    INSERT INTO user_group_rate_multipliers_history
        (operation, user_id, group_id, old_rate_multiplier, new_rate_multiplier,
         session_user, application_name, client_addr)
    VALUES
        (TG_OP,
         COALESCE(NEW.user_id, OLD.user_id),
         COALESCE(NEW.group_id, OLD.group_id),
         CASE WHEN TG_OP IN ('UPDATE','DELETE') THEN OLD.rate_multiplier END,
         CASE WHEN TG_OP IN ('INSERT','UPDATE') THEN NEW.rate_multiplier END,
         SESSION_USER,
         current_setting('application_name', true),
         inet_client_addr()::text);
    RETURN COALESCE(NEW, OLD);
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_user_group_rate_multipliers_audit ON user_group_rate_multipliers;
CREATE TRIGGER trg_user_group_rate_multipliers_audit
    AFTER INSERT OR UPDATE OR DELETE ON user_group_rate_multipliers
    FOR EACH ROW EXECUTE FUNCTION audit_user_group_rate_multipliers();

COMMENT ON TABLE user_group_rate_multipliers_history IS
    '用户专属倍率表变更审计（触发器追加）：定位异常倍率的写入方';
