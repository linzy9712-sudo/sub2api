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
| `SUB2API_BILLING_GUARD_VALIDATE_WRITES` | `true` | 管理端写入倍率时校验上限（超限拒绝落库）；需要临时写入超限值做测试时置 `false` |

建议上线顺序：先 `OBSERVE_ONLY=true` 观察告警频率，确认无误报后再切到拦截模式。

**写入侧校验**：分组倍率、用户专属倍率（批量/单个）、图片/视频独立倍率、高峰倍率、
账号倍率的全部写入路径，与计费拦截共用同一阈值——超过上限的值会在落库前被拒绝，
从源头杜绝 105072x 这类脏数据（拦截面已缩小到"直接 SQL 改库"一种）。

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
`request_id`、`model`、`user_id`、`api_key_id`、`account_id`、`group_id`（关联分组）、
`rate_multiplier`（名义 token 倍率）、`account_rate_multiplier`（账号倍率）、
`total_cost`（倍率前成本）、`actual_cost`（倍率后成本）、`reason`（拦截原因明细）、
`multiplier_breakdown`（**倍率来源拆解**，见下节）。

表由迁移 `backend/migrations/222_billing_guard_logs.sql` 创建、`223_...diagnostics.sql` 扩展，
**服务启动时自动执行迁移**，无需手工操作。

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
## 诊断：异常倍率来自哪一层？

倍率是**多层叠加 + 多级缓存**的结果。拦截时各层实际取值会写进
`multiplier_breakdown` 字段（ALERT 日志里也有），格式如：

    group_default=2000,user_rate=1,peak=1,account_rate=1

各层含义与缓存：

| 层级 | 来源 | 缓存 |
| --- | --- | --- |
| `system_default` | config.yaml 的 `default.rate_multiplier` | 无（启动时加载） |
| `group_default` | `groups.rate_multiplier` | **APIKey 鉴权缓存快照**（L1 15s / L2 300s） |
| `user_rate` | `user_group_rate_multipliers.rate_multiplier`（用户专属覆盖） | 进程内解析缓存（默认 30s） |
| `peak` | `groups.peak_rate_*`（仅订阅类型分组，高峰时段生效） | 同上（Group 快照） |
| `image_rate` / `video_rate` | `groups.image/video_rate_*`（独立倍率开启时） | 同上（Group 快照） |
| `account_rate` | `accounts.rate_multiplier`（作用于账号配额） | 账号对象缓存 |

**拆解值与数据库对不上 = 缓存陈旧**（典型原因：直接 SQL 改库、恢复备份、迁移改表——
这些都不会触发缓存失效）。处理：重启服务立即清空全部进程内缓存；
Redis 鉴权缓存可等 5 分钟自然过期，或走管理后台正常修改一次该分组触发主动失效。

逐层核对 SQL：

    -- ① 分组：倍率 + 高峰 + 图片/视频独立倍率（group_id 取审计行）
    SELECT id, name, platform, rate_multiplier,
           peak_rate_enabled, peak_rate_multiplier, peak_start, peak_end,
           image_rate_independent, image_rate_multiplier,
           video_rate_independent, video_rate_multiplier
    FROM groups WHERE id = <审计行里的 group_id>;

    -- ② 用户专属倍率覆盖（最容易被忽略的一张表）
    SELECT user_id, group_id, rate_multiplier
    FROM user_group_rate_multipliers
    WHERE group_id = <group_id> ORDER BY rate_multiplier DESC;

    -- ③ 账号倍率
    SELECT id, name, platform, rate_multiplier FROM accounts
    WHERE rate_multiplier IS NOT NULL AND rate_multiplier > 1 ORDER BY rate_multiplier DESC;

    -- ④ 系统默认倍率（不在数据库）：cat /opt/sub2api/config.yaml 看 default.rate_multiplier

如果四处值都正常、但拦截仍在发生：看拆解字段里具体是哪个 key 异常，
再对照上表缓存 TTL 判断是否缓存陈旧；重启服务后重试即可确认。
## 分析手册：拦截发生后怎么定位与处置

### 第一步：取最新的拦截记录

    SELECT created_at, action, path, group_id, user_id, api_key_id, model,
           rate_multiplier, multiplier_breakdown, reason
    FROM billing_guard_logs ORDER BY created_at DESC LIMIT 10;

### 第二步：读 multiplier_breakdown，判断异常来自哪一层

| breakdown 示例 | 含义 | 去向 |
| --- | --- | --- |
| `group_default=2000` | 分组倍率本身就是 2000 | 查 `groups` 表 → 管理后台改回 |
| `group_default=1,user_rate=2000` | 用户专属倍率覆盖是 2000 | 查 `user_group_rate_multipliers` 表 |
| `group_default=1,peak=2000` | 高峰倍率 2000（请求落在高峰窗口，仅订阅分组） | 查 `groups.peak_rate_*` 配置 |
| `image_rate=2000` / `video_rate=2000` | 图片/视频独立倍率异常 | 查 `groups.image/video_rate_*` |
| `account_rate=2000` | 账号倍率异常 | 查 `accounts.rate_multiplier` |
| 全部为 1，但 `rate_multiplier` 很大 | **比率兜底触发**（上游返回异常 usage、搜索附加费、长上下文叠加等） | 这类需要深挖，把整行发出来定位 |

### 第三步：对照数据库现值，分类处置

- **DB 值与 breakdown 一致** → 配置真的错了：走管理后台改回（后台修改会自动失效缓存）。
  顺带排查是谁改的：直接 SQL 改库、批量脚本、迁移、备份恢复都会造成这类问题。
- **DB 值是正常的，但 breakdown 是旧值** → **缓存陈旧**：
  `systemctl restart sub2api` 立即清空全部进程内缓存；或等 5 分钟让 Redis 鉴权缓存自然过期。
- **peak 异常** → 注意高峰倍率只在 [peak_start, peak_end) 时间段内生效，
  请求时间落在这个窗口才会被乘上。

### 第四步：修复后验证

    ① 用同一把 Key 再发一个请求 → 不再出现 blocked 日志，usage_logs 正常入账；
    ② billing_guard_logs 中该 group_id/user_id 不再新增 blocked 行；
    ③ 用户余额恢复按配置正常扣费。

### 日常巡检（建议每天一次）

    -- 近 7 天拦截趋势
    SELECT created_at::date AS day,
           count(*) FILTER (WHERE action='blocked')  AS blocked,
           count(*) FILTER (WHERE action='observed') AS observed
    FROM billing_guard_logs GROUP BY 1 ORDER BY 1 DESC LIMIT 7;

    -- 谁最常触发（按分组 / 按 Key）
    SELECT group_id, count(*) AS n, max(rate_multiplier) AS max_rate
    FROM billing_guard_logs WHERE group_id > 0 GROUP BY 1 ORDER BY n DESC LIMIT 10;

    SELECT api_key_id, count(*) AS n, max(rate_multiplier) AS max_rate
    FROM billing_guard_logs GROUP BY 1 ORDER BY n DESC LIMIT 10;

拦截发生时会同时输出 ALERT 日志（`journalctl -u sub2api -f | grep billingguard`），
如果希望被动告警（钉钉/webhook/邮件），可以在护栏里加通知回调——需要时随时可以加。

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
