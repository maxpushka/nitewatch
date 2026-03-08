package custody_test

import (
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/layer-3/nitewatch/custody"
	"github.com/layer-3/nitewatch/internal/checker"
	"github.com/layer-3/nitewatch/internal/store"
)

// test addresses
var (
	tokenA = common.HexToAddress("0xAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")
	tokenB = common.HexToAddress("0xBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB")
	user1  = common.HexToAddress("0x1111111111111111111111111111111111111111")
	user2  = common.HexToAddress("0x2222222222222222222222222222222222222222")
)

func newTestDB(t *testing.T) *store.Adapter {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
		Logger: logger.Discard,
	})
	require.NoError(t, err)
	adapter, err := store.NewAdapter(db)
	require.NoError(t, err)
	return adapter
}

func eth(n int64) *big.Int {
	return new(big.Int).Mul(big.NewInt(n), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil))
}

// ---------------------------------------------------------------------------
// Global limit tests
// ---------------------------------------------------------------------------

func TestPolicyEngine_GlobalHourlyLimit(t *testing.T) {
	db := newTestDB(t)
	mock := custody.NewMockBackend()

	limits := map[common.Address]checker.Limit{
		tokenA: {Hourly: eth(100), Daily: eth(1000)},
	}
	chk := checker.New(limits, nil, db)

	// First withdrawal: 60 ETH — should pass.
	require.NoError(t, chk.Check(user1, tokenA, eth(60)))
	record(t, chk, mock, user1, tokenA, eth(60), big.NewInt(1))

	// Second withdrawal: 30 ETH — total 90, still under 100 hourly.
	require.NoError(t, chk.Check(user1, tokenA, eth(30)))
	record(t, chk, mock, user1, tokenA, eth(30), big.NewInt(2))

	// Third withdrawal: 20 ETH — total 110, should exceed hourly limit.
	err := chk.Check(user1, tokenA, eth(20))
	require.Error(t, err)
	assert.ErrorIs(t, err, checker.ErrHourlyLimitExceeded)
}

func TestPolicyEngine_GlobalDailyLimit(t *testing.T) {
	db := newTestDB(t)
	mock := custody.NewMockBackend()

	// Hourly limit high enough to not interfere, daily limit at 1000.
	limits := map[common.Address]checker.Limit{
		tokenA: {Hourly: eth(5000), Daily: eth(1000)},
	}
	chk := checker.New(limits, nil, db)

	// Withdraw 400 twice → 800 total.
	for i := int64(1); i <= 2; i++ {
		require.NoError(t, chk.Check(user1, tokenA, eth(400)))
		record(t, chk, mock, user1, tokenA, eth(400), big.NewInt(i))
	}

	// 800 total so far. 300 more → 1100 > 1000 daily limit.
	err := chk.Check(user1, tokenA, eth(300))
	require.Error(t, err)
	assert.ErrorIs(t, err, checker.ErrDailyLimitExceeded)

	// But 200 more → 1000 exactly at limit — should pass.
	require.NoError(t, chk.Check(user1, tokenA, eth(200)))
}

func TestPolicyEngine_NoLimitsConfigured(t *testing.T) {
	db := newTestDB(t)

	limits := map[common.Address]checker.Limit{
		tokenA: {Hourly: eth(100)},
	}
	chk := checker.New(limits, nil, db)

	// tokenB has no limits configured.
	err := chk.Check(user1, tokenB, eth(1))
	require.Error(t, err)
	assert.ErrorIs(t, err, checker.ErrNoLimitsConfigured)
}

func TestPolicyEngine_InvalidInputs(t *testing.T) {
	db := newTestDB(t)
	chk := checker.New(map[common.Address]checker.Limit{
		tokenA: {Hourly: eth(100)},
	}, nil, db)

	t.Run("zero amount", func(t *testing.T) {
		err := chk.Check(user1, tokenA, big.NewInt(0))
		assert.ErrorIs(t, err, checker.ErrInvalidAmount)
	})

	t.Run("negative amount", func(t *testing.T) {
		err := chk.Check(user1, tokenA, big.NewInt(-1))
		assert.ErrorIs(t, err, checker.ErrInvalidAmount)
	})

	t.Run("zero user", func(t *testing.T) {
		err := chk.Check(common.Address{}, tokenA, eth(1))
		assert.ErrorIs(t, err, checker.ErrInvalidUser)
	})
}

// ---------------------------------------------------------------------------
// Per-user limit tests
// ---------------------------------------------------------------------------

func TestPolicyEngine_UserHourlyLimit(t *testing.T) {
	db := newTestDB(t)
	mock := custody.NewMockBackend()

	globalLimits := map[common.Address]checker.Limit{
		tokenA: {Hourly: eth(1000), Daily: eth(10000)},
	}
	userOverrides := map[common.Address]map[common.Address]checker.Limit{
		user1: {tokenA: {Hourly: eth(50)}},
	}
	chk := checker.New(globalLimits, userOverrides, db)

	// user1 limited to 50/hour.
	require.NoError(t, chk.Check(user1, tokenA, eth(40)))
	record(t, chk, mock, user1, tokenA, eth(40), big.NewInt(1))

	err := chk.Check(user1, tokenA, eth(20))
	require.Error(t, err)
	assert.ErrorIs(t, err, checker.ErrUserHourlyLimitExceeded)

	// user2 has no per-user override — can use global limit freely.
	require.NoError(t, chk.Check(user2, tokenA, eth(500)))
}

func TestPolicyEngine_UserDailyLimit(t *testing.T) {
	db := newTestDB(t)
	mock := custody.NewMockBackend()

	globalLimits := map[common.Address]checker.Limit{
		tokenA: {Hourly: eth(10000), Daily: eth(100000)},
	}
	userOverrides := map[common.Address]map[common.Address]checker.Limit{
		user1: {tokenA: {Daily: eth(200)}},
	}
	chk := checker.New(globalLimits, userOverrides, db)

	require.NoError(t, chk.Check(user1, tokenA, eth(150)))
	record(t, chk, mock, user1, tokenA, eth(150), big.NewInt(1))

	err := chk.Check(user1, tokenA, eth(100))
	require.Error(t, err)
	assert.ErrorIs(t, err, checker.ErrUserDailyLimitExceeded)
}

// ---------------------------------------------------------------------------
// Multi-user isolation
// ---------------------------------------------------------------------------

func TestPolicyEngine_MultiUserIsolation(t *testing.T) {
	db := newTestDB(t)
	mock := custody.NewMockBackend()

	limits := map[common.Address]checker.Limit{
		tokenA: {Hourly: eth(100), Daily: eth(1000)},
	}
	userOverrides := map[common.Address]map[common.Address]checker.Limit{
		user1: {tokenA: {Hourly: eth(30)}},
		user2: {tokenA: {Hourly: eth(30)}},
	}
	chk := checker.New(limits, userOverrides, db)

	// user1 withdraws 25.
	require.NoError(t, chk.Check(user1, tokenA, eth(25)))
	record(t, chk, mock, user1, tokenA, eth(25), big.NewInt(1))

	// user2 can still withdraw 25 independently.
	require.NoError(t, chk.Check(user2, tokenA, eth(25)))
	record(t, chk, mock, user2, tokenA, eth(25), big.NewInt(2))

	// But user1 can only do 5 more (25+5 = 30 = limit).
	require.NoError(t, chk.Check(user1, tokenA, eth(5)))
	record(t, chk, mock, user1, tokenA, eth(5), big.NewInt(3))

	err := chk.Check(user1, tokenA, eth(1))
	require.Error(t, err)
	assert.ErrorIs(t, err, checker.ErrUserHourlyLimitExceeded)
}

// ---------------------------------------------------------------------------
// Multi-token isolation
// ---------------------------------------------------------------------------

func TestPolicyEngine_MultiTokenIsolation(t *testing.T) {
	db := newTestDB(t)
	mock := custody.NewMockBackend()

	limits := map[common.Address]checker.Limit{
		tokenA: {Hourly: eth(100)},
		tokenB: {Hourly: eth(50)},
	}
	chk := checker.New(limits, nil, db)

	// Exhaust tokenA limit.
	require.NoError(t, chk.Check(user1, tokenA, eth(100)))
	record(t, chk, mock, user1, tokenA, eth(100), big.NewInt(1))

	err := chk.Check(user1, tokenA, eth(1))
	require.Error(t, err)
	assert.ErrorIs(t, err, checker.ErrHourlyLimitExceeded)

	// tokenB should still be available.
	require.NoError(t, chk.Check(user1, tokenB, eth(50)))
}

// ---------------------------------------------------------------------------
// Boundary / edge cases
// ---------------------------------------------------------------------------

func TestPolicyEngine_ExactlyAtLimit(t *testing.T) {
	db := newTestDB(t)
	mock := custody.NewMockBackend()

	limits := map[common.Address]checker.Limit{
		tokenA: {Hourly: eth(100)},
	}
	chk := checker.New(limits, nil, db)

	// Exactly at limit should pass.
	require.NoError(t, chk.Check(user1, tokenA, eth(100)))
	record(t, chk, mock, user1, tokenA, eth(100), big.NewInt(1))

	// One wei over should fail.
	err := chk.Check(user1, tokenA, big.NewInt(1))
	require.Error(t, err)
	assert.ErrorIs(t, err, checker.ErrHourlyLimitExceeded)
}

func TestPolicyEngine_SingleWeiOverLimit(t *testing.T) {
	db := newTestDB(t)

	limit := new(big.Int).SetUint64(1000)
	limits := map[common.Address]checker.Limit{
		tokenA: {Hourly: limit},
	}
	chk := checker.New(limits, nil, db)

	// 1001 wei should exceed 1000 wei hourly.
	err := chk.Check(user1, tokenA, new(big.Int).SetUint64(1001))
	require.Error(t, err)
	assert.ErrorIs(t, err, checker.ErrHourlyLimitExceeded)
}

// ---------------------------------------------------------------------------
// Mock backend — retry & failure injection
// ---------------------------------------------------------------------------

func TestMockBackend_FinalizeWithRetry(t *testing.T) {
	mock := custody.NewMockBackend()

	// Start a withdrawal.
	tx, err := mock.StartWithdraw(nil, user1, tokenA, eth(10), big.NewInt(1))
	require.NoError(t, err)
	require.NotNil(t, tx)

	// Read the event to get the withdrawal ID.
	ev := <-mock.StartedSink
	wID := ev.WithdrawalID

	// Inject 3 transient failures before success.
	mock.FailCount = 3

	var attempts int
	for {
		_, err = mock.FinalizeWithdraw(nil, wID)
		attempts++
		if err == nil {
			break
		}
		require.Less(t, attempts, 10, "too many retries")
	}

	assert.Equal(t, 4, attempts, "expected 3 failures + 1 success")
	assert.True(t, mock.Finalized[wID])
}

func TestMockBackend_RejectAfterFinalize(t *testing.T) {
	mock := custody.NewMockBackend()

	_, err := mock.StartWithdraw(nil, user1, tokenA, eth(10), big.NewInt(1))
	require.NoError(t, err)
	ev := <-mock.StartedSink

	_, err = mock.FinalizeWithdraw(nil, ev.WithdrawalID)
	require.NoError(t, err)

	// Rejecting a finalized withdrawal should fail.
	_, err = mock.RejectWithdraw(nil, ev.WithdrawalID)
	assert.ErrorIs(t, err, custody.ErrAlreadyFinalized)
}

func TestMockBackend_FinalizeNonExistent(t *testing.T) {
	mock := custody.NewMockBackend()
	_, err := mock.FinalizeWithdraw(nil, [32]byte{0xff})
	assert.ErrorIs(t, err, custody.ErrWithdrawalNotFound)
}

func TestMockBackend_DoubleFinalize(t *testing.T) {
	mock := custody.NewMockBackend()

	_, err := mock.StartWithdraw(nil, user1, tokenA, eth(5), big.NewInt(1))
	require.NoError(t, err)
	ev := <-mock.StartedSink

	_, err = mock.FinalizeWithdraw(nil, ev.WithdrawalID)
	require.NoError(t, err)

	_, err = mock.FinalizeWithdraw(nil, ev.WithdrawalID)
	assert.ErrorIs(t, err, custody.ErrAlreadyFinalized)
}

func TestMockBackend_DuplicateStart(t *testing.T) {
	mock := custody.NewMockBackend()

	_, err := mock.StartWithdraw(nil, user1, tokenA, eth(5), big.NewInt(1))
	require.NoError(t, err)
	<-mock.StartedSink

	_, err = mock.StartWithdraw(nil, user1, tokenA, eth(5), big.NewInt(1))
	assert.ErrorIs(t, err, custody.ErrWithdrawalAlreadyExists)
}

func TestMockBackend_InjectedError(t *testing.T) {
	mock := custody.NewMockBackend()

	injected := errors.New("rpc timeout")
	mock.TxErrors["start"] = injected

	_, err := mock.StartWithdraw(nil, user1, tokenA, eth(1), big.NewInt(1))
	assert.ErrorIs(t, err, injected)

	// Clear error and retry succeeds.
	delete(mock.TxErrors, "start")
	_, err = mock.StartWithdraw(nil, user1, tokenA, eth(1), big.NewInt(1))
	assert.NoError(t, err)
}

func TestMockBackend_CallLog(t *testing.T) {
	mock := custody.NewMockBackend()

	_, _ = mock.StartWithdraw(nil, user1, tokenA, eth(10), big.NewInt(1))
	ev := <-mock.StartedSink
	_, _ = mock.FinalizeWithdraw(nil, ev.WithdrawalID)

	require.Len(t, mock.CallLog, 2)
	assert.Equal(t, "StartWithdraw", mock.CallLog[0].Method)
	assert.Equal(t, "FinalizeWithdraw", mock.CallLog[1].Method)
}

func TestMockBackend_Reset(t *testing.T) {
	mock := custody.NewMockBackend()

	_, _ = mock.StartWithdraw(nil, user1, tokenA, eth(1), big.NewInt(1))
	<-mock.StartedSink

	mock.Reset()

	assert.Empty(t, mock.Withdrawals)
	assert.Empty(t, mock.CallLog)
	assert.Equal(t, "mock", mock.Mode())
}

// ---------------------------------------------------------------------------
// Full flow: policy check → mock finalize → record → re-check
// ---------------------------------------------------------------------------

func TestPolicyEngine_FullFlowWithMockBackend(t *testing.T) {
	db := newTestDB(t)
	mock := custody.NewMockBackend()

	limits := map[common.Address]checker.Limit{
		tokenA: {Hourly: eth(100)},
	}
	chk := checker.New(limits, nil, db)

	// Step 1: Check policy — should pass.
	require.NoError(t, chk.Check(user1, tokenA, eth(80)))

	// Step 2: Submit to mock backend.
	_, err := mock.StartWithdraw(nil, user1, tokenA, eth(80), big.NewInt(1))
	require.NoError(t, err)
	ev := <-mock.StartedSink

	// Step 3: Finalize on mock backend.
	_, err = mock.FinalizeWithdraw(nil, ev.WithdrawalID)
	require.NoError(t, err)

	// Step 4: Record the withdrawal.
	require.NoError(t, chk.Record(&custody.Withdrawal{
		WithdrawalID: ev.WithdrawalID,
		User:         user1,
		Token:        tokenA,
		Amount:       eth(80),
		Timestamp:    time.Now(),
	}))

	// Step 5: Second check — 30 more would exceed 100 hourly.
	err = chk.Check(user1, tokenA, eth(30))
	require.Error(t, err)
	assert.ErrorIs(t, err, checker.ErrHourlyLimitExceeded)

	// But 20 more is fine (80+20 = 100).
	require.NoError(t, chk.Check(user1, tokenA, eth(20)))
}

// ---------------------------------------------------------------------------
// Retry mechanism simulation
// ---------------------------------------------------------------------------

func TestPolicyEngine_RetryMechanism(t *testing.T) {
	db := newTestDB(t)
	mock := custody.NewMockBackend()

	limits := map[common.Address]checker.Limit{
		tokenA: {Hourly: eth(100)},
	}
	chk := checker.New(limits, nil, db)

	// Policy check passes.
	require.NoError(t, chk.Check(user1, tokenA, eth(50)))

	// Start withdrawal on mock.
	_, err := mock.StartWithdraw(nil, user1, tokenA, eth(50), big.NewInt(1))
	require.NoError(t, err)
	ev := <-mock.StartedSink

	// Simulate 5 transient failures then success.
	mock.FailCount = 5
	const maxRetries = 10
	var succeeded bool
	for i := 0; i < maxRetries; i++ {
		_, err = mock.FinalizeWithdraw(nil, ev.WithdrawalID)
		if err == nil {
			succeeded = true
			break
		}
	}
	require.True(t, succeeded, "finalize should eventually succeed")
	assert.True(t, mock.Finalized[ev.WithdrawalID])

	// Record and verify limit is consumed.
	require.NoError(t, chk.Record(&custody.Withdrawal{
		WithdrawalID: ev.WithdrawalID,
		User:         user1,
		Token:        tokenA,
		Amount:       eth(50),
		Timestamp:    time.Now(),
	}))

	// 60 more would exceed 100 hourly limit.
	err = chk.Check(user1, tokenA, eth(60))
	assert.ErrorIs(t, err, checker.ErrHourlyLimitExceeded)
}

func TestPolicyEngine_RetryExhausted(t *testing.T) {
	mock := custody.NewMockBackend()

	_, err := mock.StartWithdraw(nil, user1, tokenA, eth(10), big.NewInt(1))
	require.NoError(t, err)
	ev := <-mock.StartedSink

	// All attempts fail.
	mock.TxErrors["finalize"] = errors.New("permanent failure")

	const maxRetries = 5
	var lastErr error
	for i := 0; i < maxRetries; i++ {
		_, lastErr = mock.FinalizeWithdraw(nil, ev.WithdrawalID)
	}

	require.Error(t, lastErr)
	assert.False(t, mock.Finalized[ev.WithdrawalID], "should not be finalized after exhausting retries")
}

func TestPolicyEngine_RejectFlowWithRetry(t *testing.T) {
	mock := custody.NewMockBackend()

	_, err := mock.StartWithdraw(nil, user1, tokenA, eth(10), big.NewInt(1))
	require.NoError(t, err)
	ev := <-mock.StartedSink

	// First reject attempt fails transiently.
	mock.TxErrors["reject"] = errors.New("nonce too low")
	_, err = mock.RejectWithdraw(nil, ev.WithdrawalID)
	require.Error(t, err)

	// Retry succeeds.
	delete(mock.TxErrors, "reject")
	_, err = mock.RejectWithdraw(nil, ev.WithdrawalID)
	require.NoError(t, err)

	assert.True(t, mock.Rejected[ev.WithdrawalID])
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func record(t *testing.T, chk *checker.Checker, mock *custody.MockBackend, user, token common.Address, amount, nonce *big.Int) {
	t.Helper()

	_, err := mock.StartWithdraw(nil, user, token, amount, nonce)
	require.NoError(t, err)
	ev := <-mock.StartedSink

	_, err = mock.FinalizeWithdraw(nil, ev.WithdrawalID)
	require.NoError(t, err)

	require.NoError(t, chk.Record(&custody.Withdrawal{
		WithdrawalID: ev.WithdrawalID,
		User:         user,
		Token:        token,
		Amount:       amount,
		Timestamp:    time.Now(),
	}))
}
