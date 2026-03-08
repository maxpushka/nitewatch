package custody

import (
	"context"
	"errors"
	"math/big"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

var (
	ErrWithdrawalNotFound      = errors.New("withdrawal not found")
	ErrWithdrawalAlreadyExists = errors.New("withdrawal already exists")
	ErrAlreadyFinalized        = errors.New("withdrawal already finalized")
	ErrAlreadyRejected         = errors.New("withdrawal already rejected")
)

type mockWithdrawal struct {
	User   common.Address
	Token  common.Address
	Amount *big.Int
	Nonce  *big.Int
}

// MockBackend is a fully in-memory custody backend for unit tests.
// All fields are exported so tests can inspect state and inject faults.
type MockBackend struct {
	mu sync.Mutex

	Withdrawals map[[32]byte]mockWithdrawal
	Finalized   map[[32]byte]bool
	Rejected    map[[32]byte]bool
	NextNonce   uint64

	// TxErrors lets tests inject errors for specific operations.
	// Keys: "start", "finalize", "reject", or a hex withdrawal ID.
	TxErrors map[string]error

	// Latency adds artificial delay to each call (for retry testing).
	Latency time.Duration

	// FailCount makes the next N calls to FinalizeWithdraw return
	// a transient error before succeeding. Decremented on each failure.
	FailCount int

	// Event sinks that tests can read to observe emitted events.
	StartedSink   chan *WithdrawStartedEvent
	FinalizedSink chan *WithdrawFinalizedEvent

	// CallLog records each method invocation for assertions.
	CallLog []MockCall
}

type MockCall struct {
	Method       string
	WithdrawalID [32]byte
	Args         []interface{}
}

var _ Backend = (*MockBackend)(nil)

func (m *MockBackend) Mode() string { return "mock" }

func (m *MockBackend) delay() {
	if m.Latency > 0 {
		time.Sleep(m.Latency)
	}
}

func (m *MockBackend) StartWithdraw(opts *bind.TransactOpts, user common.Address, token common.Address, amount *big.Int, nonce *big.Int) (*types.Transaction, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.delay()

	if err, ok := m.TxErrors["start"]; ok {
		return nil, err
	}

	wID := withdrawalID(user, token, nonce)

	if _, exists := m.Withdrawals[wID]; exists {
		return nil, ErrWithdrawalAlreadyExists
	}

	m.Withdrawals[wID] = mockWithdrawal{
		User:   user,
		Token:  token,
		Amount: new(big.Int).Set(amount),
		Nonce:  new(big.Int).Set(nonce),
	}

	m.CallLog = append(m.CallLog, MockCall{Method: "StartWithdraw", WithdrawalID: wID})

	// Emit event to sink (non-blocking).
	select {
	case m.StartedSink <- &WithdrawStartedEvent{
		WithdrawalID: wID,
		User:         user,
		Token:        token,
		Amount:       new(big.Int).Set(amount),
		Nonce:        new(big.Int).Set(nonce),
		BlockNumber:  m.NextNonce,
	}:
	default:
	}

	return types.NewTx(&types.LegacyTx{Nonce: m.NextNonce}), nil
}

func (m *MockBackend) FinalizeWithdraw(opts *bind.TransactOpts, withdrawalId [32]byte) (*types.Transaction, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.delay()

	if err, ok := m.TxErrors["finalize"]; ok {
		return nil, err
	}
	if err, ok := m.TxErrors[common.Hash(withdrawalId).Hex()]; ok {
		return nil, err
	}

	if m.FailCount > 0 {
		m.FailCount--
		return nil, errors.New("transient error: backend unavailable")
	}

	if _, exists := m.Withdrawals[withdrawalId]; !exists {
		return nil, ErrWithdrawalNotFound
	}
	if m.Finalized[withdrawalId] {
		return nil, ErrAlreadyFinalized
	}
	if m.Rejected[withdrawalId] {
		return nil, ErrAlreadyRejected
	}

	m.Finalized[withdrawalId] = true
	m.CallLog = append(m.CallLog, MockCall{Method: "FinalizeWithdraw", WithdrawalID: withdrawalId})

	select {
	case m.FinalizedSink <- &WithdrawFinalizedEvent{
		WithdrawalID: withdrawalId,
		Success:      true,
		BlockNumber:  m.NextNonce,
	}:
	default:
	}

	return types.NewTx(&types.LegacyTx{Nonce: m.NextNonce}), nil
}

func (m *MockBackend) RejectWithdraw(opts *bind.TransactOpts, withdrawalId [32]byte) (*types.Transaction, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.delay()

	if err, ok := m.TxErrors["reject"]; ok {
		return nil, err
	}

	if _, exists := m.Withdrawals[withdrawalId]; !exists {
		return nil, ErrWithdrawalNotFound
	}
	if m.Finalized[withdrawalId] {
		return nil, ErrAlreadyFinalized
	}
	if m.Rejected[withdrawalId] {
		return nil, ErrAlreadyRejected
	}

	m.Rejected[withdrawalId] = true
	m.CallLog = append(m.CallLog, MockCall{Method: "RejectWithdraw", WithdrawalID: withdrawalId})

	select {
	case m.FinalizedSink <- &WithdrawFinalizedEvent{
		WithdrawalID: withdrawalId,
		Success:      false,
		BlockNumber:  m.NextNonce,
	}:
	default:
	}

	return types.NewTx(&types.LegacyTx{Nonce: m.NextNonce}), nil
}

// WatchWithdrawStarted drains StartedSink into the provided sink until ctx is cancelled.
func (m *MockBackend) WatchWithdrawStarted(ctx context.Context, sink chan<- *WithdrawStartedEvent, _ uint64, _ uint32) error {
	defer close(sink)
	for {
		select {
		case <-ctx.Done():
			return nil
		case ev, ok := <-m.StartedSink:
			if !ok {
				return nil
			}
			sink <- ev
		}
	}
}

// WatchWithdrawFinalized drains FinalizedSink into the provided sink until ctx is cancelled.
func (m *MockBackend) WatchWithdrawFinalized(ctx context.Context, sink chan<- *WithdrawFinalizedEvent, _ uint64, _ uint32) error {
	defer close(sink)
	for {
		select {
		case <-ctx.Done():
			return nil
		case ev, ok := <-m.FinalizedSink:
			if !ok {
				return nil
			}
			sink <- ev
		}
	}
}

// WatchDeposited is a no-op for the mock backend.
func (m *MockBackend) WatchDeposited(ctx context.Context, sink chan<- *DepositedEvent, _ uint64, _ uint32) error {
	defer close(sink)
	<-ctx.Done()
	return nil
}

// Reset clears all state for reuse between test cases.
func (m *MockBackend) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Withdrawals = make(map[[32]byte]mockWithdrawal)
	m.Finalized = make(map[[32]byte]bool)
	m.Rejected = make(map[[32]byte]bool)
	m.TxErrors = make(map[string]error)
	m.CallLog = nil
	m.FailCount = 0
	m.NextNonce = 0
	m.StartedSink = make(chan *WithdrawStartedEvent, 100)
	m.FinalizedSink = make(chan *WithdrawFinalizedEvent, 100)
}

// withdrawalID deterministically derives a withdrawal ID from its parameters.
func withdrawalID(user common.Address, token common.Address, nonce *big.Int) [32]byte {
	var id [32]byte
	copy(id[:20], user.Bytes())
	copy(id[20:], nonce.Bytes())
	return id
}
