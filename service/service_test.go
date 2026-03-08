package service

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"github.com/layer-3/nitewatch/config"
	"github.com/layer-3/nitewatch/custody"
	"github.com/layer-3/nitewatch/internal/checker"
	"github.com/layer-3/nitewatch/internal/store"
)

// --- Mock helpers ---

func newTestStore(t *testing.T) *store.Adapter {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "test.db")), &gorm.Config{
		Logger: gormlogger.Discard,
	})
	require.NoError(t, err)
	adapter, err := store.NewAdapter(db)
	require.NoError(t, err)
	return adapter
}

func newTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	})).With("service", "nitewatch-test")
}

// --- Tests ---

func TestTxMuSerializesTransactions(t *testing.T) {
	// Verify that sendTx serializes concurrent calls through the mutex.
	svc := &Service{
		auth:   &bind.TransactOpts{From: common.HexToAddress("0x1")},
		Logger: newTestLogger(),
	}

	var order []int
	var mu sync.Mutex
	var wg sync.WaitGroup

	for i := range 5 {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			_, _ = svc.sendTx(context.Background(), func(auth *bind.TransactOpts) (*types.Transaction, error) {
				mu.Lock()
				order = append(order, idx)
				mu.Unlock()
				return nil, nil
			})
		}(i)
	}

	wg.Wait()
	// All 5 calls completed.
	assert.Len(t, order, 5)
}

func TestRecordEventRecoverableDoesNotAdvanceCursor(t *testing.T) {
	db := newTestStore(t)
	svc := &Service{
		store:  db,
		Logger: newTestLogger(),
	}

	ev := &store.WithdrawEventModel{
		WithdrawalID: "0xaaa",
		UserAddress:  "0x111",
		TokenAddress: "0x000",
		Amount:       "100",
		Decision:     "error",
		Reason:       "test error",
		BlockNumber:  42,
		TxHash:       "0xdeadbeef",
		LogIndex:     0,
	}

	svc.recordEventRecoverable(svc.Logger, ev)

	// The event should be stored.
	assert.True(t, db.HasWithdrawEvent("0xaaa"))

	// But the cursor should NOT be advanced.
	block, logIdx, err := db.GetCursor("withdraw_started")
	require.NoError(t, err)
	assert.Equal(t, uint64(0), block)
	assert.Equal(t, uint32(0), logIdx)
}

func TestRecordEventAdvancesCursor(t *testing.T) {
	db := newTestStore(t)
	svc := &Service{
		store:  db,
		Logger: newTestLogger(),
	}

	ev := &store.WithdrawEventModel{
		WithdrawalID: "0xbbb",
		UserAddress:  "0x111",
		TokenAddress: "0x000",
		Amount:       "200",
		Decision:     "approved",
		BlockNumber:  99,
		TxHash:       "0xbeef",
		LogIndex:     3,
	}

	svc.recordEvent(svc.Logger, ev)

	block, logIdx, err := db.GetCursor("withdraw_started")
	require.NoError(t, err)
	assert.Equal(t, uint64(99), block)
	assert.Equal(t, uint32(3), logIdx)
}

func TestPendingFinalizationLifecycle(t *testing.T) {
	db := newTestStore(t)

	// Save a pending finalization.
	p := &store.PendingFinalizationModel{
		WithdrawalID: "0xccc",
		UserAddress:  "0x111",
		TokenAddress: "0x000",
		Amount:       "300",
	}
	require.NoError(t, db.SavePendingFinalization(p))

	// Verify it's returned by GetPendingFinalizations.
	pending, err := db.GetPendingFinalizations()
	require.NoError(t, err)
	require.Len(t, pending, 1)
	assert.Equal(t, "0xccc", pending[0].WithdrawalID)
	assert.Equal(t, 0, pending[0].RetryCount)

	// Increment retry count.
	require.NoError(t, db.IncrementFinalizationRetry("0xccc"))
	pending, err = db.GetPendingFinalizations()
	require.NoError(t, err)
	assert.Equal(t, 1, pending[0].RetryCount)

	// Complete.
	require.NoError(t, db.CompletePendingFinalization("0xccc"))
	pending, err = db.GetPendingFinalizations()
	require.NoError(t, err)
	assert.Empty(t, pending)
}

func TestPendingRejectionRetryCount(t *testing.T) {
	db := newTestStore(t)

	p := &store.PendingRejectionModel{
		WithdrawalID: "0xddd",
		Reason:       "limit exceeded",
	}
	require.NoError(t, db.SavePendingRejection(p))

	pending, err := db.GetPendingRejections()
	require.NoError(t, err)
	require.Len(t, pending, 1)
	assert.Equal(t, 0, pending[0].RetryCount)

	// Increment 3 times.
	for range 3 {
		require.NoError(t, db.IncrementRejectionRetry("0xddd"))
	}

	pending, err = db.GetPendingRejections()
	require.NoError(t, err)
	assert.Equal(t, 3, pending[0].RetryCount)
}

func TestUpdateWithdrawEventDecision(t *testing.T) {
	db := newTestStore(t)

	ev := &store.WithdrawEventModel{
		WithdrawalID: "0xeee",
		UserAddress:  "0x111",
		TokenAddress: "0x000",
		Amount:       "400",
		Decision:     "pending",
		Reason:       "awaiting threshold",
		BlockNumber:  50,
		TxHash:       "0xfeed",
		LogIndex:     1,
	}
	require.NoError(t, db.RecordWithdrawEventOnly(ev))

	// Update decision.
	require.NoError(t, db.UpdateWithdrawEventDecision("0xeee", "approved", "finalized by quorum"))

	// HasWithdrawEvent should still return true.
	assert.True(t, db.HasWithdrawEvent("0xeee"))
}

func TestDuplicatePendingFinalizationIgnored(t *testing.T) {
	db := newTestStore(t)

	p := &store.PendingFinalizationModel{
		WithdrawalID: "0xfff",
		UserAddress:  "0x111",
		TokenAddress: "0x000",
		Amount:       "500",
	}
	require.NoError(t, db.SavePendingFinalization(p))
	// Second save should not error (OnConflict DoNothing).
	require.NoError(t, db.SavePendingFinalization(p))

	pending, err := db.GetPendingFinalizations()
	require.NoError(t, err)
	assert.Len(t, pending, 1)
}

func TestUpsertFinalizedCursor(t *testing.T) {
	db := newTestStore(t)

	require.NoError(t, db.UpsertFinalizedCursor(100, 5))

	block, logIdx, err := db.GetCursor("withdraw_finalized")
	require.NoError(t, err)
	assert.Equal(t, uint64(100), block)
	assert.Equal(t, uint32(5), logIdx)

	// Update.
	require.NoError(t, db.UpsertFinalizedCursor(200, 10))

	block, logIdx, err = db.GetCursor("withdraw_finalized")
	require.NoError(t, err)
	assert.Equal(t, uint64(200), block)
	assert.Equal(t, uint32(10), logIdx)
}

func TestConfigSlogLevel(t *testing.T) {
	tests := []struct {
		input    string
		expected slog.Level
	}{
		{"", slog.LevelInfo},
		{"INFO", slog.LevelInfo},
		{"info", slog.LevelInfo},
		{"DEBUG", slog.LevelDebug},
		{"debug", slog.LevelDebug},
		{"WARN", slog.LevelWarn},
		{"WARNING", slog.LevelWarn},
		{"ERROR", slog.LevelError},
		{"unknown", slog.LevelInfo},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			c := config.Config{LogLevel: tt.input}
			assert.Equal(t, tt.expected, c.SlogLevel())
		})
	}
}

func TestProcessDeferredRejectionsMaxRetries(t *testing.T) {
	db := newTestStore(t)
	logger := newTestLogger()

	// Create a pending rejection at max retries.
	p := &store.PendingRejectionModel{
		WithdrawalID: "0x123",
		Reason:       "limit exceeded",
		RetryCount:   maxDeferredRetries,
	}
	require.NoError(t, db.SavePendingRejection(p))

	svc := &Service{
		store:  db,
		Logger: logger,
		auth:   &bind.TransactOpts{From: common.HexToAddress("0x1")},
	}

	svc.processDeferredRejections(context.Background())

	// Should be marked completed due to max retries.
	pending, err := db.GetPendingRejections()
	require.NoError(t, err)
	assert.Empty(t, pending)
}

func TestProcessDeferredFinalizationsMaxRetries(t *testing.T) {
	db := newTestStore(t)
	logger := newTestLogger()

	p := &store.PendingFinalizationModel{
		WithdrawalID: "0x456",
		UserAddress:  "0x111",
		TokenAddress: "0x000",
		Amount:       "100",
		RetryCount:   maxDeferredRetries,
	}
	require.NoError(t, db.SavePendingFinalization(p))

	svc := &Service{
		store:  db,
		Logger: logger,
		auth:   &bind.TransactOpts{From: common.HexToAddress("0x1")},
	}

	svc.processDeferredFinalizations(context.Background())

	pending, err := db.GetPendingFinalizations()
	require.NoError(t, err)
	assert.Empty(t, pending)
}

func TestProcessFinalizedCancelsPending(t *testing.T) {
	db := newTestStore(t)
	logger := newTestLogger()

	// Use a proper 32-byte withdrawal ID, as processFinalized converts via common.Hash.Hex()
	var rawID [32]byte
	rawID[31] = 0xab
	wID := common.Hash(rawID).Hex() // "0x00000000...ab"

	// Create pending rejection and finalization for the same withdrawal.
	require.NoError(t, db.SavePendingRejection(&store.PendingRejectionModel{
		WithdrawalID: wID,
		Reason:       "limit exceeded",
	}))
	require.NoError(t, db.SavePendingFinalization(&store.PendingFinalizationModel{
		WithdrawalID: wID,
		UserAddress:  "0x111",
		TokenAddress: "0x000",
		Amount:       "100",
	}))

	// Create a pending event.
	require.NoError(t, db.RecordWithdrawEventOnly(&store.WithdrawEventModel{
		WithdrawalID: wID,
		UserAddress:  "0x111",
		TokenAddress: "0x000",
		Amount:       "100",
		Decision:     "pending",
		Reason:       "awaiting threshold",
		BlockNumber:  10,
		TxHash:       "0xaaa",
		LogIndex:     0,
	}))

	svc := &Service{
		store:   db,
		Logger:  logger,
		checker: checker.New(nil, nil, db),
	}

	event := &custody.WithdrawFinalizedEvent{
		WithdrawalID: rawID,
		Success:      true,
		BlockNumber:  20,
		TxHash:       common.HexToHash("0xbbb"),
		LogIndex:     1,
	}

	svc.processFinalized(context.Background(), event)

	// Both pending items should be completed.
	rejections, err := db.GetPendingRejections()
	require.NoError(t, err)
	assert.Empty(t, rejections)

	finalizations, err := db.GetPendingFinalizations()
	require.NoError(t, err)
	assert.Empty(t, finalizations)
}

func TestIsContractRevert(t *testing.T) {
	assert.False(t, isContractRevert(nil))
	assert.False(t, isContractRevert(context.Canceled))

	// rpc.Error with code 3 is a contract revert.
	revertErr := rpcError{code: 3, msg: "execution reverted"}
	assert.True(t, isContractRevert(revertErr))

	// rpc.Error with different code is not.
	otherErr := rpcError{code: -32000, msg: "server error"}
	assert.False(t, isContractRevert(otherErr))
}

// rpcError implements rpc.Error for testing.
type rpcError struct {
	code int
	msg  string
}

func (e rpcError) Error() string  { return e.msg }
func (e rpcError) ErrorCode() int { return e.code }

func TestSendTxPassesAuthCopy(t *testing.T) {
	origFrom := common.HexToAddress("0xABCD")
	svc := &Service{
		auth:   &bind.TransactOpts{From: origFrom},
		Logger: newTestLogger(),
	}

	var capturedFrom common.Address
	_, _ = svc.sendTx(context.Background(), func(auth *bind.TransactOpts) (*types.Transaction, error) {
		capturedFrom = auth.From
		return nil, nil
	})

	assert.Equal(t, origFrom, capturedFrom, "sendTx should pass a copy with the same From address")
}

func TestConcurrentSendTxNonceOrdering(t *testing.T) {
	// Verify that concurrent sendTx calls are serialized (not interleaved).
	svc := &Service{
		auth:   &bind.TransactOpts{From: common.HexToAddress("0x1")},
		Logger: newTestLogger(),
	}

	var counter atomic.Int32
	var maxConcurrent atomic.Int32
	var wg sync.WaitGroup

	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = svc.sendTx(context.Background(), func(auth *bind.TransactOpts) (*types.Transaction, error) {
				current := counter.Add(1)
				if current > maxConcurrent.Load() {
					maxConcurrent.Store(current)
				}
				time.Sleep(1 * time.Millisecond)
				counter.Add(-1)
				return nil, nil
			})
		}()
	}

	wg.Wait()
	// Max concurrent should be 1 (serialized).
	assert.Equal(t, int32(1), maxConcurrent.Load(), "sendTx should serialize calls (max concurrency = 1)")
}

func TestProcessFinalizedUpdatesCursor(t *testing.T) {
	db := newTestStore(t)
	logger := newTestLogger()

	svc := &Service{
		store:   db,
		Logger:  logger,
		checker: checker.New(nil, nil, db),
	}

	event := &custody.WithdrawFinalizedEvent{
		WithdrawalID: [32]byte{0x01},
		Success:      true,
		BlockNumber:  42,
		TxHash:       common.HexToHash("0xfeed"),
		LogIndex:     7,
	}

	svc.processFinalized(context.Background(), event)

	block, logIdx, err := db.GetCursor("withdraw_finalized")
	require.NoError(t, err)
	assert.Equal(t, uint64(42), block)
	assert.Equal(t, uint32(7), logIdx)
}
