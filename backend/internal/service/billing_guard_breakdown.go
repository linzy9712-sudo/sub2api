package service

import (
	"fmt"
	"strings"
)

// buildBillingGuardBreakdown 生成倍率来源拆解字符串，用于护栏拦截告警日志与
// billing_guard_logs 审计表，回答「异常倍率到底来自哪一层」。
//
// 倍率叠加链路（token 计费）：
//
//	system_default（config.yaml） → group_default（分组） → user_rate（用户专属覆盖）
//	→ × peak（高峰因子） → 最终 token 倍率。
//
// 图片/视频按次计费在 group 开启独立倍率时使用 image_rate/video_rate，
// 账号配额消耗另乘 account_rate。
//
// 各参数：
//   - systemDefault：系统默认倍率（0 表示该路径不适用）
//   - groupDefault：分组默认倍率（0 表示无分组）
//   - userResolved：高峰叠加前的实际值；与 groupDefault 不同即存在用户专属覆盖
//   - peak：高峰因子（1 表示未触发）
//   - imageRate / videoRate：图片/视频最终按次倍率（0 表示未启用；等于 userResolved 时省略）
//   - accountRate：账号倍率（1 表示默认）
func buildBillingGuardBreakdown(systemDefault, groupDefault, userResolved, peak, imageRate, videoRate, accountRate float64) string {
	var parts []string
	if systemDefault != 0 && systemDefault != 1 {
		parts = append(parts, fmt.Sprintf("system_default=%g", systemDefault))
	}
	parts = append(parts, fmt.Sprintf("group_default=%g", groupDefault))
	if userResolved != groupDefault {
		parts = append(parts, fmt.Sprintf("user_rate=%g", userResolved))
	}
	if peak != 1 {
		parts = append(parts, fmt.Sprintf("peak=%g", peak))
	}
	if imageRate != 0 && imageRate != userResolved {
		parts = append(parts, fmt.Sprintf("image_rate=%g", imageRate))
	}
	if videoRate != 0 && videoRate != userResolved {
		parts = append(parts, fmt.Sprintf("video_rate=%g", videoRate))
	}
	if accountRate != 1 {
		parts = append(parts, fmt.Sprintf("account_rate=%g", accountRate))
	}
	return strings.Join(parts, ",")
}
