// Package billingguard 是一个自包含的"外挂式"计费护栏组件。
//
// 它在请求计费（RecordUsage / 批量任务定价）的最前端拦截异常倍率：
//   - 倍率（组/用户/高峰等叠加后的 token 倍率、账号倍率）超过阈值，例如 >1000x；
//   - 单笔 ActualCost 超过绝对金额上限（可选，默认关闭）。
//
// 命中拦截时：不写 usage_log、不执行任何扣费（余额/订阅/APIKey 配额/账号配额），
// 只发 ALERT 日志并累计指标。批量图片任务在定价（hold）阶段即被拒绝，任务与
// 冻结记录都不会落库。
//
// 组件不依赖项目内其它包（仅标准库），通过环境变量自配置：
//
//	SUB2API_BILLING_GUARD_ENABLED                  （默认 true）
//	SUB2API_BILLING_GUARD_MAX_RATE_MULTIPLIER      （默认 1000）
//	SUB2API_BILLING_GUARD_MAX_ACCOUNT_RATE_MULTIPLIER（默认 1000）
//	SUB2API_BILLING_GUARD_MAX_ABSOLUTE_COST_USD    （默认 0 = 关闭）
//	SUB2API_BILLING_GUARD_OBSERVE_ONLY             （默认 false；true 只告警不拦截，用于灰度观察）
//
// 启动时用 billingguard.Configure(billingguard.LoadFromEnv()) 显式装配；
// 未装配时首次 Check 会自动从环境变量加载。测试可用 Configure 覆盖全局配置。
package billingguard

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// 环境变量名。
const (
	EnvEnabled                  = "SUB2API_BILLING_GUARD_ENABLED"
	EnvMaxRateMultiplier        = "SUB2API_BILLING_GUARD_MAX_RATE_MULTIPLIER"
	EnvMaxAccountRateMultiplier = "SUB2API_BILLING_GUARD_MAX_ACCOUNT_RATE_MULTIPLIER"
	EnvMaxAbsoluteCostUSD       = "SUB2API_BILLING_GUARD_MAX_ABSOLUTE_COST_USD"
	EnvValidateWrites           = "SUB2API_BILLING_GUARD_VALIDATE_WRITES"
	EnvObserveOnly              = "SUB2API_BILLING_GUARD_OBSERVE_ONLY"
	EnvLogAboveMultiplier       = "SUB2API_BILLING_GUARD_LOG_ABOVE_MULTIPLIER"
	EnvLogIntervalSeconds       = "SUB2API_BILLING_GUARD_LOG_INTERVAL_SECONDS"
)

// LogTag 是护栏所有日志的统一前缀标签（journald 消息行内可见）：
// 形如 "[BillingGuard] suspicious user group rate ..."。
// 查询：journalctl -u sub2api | grep -i billingguard（或 grep '[BillingGuard]'）。
const LogTag = "[BillingGuard] "

const (
	// DefaultMaxRateMultiplier 是组/用户级倍率默认上限：超过即视为异常
	// （配置错误、恶意篡改或上游返回异常 usage 导致的放大）。
	DefaultMaxRateMultiplier = 1000
	// DefaultMaxAccountRateMultiplier 是账号计费倍率默认上限。
	DefaultMaxAccountRateMultiplier = 1000
	// DefaultLogAboveMultiplier 是可疑倍率日志阈值：解析/写入/计费时发现
	// 倍率超过该值（但未达拦截阈值）即输出 WARN 日志，供复盘倍率来源。
	DefaultLogAboveMultiplier = 10
)

// ratioEpsilon 是 ActualCost/TotalCost 比率检查的相对容差，吸收浮点尾数噪声，
// 避免倍率恰好等于阈值时因 1000.0000000001 这类误差误拦。
const ratioEpsilon = 1e-9

// ErrBlocked 是护栏拦截时返回的哨兵错误；调用方可用 errors.Is 识别。
var ErrBlocked = errors.New("billing guard blocked abnormal billing record")

// Event 描述一笔即将计费（写 usage_log / 扣费 / 批量任务 hold）的请求快照。
type Event struct {
	Path      string // 计费来源："gateway" / "openai" / "batch_image"
	RequestID string
	Model     string
	UserID    int64
	APIKeyID  int64
	AccountID int64
	GroupID   int64 // 关联分组（0 表示无）

	// Breakdown 是倍率来源拆解（诊断用）：记录系统默认/分组默认/用户专属覆盖/
	// 高峰因子/图片/视频独立倍率/账号倍率各层的取值，用于回答
	// 「异常倍率到底来自哪一层」。仅写入日志与审计表，不参与判定。
	Breakdown string

	// RateMultiplier 是本次名义 token 倍率（已含高峰因子）。
	RateMultiplier float64
	// AccountRateMultiplier 是账号级倍率，作用于账号配额消耗。
	AccountRateMultiplier float64

	// TotalCost / ActualCost：倍率前成本与倍率后成本。
	// ActualCost/TotalCost 即为实际生效倍率，护栏用该比率兜底捕捉任何
	// 未显式列出的倍率来源（图片/视频按次倍率、搜索附加费、长上下文叠加等）。
	TotalCost  float64
	ActualCost float64

	// —— 计费取证字段：写入完整日志（journald），便于不查库也能还原现场 ——
	InputTokens         int
	OutputTokens        int
	CacheCreationTokens int
	CacheReadTokens     int
	ImageCount          int
	SearchCount         int
	VideoCount          int
	UpstreamModel       string // 实际转发到上游的模型
	BillingModel        string // 定价使用的模型
	ServiceTier         string // OpenAI Responses 服务档位（priority/flex）
	ReasoningEffort     string
	InboundEndpoint     string // 客户端请求路径
	UpstreamEndpoint    string // 上游实际路径
	PricingAt           string // 定价时刻（RFC3339）
	Stream              bool
}

// Decision 是护栏判定结果。
type Decision struct {
	Block  bool
	Reason string
}

// Config 是护栏配置。
type Config struct {
	// Enabled 关闭后护栏完全不参与（不检查、不记录、不拦截）。
	Enabled bool
	// MaxRateMultiplier token/按次计费倍率上限；倍率 > 该值即拦截。
	MaxRateMultiplier float64
	// MaxAccountRateMultiplier 账号计费倍率上限。
	MaxAccountRateMultiplier float64
	// MaxAbsoluteCostUSD 单笔 ActualCost 上限（USD）；<= 0 表示不启用。
	MaxAbsoluteCostUSD float64
	// ObserveOnly 只告警不拦截，用于上线前灰度观察。
	ObserveOnly bool
	// ValidateWrites 在管理端写入倍率时校验上限（默认 true）：
	// 超过阈值的分组/用户/账号倍率直接拒绝落库，从源头避免脏数据。
	// 需要临时写入超限值做测试时置 false（如构造 2000x 测试分组）。
	ValidateWrites bool
	// LogAboveMultiplier 是可疑倍率日志阈值：解析/写入/计费时发现倍率
	// 超过该值（未达拦截阈值）即输出 WARN 日志，用于复盘倍率来源；
	// <= 0 关闭。默认 10。
	LogAboveMultiplier float64
	// LogIntervalSeconds 是可疑/拦截日志的节流间隔：同一「路径:用户:分组」
	// 每间隔最多输出 1 条 journald 日志，其余计入 suppressed 计数；
	// <= 0 不节流（每条都输出）。默认 60。正常请求（倍率 <= 阈值）零日志。
	LogIntervalSeconds int
}

// DefaultConfig 返回默认配置：启用、倍率上限 1000、单笔金额上限关闭。
func DefaultConfig() Config {
	return Config{
		Enabled:                  true,
		MaxRateMultiplier:        DefaultMaxRateMultiplier,
		MaxAccountRateMultiplier: DefaultMaxAccountRateMultiplier,
		MaxAbsoluteCostUSD:       0,
		ObserveOnly:              false,
		ValidateWrites:           true,
		LogAboveMultiplier:       DefaultLogAboveMultiplier,
		LogIntervalSeconds:       60,
	}
}

// normalized 清洗非法配置值（NaN/Inf/非正阈值回落默认），保证护栏语义稳定。
func (c Config) normalized() Config {
	if !finitePositive(c.MaxRateMultiplier) {
		c.MaxRateMultiplier = DefaultMaxRateMultiplier
	}
	if !finitePositive(c.MaxAccountRateMultiplier) {
		c.MaxAccountRateMultiplier = DefaultMaxAccountRateMultiplier
	}
	if math.IsNaN(c.MaxAbsoluteCostUSD) || math.IsInf(c.MaxAbsoluteCostUSD, 0) || c.MaxAbsoluteCostUSD < 0 {
		c.MaxAbsoluteCostUSD = 0
	}
	if math.IsNaN(c.LogAboveMultiplier) || math.IsInf(c.LogAboveMultiplier, 0) {
		c.LogAboveMultiplier = DefaultLogAboveMultiplier
	}
	if c.LogIntervalSeconds < 0 {
		c.LogIntervalSeconds = 60
	}
	return c
}

var (
	loadOnce sync.Once
	mu       sync.RWMutex
	current  = DefaultConfig()

	blockedTotal  atomic.Int64
	observedTotal atomic.Int64
)

// Configure 装配护栏配置（幂等；调用后环境变量不再自动加载）。
func Configure(cfg Config) {
	mu.Lock()
	current = cfg.normalized()
	mu.Unlock()
	// 标记已加载：显式装配优先于懒加载。
	loadOnce.Do(func() {})
}

// Current 返回当前生效配置（已归一化）。
func Current() Config {
	ensureLoaded()
	mu.RLock()
	defer mu.RUnlock()
	return current
}

// Stats 返回 (拦截次数, 观察告警次数)。
func Stats() (blocked, observed int64) {
	return blockedTotal.Load(), observedTotal.Load()
}

// ensureLoaded 首次使用时从环境变量懒加载配置。
func ensureLoaded() {
	loadOnce.Do(func() {
		cfg := LoadFromEnv()
		mu.Lock()
		current = cfg.normalized()
		mu.Unlock()
	})
}

// LoadFromEnv 从环境变量构造配置：未设置的变量保留默认值；解析失败告警并保留默认。
func LoadFromEnv() Config {
	cfg := DefaultConfig()
	cfg.Enabled = envBool(EnvEnabled, cfg.Enabled)
	cfg.MaxRateMultiplier = envFloat(EnvMaxRateMultiplier, cfg.MaxRateMultiplier)
	cfg.MaxAccountRateMultiplier = envFloat(EnvMaxAccountRateMultiplier, cfg.MaxAccountRateMultiplier)
	cfg.MaxAbsoluteCostUSD = envFloat(EnvMaxAbsoluteCostUSD, cfg.MaxAbsoluteCostUSD)
	cfg.ObserveOnly = envBool(EnvObserveOnly, cfg.ObserveOnly)
	cfg.ValidateWrites = envBool(EnvValidateWrites, cfg.ValidateWrites)
	cfg.LogAboveMultiplier = envFloat(EnvLogAboveMultiplier, cfg.LogAboveMultiplier)
	cfg.LogIntervalSeconds = envInt(EnvLogIntervalSeconds, cfg.LogIntervalSeconds)
	return cfg
}

func envBool(key string, def bool) bool {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		slog.Warn(LogTag+"invalid env value, using default",
			"key", key, "value", v, "default", def)
		return def
	}
	return b
}

func envInt(key string, def int) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		slog.Warn("billingguard: invalid env value, using default",
			"key", key, "value", v, "default", def)
		return def
	}
	return n
}

func envFloat(key string, def float64) float64 {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		slog.Warn(LogTag+"invalid env value, using default",
			"key", key, "value", v, "default", def)
		return def
	}
	return f
}

func finitePositive(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0) && v > 0
}

func isFinite(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0)
}

// Check 评估一笔计费记录；命中异常返回 Decision{Block: true, Reason: ...}。
// 倍率 <= 阈值（含相等）与 0/负值（免费或上游已钳制）均放行。
func Check(ev Event) Decision {
	ensureLoaded()
	mu.RLock()
	cfg := current
	mu.RUnlock()

	if !cfg.Enabled {
		return Decision{}
	}

	var reasons []string
	appendReason := func(format string, args ...any) {
		reasons = append(reasons, fmt.Sprintf(format, args...))
	}

	// 名义 token 倍率：显式检查（NaN/Inf 一律拦截，视为数据损坏）。
	if rate := ev.RateMultiplier; !isFinite(rate) {
		appendReason("rate_multiplier is non-finite: %v", rate)
	} else if rate > cfg.MaxRateMultiplier {
		appendReason("rate_multiplier %.2f exceeds max %.2f", rate, cfg.MaxRateMultiplier)
	}

	// 账号倍率：作用于账号配额消耗。
	if acc := ev.AccountRateMultiplier; !isFinite(acc) {
		appendReason("account_rate_multiplier is non-finite: %v", acc)
	} else if acc > cfg.MaxAccountRateMultiplier {
		appendReason("account_rate_multiplier %.2f exceeds max %.2f", acc, cfg.MaxAccountRateMultiplier)
	}

	// 比率兜底：ActualCost/TotalCost 即实际生效倍率，捕捉任何未显式列出的
	// 倍率来源（图片/视频按次倍率、搜索附加费、长上下文叠加后的异常值）。
	if ev.TotalCost > 0 && ev.ActualCost > 0 {
		ratio := ev.ActualCost / ev.TotalCost
		if !isFinite(ratio) || ratio > cfg.MaxRateMultiplier*(1+ratioEpsilon) {
			appendReason("effective billed ratio %.6f exceeds max %.2f", ratio, cfg.MaxRateMultiplier)
		}
	}

	// 单笔绝对金额上限（可选）。
	if cfg.MaxAbsoluteCostUSD > 0 && ev.ActualCost > cfg.MaxAbsoluteCostUSD {
		appendReason("actual_cost %.8f exceeds max_absolute_cost_usd %.8f", ev.ActualCost, cfg.MaxAbsoluteCostUSD)
	}

	if len(reasons) == 0 {
		return Decision{}
	}
	return Decision{Block: true, Reason: strings.Join(reasons, "; ")}
}

// —— 日志节流：可疑/拦截日志按「路径:用户:分组」限频，正常请求零日志 ——

var (
	throttleMu         sync.Mutex
	throttleLastLog    = map[string]time.Time{}
	throttleSuppressed = map[string]int64{}
)

// ResetLogThrottle 清空节流状态（测试用；生产无需调用）。
func ResetLogThrottle() {
	throttleMu.Lock()
	throttleLastLog = map[string]time.Time{}
	throttleSuppressed = map[string]int64{}
	throttleMu.Unlock()
}

// LogThrottle 判断 key 是否允许本次输出日志：
// 同一 key 在 LogIntervalSeconds 内只放行一次，其余返回 false 并累计被抑制次数；
// 放行时返回此前被抑制的条数（供日志附带 suppressed_logs 字段）。
// LogIntervalSeconds <= 0 时永不节流。
func LogThrottle(key string) (emit bool, suppressed int64) {
	ensureLoaded()
	mu.RLock()
	interval := current.LogIntervalSeconds
	mu.RUnlock()
	if interval <= 0 {
		return true, 0
	}

	throttleMu.Lock()
	defer throttleMu.Unlock()
	now := time.Now()
	if last, ok := throttleLastLog[key]; ok && now.Sub(last) < time.Duration(interval)*time.Second {
		throttleSuppressed[key]++
		return false, throttleSuppressed[key]
	}
	suppressed = throttleSuppressed[key]
	throttleSuppressed[key] = 0
	throttleLastLog[key] = now
	// 防内存增长：key 数超限时整体清空（只影响节流精度，不影响正确性）
	if len(throttleLastLog) > 10000 {
		throttleLastLog = map[string]time.Time{}
		throttleSuppressed = map[string]int64{}
	}
	return true, suppressed
}

// ShouldLogMultiplier 判断倍率是否超过"可疑日志阈值"（未达拦截阈值）。
// 供解析/写入路径在发现可疑值时输出带来源的 WARN 日志。
func ShouldLogMultiplier(v float64) bool {
	if !isFinite(v) {
		return false
	}
	ensureLoaded()
	mu.RLock()
	threshold := current.LogAboveMultiplier
	enabled := current.Enabled
	mu.RUnlock()
	return enabled && threshold > 0 && v > threshold
}

// CheckAndBlock 是计费路径的统一挂载点：
//   - 未命中 / 组件关闭：返回 nil，透明放行；
//   - ObserveOnly：告警但不拦截，返回 nil；
//   - 拦截：累计指标、输出 ALERT 日志并返回 ErrBlocked 哨兵错误。
//
// 调用方拿到非 nil 错误时应直接中止计费流程：不写 usage_log、不执行任何扣费。
func CheckAndBlock(ev Event) error {
	decision := Check(ev)
	throttleKey := fmt.Sprintf("%s:%d:%d", ev.Path, ev.UserID, ev.GroupID)

	if !decision.Block {
		// 未达拦截阈值但超过日志阈值：记录"可疑倍率"WARN（含来源拆解），
		// 用于复盘倍率来源与趋势，不影响计费。按 key 节流，防止可疑流量刷爆日志。
		if ShouldLogMultiplier(ev.RateMultiplier) || ShouldLogMultiplier(ev.AccountRateMultiplier) {
			if emit, suppressed := LogThrottle(throttleKey); emit {
				attrs := guardAttrs(ev, "below block threshold")
				if suppressed > 0 {
					attrs = append(attrs, "suppressed_logs", suppressed)
				}
				slog.Warn(LogTag+"elevated multiplier observed (below block threshold)", attrs...)
			}
		}
		return nil
	}

	ensureLoaded()
	mu.RLock()
	observeOnly := current.ObserveOnly
	mu.RUnlock()

	if observeOnly {
		observedTotal.Add(1)
		if emit, suppressed := LogThrottle(throttleKey); emit {
			attrs := guardAttrs(ev, decision.Reason)
			if suppressed > 0 {
				attrs = append(attrs, "suppressed_logs", suppressed)
			}
			slog.Warn(LogTag+"abnormal billing observed (observe-only, not blocked)", attrs...)
		}
		notifyRecorder(ev, "observed", decision.Reason)
		return nil
	}

	blockedTotal.Add(1)
	// 拦截日志按 key 节流（同一用户同一分组每 60s 最多 1 条），
	// 每笔拦截仍会写审计表（billing_guard_logs）并计数，复盘不丢事件。
	if emit, suppressed := LogThrottle(throttleKey); emit {
		attrs := guardAttrs(ev, decision.Reason)
		if suppressed > 0 {
			attrs = append(attrs, "suppressed_logs", suppressed)
		}
		slog.Error(LogTag+"blocked abnormal billing record (usage log and quota deduction skipped)", attrs...)
	}
	notifyRecorder(ev, "blocked", decision.Reason)
	return NewBlockError(decision.Reason)
}

// guardAttrs 构造结构化日志属性，统一拦截与观察两条路径的字段。
func guardAttrs(ev Event, reason string) []any {
	return []any{
		"reason", reason,
		"path", ev.Path,
		"request_id", strings.TrimSpace(ev.RequestID),
		"model", ev.Model,
		"user_id", ev.UserID,
		"api_key_id", ev.APIKeyID,
		"account_id", ev.AccountID,
		"group_id", ev.GroupID,
		"multiplier_breakdown", ev.Breakdown,
		"rate_multiplier", ev.RateMultiplier,
		"account_rate_multiplier", ev.AccountRateMultiplier,
		"total_cost", ev.TotalCost,
		"actual_cost", ev.ActualCost,
		// 计费取证字段
		"input_tokens", ev.InputTokens,
		"output_tokens", ev.OutputTokens,
		"cache_creation_tokens", ev.CacheCreationTokens,
		"cache_read_tokens", ev.CacheReadTokens,
		"image_count", ev.ImageCount,
		"search_count", ev.SearchCount,
		"video_count", ev.VideoCount,
		"upstream_model", ev.UpstreamModel,
		"billing_model", ev.BillingModel,
		"service_tier", ev.ServiceTier,
		"reasoning_effort", ev.ReasoningEffort,
		"inbound_endpoint", ev.InboundEndpoint,
		"upstream_endpoint", ev.UpstreamEndpoint,
		"pricing_at", ev.PricingAt,
		"stream", ev.Stream,
	}
}

// NewBlockError 包装 ErrBlocked 哨兵错误并附带具体原因。
func NewBlockError(reason string) error {
	return fmt.Errorf("%w: %s", ErrBlocked, strings.TrimSpace(reason))
}

// —— 写入侧校验：管理端倍率写入口复用护栏阈值，从源头拒绝脏配置 ——

// ValidateWriteRateMultiplier 校验分组/用户级倍率写入：
// 必须 > 0，且不超过当前配置的 MaxRateMultiplier（ValidateWrites=false 时只校验 > 0）。
func ValidateWriteRateMultiplier(field string, v float64) error {
	if v <= 0 {
		return fmt.Errorf("%s must be > 0", field)
	}
	return validateRateMultiplierUpperBound(field, v, false)
}

// ValidateWriteRateMultiplierAllowZero 校验允许 0 的倍率写入（图片/视频独立倍率、高峰倍率）：
// 必须 >= 0，且不超过当前配置的 MaxRateMultiplier。
func ValidateWriteRateMultiplierAllowZero(field string, v float64) error {
	if v < 0 {
		return fmt.Errorf("%s must be >= 0", field)
	}
	return validateRateMultiplierUpperBound(field, v, false)
}

// ValidateWriteAccountRateMultiplier 校验账号倍率写入：
// 必须 >= 0（0 = 该账号计费为 0），且不超过 MaxAccountRateMultiplier。
func ValidateWriteAccountRateMultiplier(field string, v float64) error {
	if v < 0 {
		return fmt.Errorf("%s must be >= 0", field)
	}
	return validateRateMultiplierUpperBound(field, v, true)
}

// validateRateMultiplierUpperBound 读取当前护栏配置执行上限校验；
// ValidateWrites=false 时跳过上限（仍保留调用方的正负校验）。
func validateRateMultiplierUpperBound(field string, v float64, account bool) error {
	ensureLoaded()
	mu.RLock()
	validateWrites := current.ValidateWrites
	limit := current.MaxRateMultiplier
	if account {
		limit = current.MaxAccountRateMultiplier
	}
	mu.RUnlock()
	if validateWrites && v > limit {
		// 写入被拒：必然是可疑事件（正常写入不会触发），输出 WARN 供复盘。
		slog.Warn(LogTag+"rejected multiplier write above limit",
			"field", field, "value", v, "limit", limit)
		return fmt.Errorf("%s %.2f exceeds max %.2f (billing guard write limit)", field, v, limit)
	}
	return nil
}

// Recorder 是护栏审计日志记录器接口（可选装配）。
// 装配后，每次命中拦截（blocked）或观察告警（observed）都会回调 RecordEvent，
// 由实现把事件写入审计表（如 billing_guard_logs）。
//
// 实现约定：RecordEvent 必须快速、非阻塞（建议自带超时），
// 护栏以 fire-and-forget 方式调用并兜底 recover，任何失败都不影响计费主流程。
type Recorder interface {
	RecordEvent(ctx context.Context, ev Event, action, reason string)
}

var (
	recorderMu sync.RWMutex
	recorder   Recorder
)

// SetRecorder 装配审计记录器；传 nil 卸载。幂等，可随时替换。
func SetRecorder(r Recorder) {
	recorderMu.Lock()
	recorder = r
	recorderMu.Unlock()
}

// CurrentRecorder 返回当前装配的审计记录器（可能为 nil）。
func CurrentRecorder() Recorder {
	recorderMu.RLock()
	defer recorderMu.RUnlock()
	return recorder
}

// notifyRecorder 以 fire-and-forget 方式把事件交给审计记录器。
// 拦截属于低频路径，单 goroutine + recover 的开销可忽略；
// 记录器内部自带超时（实现约定），不会拖慢计费主流程。
func notifyRecorder(ev Event, action, reason string) {
	recorderMu.RLock()
	r := recorder
	recorderMu.RUnlock()
	if r == nil {
		return
	}
	go func() {
		defer func() {
			if p := recover(); p != nil {
				slog.Error(LogTag+"recorder panicked",
					"panic", p, "action", action, "request_id", ev.RequestID)
			}
		}()
		r.RecordEvent(context.Background(), ev, action, reason)
	}()
}
