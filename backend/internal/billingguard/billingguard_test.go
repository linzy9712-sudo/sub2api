//go:build unit

package billingguard

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func resetGuard(t *testing.T) {
	t.Helper()
	Configure(DefaultConfig())
	SetRecorder(nil)
	t.Cleanup(func() {
		Configure(DefaultConfig())
		SetRecorder(nil)
	})
}

// fakeRecorder 通过 channel 同步通知，供测试断言 fire-and-forget 的审计回调。
type fakeRecorder struct {
	ch chan recorderCall
}

type recorderCall struct {
	action string
	reason string
	ev     Event
}

func (f *fakeRecorder) RecordEvent(_ context.Context, ev Event, action, reason string) {
	if f.ch != nil {
		f.ch <- recorderCall{action: action, reason: reason, ev: ev}
	}
}

// panicRecorder 用于验证记录器 panic 不影响拦截主流程。
type panicRecorder struct{}

func (panicRecorder) RecordEvent(_ context.Context, _ Event, _, _ string) {
	panic("recorder boom")
}

func waitRecorderCall(t *testing.T, ch chan recorderCall) recorderCall {
	t.Helper()
	select {
	case call := <-ch:
		return call
	case <-time.After(5 * time.Second):
		t.Fatal("recorder was not called within timeout")
		return recorderCall{}
	}
}

func TestCheck_DisabledAllowsEverything(t *testing.T) {
	resetGuard(t)
	Configure(Config{Enabled: false})

	d := Check(Event{RateMultiplier: 1e9, AccountRateMultiplier: 1e9, TotalCost: 0.01, ActualCost: 1e9})
	require.False(t, d.Block)
}

func TestCheck_BlocksAboveThreshold(t *testing.T) {
	resetGuard(t)

	// 恰好等于阈值：放行。
	require.False(t, Check(Event{RateMultiplier: 1000}).Block)
	require.False(t, Check(Event{RateMultiplier: 999.99}).Block)
	// 超过阈值：拦截。
	d := Check(Event{RateMultiplier: 1000.01})
	require.True(t, d.Block)
	require.Contains(t, d.Reason, "rate_multiplier")
}

func TestCheck_BlocksAccountRateMultiplier(t *testing.T) {
	resetGuard(t)

	require.False(t, Check(Event{AccountRateMultiplier: 1000}).Block)
	d := Check(Event{AccountRateMultiplier: 1200})
	require.True(t, d.Block)
	require.Contains(t, d.Reason, "account_rate_multiplier")
}

func TestCheck_BlocksNonFinite(t *testing.T) {
	resetGuard(t)

	for name, ev := range map[string]Event{
		"rate_nan":       {RateMultiplier: math.NaN()},
		"rate_inf":       {RateMultiplier: math.Inf(1)},
		"account_nan":    {AccountRateMultiplier: math.NaN()},
		"account_neginf": {AccountRateMultiplier: math.Inf(-1)},
	} {
		t.Run(name, func(t *testing.T) {
			d := Check(ev)
			require.True(t, d.Block)
			require.Contains(t, d.Reason, "non-finite")
		})
	}
}

// 比率兜底：名义倍率正常，但 ActualCost/TotalCost 被图片/视频按次倍率或
// 其它叠加来源放大到阈值以上时同样拦截。
func TestCheck_RatioFallbackCatchesEffectiveMultiplier(t *testing.T) {
	resetGuard(t)

	d := Check(Event{RateMultiplier: 1, TotalCost: 0.01, ActualCost: 12})
	require.True(t, d.Block)
	require.Contains(t, d.Reason, "effective billed ratio")

	// 比率恰在阈值内：放行。
	require.False(t, Check(Event{RateMultiplier: 1, TotalCost: 0.01, ActualCost: 10}).Block)
	// 零成本（免费倍率 0 或未定价）：不做比率检查。
	require.False(t, Check(Event{RateMultiplier: 1, TotalCost: 0, ActualCost: 0}).Block)
}

func TestCheck_MaxAbsoluteCostUSD(t *testing.T) {
	resetGuard(t)
	Configure(Config{
		Enabled:                  true,
		MaxRateMultiplier:        1000,
		MaxAccountRateMultiplier: 1000,
		MaxAbsoluteCostUSD:       10,
	})

	require.False(t, Check(Event{RateMultiplier: 1, ActualCost: 10}).Block)
	d := Check(Event{RateMultiplier: 1, ActualCost: 10.01})
	require.True(t, d.Block)
	require.Contains(t, d.Reason, "max_absolute_cost_usd")
}

func TestCheckAndBlock_ReturnsSentinel(t *testing.T) {
	resetGuard(t)
	Configure(Config{
		Enabled:                  true,
		MaxRateMultiplier:        100,
		MaxAccountRateMultiplier: 100,
	})

	beforeBlocked, beforeObserved := Stats()
	err := CheckAndBlock(Event{
		Path: "gateway", RequestID: "req-1", Model: "m",
		UserID: 1, APIKeyID: 2, AccountID: 3,
		RateMultiplier: 150,
	})
	require.Error(t, err)
	require.ErrorIs(t, err, ErrBlocked)
	require.Contains(t, err.Error(), "rate_multiplier 150.00")

	blocked, observed := Stats()
	require.Equal(t, beforeBlocked+1, blocked)
	require.Equal(t, beforeObserved, observed)

	// 未命中：透明放行。
	require.NoError(t, CheckAndBlock(Event{RateMultiplier: 50}))
}

func TestCheckAndBlock_ObserveOnlyDoesNotBlock(t *testing.T) {
	resetGuard(t)
	Configure(Config{
		Enabled:                  true,
		MaxRateMultiplier:        100,
		MaxAccountRateMultiplier: 100,
		ObserveOnly:              true,
	})

	beforeBlocked, beforeObserved := Stats()
	require.NoError(t, CheckAndBlock(Event{RateMultiplier: 150}))
	blocked, observed := Stats()
	require.Equal(t, beforeBlocked, blocked)
	require.Equal(t, beforeObserved+1, observed)
}

func TestConfigure_NormalizesInvalidValues(t *testing.T) {
	resetGuard(t)
	Configure(Config{Enabled: true, MaxRateMultiplier: 0, MaxAccountRateMultiplier: math.NaN(), MaxAbsoluteCostUSD: -5})

	c := Current()
	require.Equal(t, float64(DefaultMaxRateMultiplier), c.MaxRateMultiplier)
	require.Equal(t, float64(DefaultMaxAccountRateMultiplier), c.MaxAccountRateMultiplier)
	require.Equal(t, 0.0, c.MaxAbsoluteCostUSD)
}

func TestLoadFromEnv(t *testing.T) {
	t.Setenv(EnvEnabled, "false")
	t.Setenv(EnvMaxRateMultiplier, "42")
	t.Setenv(EnvMaxAccountRateMultiplier, "7")
	t.Setenv(EnvMaxAbsoluteCostUSD, "1.5")
	t.Setenv(EnvObserveOnly, "true")

	c := LoadFromEnv()
	require.False(t, c.Enabled)
	require.Equal(t, 42.0, c.MaxRateMultiplier)
	require.Equal(t, 7.0, c.MaxAccountRateMultiplier)
	require.Equal(t, 1.5, c.MaxAbsoluteCostUSD)
	require.True(t, c.ObserveOnly)
}

func TestLoadFromEnv_InvalidValuesFallBackToDefaults(t *testing.T) {
	t.Setenv(EnvEnabled, "not-a-bool")
	t.Setenv(EnvMaxRateMultiplier, "abc")

	c := LoadFromEnv()
	require.True(t, c.Enabled)
	require.Equal(t, float64(DefaultMaxRateMultiplier), c.MaxRateMultiplier)
}

func TestNewBlockError(t *testing.T) {
	err := NewBlockError("rate_multiplier 2000 exceeds max 1000")
	require.ErrorIs(t, err, ErrBlocked)
	require.True(t, errors.Is(err, ErrBlocked))
}

func TestCheckAndBlock_NotifiesRecorderOnBlock(t *testing.T) {
	resetGuard(t)
	Configure(Config{Enabled: true, MaxRateMultiplier: 100, MaxAccountRateMultiplier: 100})
	rec := &fakeRecorder{ch: make(chan recorderCall, 1)}
	SetRecorder(rec)

	ev := Event{Path: "gateway", RequestID: "req-1", Model: "m", UserID: 1, APIKeyID: 2, AccountID: 3, RateMultiplier: 150}
	require.ErrorIs(t, CheckAndBlock(ev), ErrBlocked)

	call := waitRecorderCall(t, rec.ch)
	require.Equal(t, "blocked", call.action)
	require.Equal(t, ev, call.ev)
	require.Contains(t, call.reason, "rate_multiplier 150.00")
}

func TestCheckAndBlock_NotifiesRecorderOnObserved(t *testing.T) {
	resetGuard(t)
	Configure(Config{Enabled: true, MaxRateMultiplier: 100, MaxAccountRateMultiplier: 100, ObserveOnly: true})
	rec := &fakeRecorder{ch: make(chan recorderCall, 1)}
	SetRecorder(rec)

	ev := Event{Path: "openai", RequestID: "req-2", RateMultiplier: 150}
	require.NoError(t, CheckAndBlock(ev))

	call := waitRecorderCall(t, rec.ch)
	require.Equal(t, "observed", call.action)
	require.Equal(t, ev, call.ev)
}

func TestCheckAndBlock_NoRecorderCallWhenDisabledOrClean(t *testing.T) {
	resetGuard(t)
	rec := &fakeRecorder{ch: make(chan recorderCall, 1)}
	SetRecorder(rec)

	// 组件关闭：不拦截、不审计。
	Configure(Config{Enabled: false, MaxRateMultiplier: 100, MaxAccountRateMultiplier: 100})
	require.NoError(t, CheckAndBlock(Event{RateMultiplier: 150}))

	// 正常放行：不审计。
	Configure(Config{Enabled: true, MaxRateMultiplier: 100, MaxAccountRateMultiplier: 100})
	require.NoError(t, CheckAndBlock(Event{RateMultiplier: 50}))

	select {
	case call := <-rec.ch:
		t.Fatalf("recorder unexpectedly called: %+v", call)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestCheckAndBlock_RecorderPanicDoesNotAffectBlocking(t *testing.T) {
	resetGuard(t)
	Configure(Config{Enabled: true, MaxRateMultiplier: 100, MaxAccountRateMultiplier: 100})
	SetRecorder(panicRecorder{})

	require.ErrorIs(t, CheckAndBlock(Event{RateMultiplier: 150}), ErrBlocked)
}
