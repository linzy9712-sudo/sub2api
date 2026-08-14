//go:build unit

package service

import (
	"context"
	"errors"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/billingguard"
	"github.com/stretchr/testify/require"
)

func TestBillingGuardLogRecorder_InsertsRow(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO billing_guard_logs")).
		WithArgs(
			"blocked", "gateway", "req-1", "claude-sonnet-4",
			int64(601), int64(501), int64(701),
			1000.0, 1.0, 0.00012, 0.12,
			"rate_multiplier 1000.00 exceeds max 100.00",
		).
		WillReturnResult(sqlmock.NewResult(1, 1))

	rec := NewBillingGuardLogRecorder(db)
	rec.RecordEvent(context.Background(), billingguard.Event{
		Path:                  "gateway",
		RequestID:             "req-1",
		Model:                 "claude-sonnet-4",
		UserID:                601,
		APIKeyID:              501,
		AccountID:             701,
		RateMultiplier:        1000,
		AccountRateMultiplier: 1,
		TotalCost:             0.00012,
		ActualCost:            0.12,
	}, "blocked", "rate_multiplier 1000.00 exceeds max 100.00")

	require.NoError(t, mock.ExpectationsWereMet())
}

func TestBillingGuardLogRecorder_InsertFailureCounts(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO billing_guard_logs")).
		WillReturnError(errors.New("db down"))

	before := BillingGuardLogInsertStats()
	rec := NewBillingGuardLogRecorder(db)
	rec.RecordEvent(context.Background(), billingguard.Event{Path: "gateway"}, "blocked", "x")
	require.Equal(t, before+1, BillingGuardLogInsertStats())

	require.NoError(t, mock.ExpectationsWereMet())
}

func TestBillingGuardLogRecorder_NilDBSafe(t *testing.T) {
	// 记录器退化：nil db 不 panic、不计数。
	before := BillingGuardLogInsertStats()
	rec := NewBillingGuardLogRecorder(nil)
	rec.RecordEvent(context.Background(), billingguard.Event{Path: "gateway"}, "blocked", "x")
	require.Equal(t, before, BillingGuardLogInsertStats())
}
