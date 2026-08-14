# 计费护栏（Billing Guard）

一个"外挂式"计费拦截组件，位于 `backend/internal/billingguard`。它在**请求计费**的最后一公里拦截异常倍率：
当一笔请求的有效倍率异常（默认 >1000x）时，直接丢弃这笔计费记录——**不写 usage_log、不扣任何费用**
（余额 / 订阅用量 / APIKey 配额 / 账号配额 / 平台配额全部跳过），只输出 ALERT 日志并累计指标。

## 背景

倍率是计费链路上风险最高的单一乘数。一旦分组倍率、用户专属倍率、高峰倍率或账号倍率被错误配置
（或数据库被篡改、上游返回异常 usage 放大），一个普通请求就可能产生数百上千倍的扣费，
几分钟内扣光用户余额。本组件在所有计费落库路径的前置点统一设卡：

| 计费路径 | 挂载点 | 拦截效果 |
| --- | --- | --- |
| Anthropic / Gemini / 其它平台 token、图片、语音计费 | `GatewayService.recordUsageCore`（`service/gateway_usage_billing.go`） | 不写 usage_log、不扣费 |
| OpenAI / Grok token、图片、视频、语音计费 | `OpenAIGatewayService.RecordUsage`（`service/openai_gateway_usage.go`） | 不写 usage_log、不扣费 |
| 批量图片任务定价（hold） | `BatchImagePublicService.resolvePricingSnapshot`（`service/batch_image_public.go`） | 拒绝创建任务、不冻结余额、不写 batch_image_jobs |

组件在 `cmd/server/main.go` 启动时通过 `billingguard.Configure(billingguard.LoadFromEnv())` 装配，
并打印生效配置。组件**自包含**（仅依赖标准库），不依赖任何业务包，随时可通过
`SUB2API_BILLING_GUARD_ENABLED=false` 整体关闭或直接删除挂载点摘除。

## 检查规则

对每一笔即将计费的记录检查（命中任一即拦截）：

1. **名义倍率**：token 计费倍率（已含高峰因子）`> max_rate_multiplier` → 拦截；
2. **账号倍率**：账号级倍率（作用于账号配额消耗）`> max_account_rate_multiplier` → 拦截；
3. **比率兜底**：`ActualCost / TotalCost` 即实际生效倍率，捕捉图片/视频按次倍率、
   搜索附加费、长上下文叠加等未显式列出的倍率来源，`> max_rate_multiplier` → 拦截；
4. **单笔金额上限**（可选，默认关闭）：`ActualCost > max_absolute_cost_usd` → 拦截；
5. NaN / ±Inf 倍率一律拦截（视为数据损坏）。

放行语义：倍率**恰好等于**阈值放行；0 / 负值倍率放行（免费分组或上游已钳制）。

## 配置（环境变量）

| 环境变量 | 默认值 | 说明 |
| --- | --- | --- |
| `SUB2API_BILLING_GUARD_ENABLED` | `true` | 总开关；关闭后组件完全不参与 |
| `SUB2API_BILLING_GUARD_MAX_RATE_MULTIPLIER` | `1000` | 倍率上限（> 该值拦截） |
| `SUB2API_BILLING_GUARD_MAX_ACCOUNT_RATE_MULTIPLIER` | `1000` | 账号倍率上限 |
| `SUB2API_BILLING_GUARD_MAX_ABSOLUTE_COST_USD` | `0` | 单笔金额上限；`<= 0` 关闭 |
| `SUB2API_BILLING_GUARD_OBSERVE_ONLY` | `false` | `true` 时只告警不拦截（灰度观察用） |

建议上线顺序：先 `OBSERVE_ONLY=true` 观察告警频率，确认无误报后再切到拦截模式。

## 拦截时的行为

- 输出 `slog` ERROR 日志，关键词 `billingguard: blocked`，携带 `reason / path / request_id / model / user_id / api_key_id / account_id / rate_multiplier / account_rate_multiplier / total_cost / actual_cost` 字段；
- **写入拦截事件日志表 `billing_guard_logs`**（见下节），供复盘"是否拦截成功"；
- 计数器 `billingguard.Stats()` 返回 (拦截次数, 观察告警次数)，可接入 ops 面板做斜率告警；
- `RecordUsage` 返回 `billingguard.ErrBlocked` 哨兵错误（可用 `errors.Is` 识别），调用方仅记录日志。

注意：拦截发生在**上游响应之后**（后付费模型），对客户端完全透明——客户端仍收到正常响应，
只是这笔流量不被计费。这正符合"倍率异常时宁可少收、不可乱扣"的诉求。

## 拦截异常计费事件日志表（billing_guard_logs）

每次拦截（`action=blocked`）或观察告警（`action=observed`，OBSERVE_ONLY 灰度模式）都会
**异步**写入一行审计日志（5s 超时、fire-and-forget）：写入失败只告警并累计
`service.BillingGuardLogInsertStats()`，绝不影响计费主流程。

字段：`created_at`（发生时间）、`action`、`path`（gateway/openai/batch_image）、
`request_id`、`model`、`user_id`、`api_key_id`、`account_id`、
`rate_multiplier`（名义 token 倍率）、`account_rate_multiplier`（账号倍率）、
`total_cost`（倍率前成本）、`actual_cost`（倍率后成本）、`reason`（拦截原因明细）。

表由迁移 `backend/migrations/222_billing_guard_logs.sql` 创建，**服务启动时自动执行迁移**，
无需手工操作。

复盘常用 SQL（psql）：

    -- 最近 20 条拦截/观察事件
    SELECT created_at, action, path, model, user_id, api_key_id,
           rate_multiplier, account_rate_multiplier, actual_cost, reason
    FROM billing_guard_logs ORDER BY created_at DESC LIMIT 20;

    -- 每日拦截次数
    SELECT created_at::date AS day, action, count(*)
    FROM billing_guard_logs GROUP BY 1, 2 ORDER BY 1 DESC;

    -- 按 APIKey 汇总（谁最常触发）
    SELECT api_key_id, count(*), max(rate_multiplier)
    FROM billing_guard_logs GROUP BY 1 ORDER BY 2 DESC;

## 测试

- 组件单测：`backend/internal/billingguard/billingguard_test.go`（阈值边界、NaN/Inf、比率兜底、观察模式、环境变量解析）；
- 挂载点测试：`service/gateway_record_usage_test.go`、`service/openai_gateway_record_usage_test.go`、`service/batch_image_public_test.go` 中的 `*_BillingGuard*` 用例，
  断言被拦截时 usage_log 与扣费仓库均零调用。

运行：

    cd backend
    go test -tags=unit ./internal/billingguard/...
    go test -tags=unit ./internal/service/ -run 'BillingGuard'

## 扩展

- 新计费路径接入：在写 usage_log / 执行扣费之前调用三行 `billingguard.CheckAndBlock(billingguard.Event{...})` 即可；
- 管理端写入校验（可选）：在分组倍率 / 账号倍率的 admin 接口复用同一阈值拒绝异常配置，
  从源头避免脏数据（当前组件只做计费时拦截，不做配置时拦截）。
