package service

import (
	"context"
	"database/sql"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/billingguard"
)

// billingGuardLogInsertTimeout 是审计日志单次写入的超时。
// 拦截路径本身已 fire-and-forget，这里再兜一层超时，保证 DB 抖动绝不拖慢计费主流程。
const billingGuardLogInsertTimeout = 5 * time.Second

// billingGuardLogInsertErrorTotal 统计审计日志写入失败次数。
// 失败意味着该次拦截事件在 DB 中缺失（ALERT 日志仍在），监控可按斜率告警。
var billingGuardLogInsertErrorTotal atomic.Int64

// BillingGuardLogInsertStats 返回审计日志写入失败累计次数。
func BillingGuardLogInsertStats() int64 {
	return billingGuardLogInsertErrorTotal.Load()
}

// BillingGuardLogRecorder 把护栏拦截事件写入 billing_guard_logs（拦截异常计费事件日志表）。
// 实现 billingguard.Recorder：每次拦截（blocked）/观察告警（observed）都会异步回调本方法。
// 写入失败仅累计指标 + WARN 日志，绝不影响计费主流程。
type BillingGuardLogRecorder struct {
	db *sql.DB
}

// NewBillingGuardLogRecorder 构造审计日志记录器；db 为 nil 时退化为空操作。
func NewBillingGuardLogRecorder(db *sql.DB) *BillingGuardLogRecorder {
	return &BillingGuardLogRecorder{db: db}
}

const insertBillingGuardLogSQL = `
INSERT INTO billing_guard_logs
    (action, path, request_id, model, user_id, api_key_id, account_id,
     rate_multiplier, account_rate_multiplier, total_cost, actual_cost, reason)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`

// RecordEvent 实现 billingguard.Recorder。
func (r *BillingGuardLogRecorder) RecordEvent(ctx context.Context, ev billingguard.Event, action, reason string) {
	if r == nil || r.db == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, billingGuardLogInsertTimeout)
	defer cancel()

	if _, err := r.db.ExecContext(ctx, insertBillingGuardLogSQL,
		action,
		ev.Path,
		strings.TrimSpace(ev.RequestID),
		ev.Model,
		ev.UserID,
		ev.APIKeyID,
		ev.AccountID,
		ev.RateMultiplier,
		ev.AccountRateMultiplier,
		ev.TotalCost,
		ev.ActualCost,
		reason,
	); err != nil {
		billingGuardLogInsertErrorTotal.Add(1)
		slog.Warn("billingguard: insert billing_guard_logs failed (audit only, billing unaffected)",
			"error", err,
			"action", action,
			"path", ev.Path,
			"request_id", ev.RequestID,
			"user_id", ev.UserID,
			"api_key_id", ev.APIKeyID,
		)
	}
}
