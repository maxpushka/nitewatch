package custody

import (
	"context"
	"math/big"

	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

// Backend is the unified port for custody operations: both on-chain writes
// and event listening. The three concrete adapters (Live, Simulated, Mock)
// satisfy this interface and can be swapped at construction time.
type Backend interface {
	Custody
	EventListener

	// Mode returns the kind of backend ("live", "simulated", "mock").
	Mode() string
}

// TxResult wraps a submitted transaction and its receipt for backends
// that can return a receipt immediately (simulated/mock).
type TxResult struct {
	Tx      *types.Transaction
	Receipt *types.Receipt // nil for live backend (async mining)
}

// BackendConfig holds common configuration for constructing backends.
type BackendConfig struct {
	ContractAddress    common.Address
	ConfirmationBlocks uint64
	Auth               *bind.TransactOpts
}

// NewLiveBackend creates a backend that talks to a real blockchain node.
func NewLiveBackend(client EthBackend, cfg BackendConfig, withdraw *IWithdraw, deposit *IDeposit) Backend {
	return &liveBackend{
		contract: &withdraw.IWithdrawTransactor,
		listener: NewListener(client, cfg.ContractAddress, cfg.ConfirmationBlocks, 0, withdraw, deposit),
	}
}

// NewSimulatedBackend creates a backend backed by go-ethereum's simulated chain.
// commitFn is called after each successful write to mine the pending block.
func NewSimulatedBackend(client EthBackend, cfg BackendConfig, withdraw *IWithdraw, deposit *IDeposit, commitFn func() common.Hash) *SimulatedBackend {
	return &SimulatedBackend{
		contract: &withdraw.IWithdrawTransactor,
		listener: NewListener(client, cfg.ContractAddress, cfg.ConfirmationBlocks, 0, withdraw, deposit),
		CommitFn: commitFn,
		Client:   client,
	}
}

// NewMockBackend creates a fully in-memory backend for unit tests.
func NewMockBackend() *MockBackend {
	return &MockBackend{
		Withdrawals:  make(map[[32]byte]mockWithdrawal),
		Finalized:    make(map[[32]byte]bool),
		Rejected:     make(map[[32]byte]bool),
		TxErrors:     make(map[string]error),
		StartedSink:  make(chan *WithdrawStartedEvent, 100),
		FinalizedSink: make(chan *WithdrawFinalizedEvent, 100),
	}
}

// liveBackend wraps real on-chain contract calls and a polling listener.
type liveBackend struct {
	contract *IWithdrawTransactor
	listener *Listener
}

func (b *liveBackend) Mode() string { return "live" }

func (b *liveBackend) StartWithdraw(opts *bind.TransactOpts, user common.Address, token common.Address, amount *big.Int, nonce *big.Int) (*types.Transaction, error) {
	return b.contract.StartWithdraw(opts, user, token, amount, nonce)
}

func (b *liveBackend) FinalizeWithdraw(opts *bind.TransactOpts, withdrawalId [32]byte) (*types.Transaction, error) {
	return b.contract.FinalizeWithdraw(opts, withdrawalId)
}

func (b *liveBackend) RejectWithdraw(opts *bind.TransactOpts, withdrawalId [32]byte) (*types.Transaction, error) {
	return b.contract.RejectWithdraw(opts, withdrawalId)
}

func (b *liveBackend) WatchWithdrawStarted(ctx context.Context, sink chan<- *WithdrawStartedEvent, fromBlock uint64, fromLogIndex uint32) error {
	return b.listener.WatchWithdrawStarted(ctx, sink, fromBlock, fromLogIndex)
}

func (b *liveBackend) WatchWithdrawFinalized(ctx context.Context, sink chan<- *WithdrawFinalizedEvent, fromBlock uint64, fromLogIndex uint32) error {
	return b.listener.WatchWithdrawFinalized(ctx, sink, fromBlock, fromLogIndex)
}

func (b *liveBackend) WatchDeposited(ctx context.Context, sink chan<- *DepositedEvent, fromBlock uint64, fromLogIndex uint32) error {
	return b.listener.WatchDeposited(ctx, sink, fromBlock, fromLogIndex)
}

// SimulatedBackend wraps a go-ethereum simulated chain. It auto-commits
// after each write so that events appear immediately. Exported so that
// integration tests can access CommitFn and Client for setup operations
// (deposits, receipt checks) that fall outside the Backend interface.
type SimulatedBackend struct {
	contract *IWithdrawTransactor
	listener *Listener
	CommitFn func() common.Hash // calls sim.Commit()
	Client   EthBackend        // exposed for test helpers (receipts, balances)
}

var _ Backend = (*SimulatedBackend)(nil)

func (b *SimulatedBackend) Mode() string { return "simulated" }

func (b *SimulatedBackend) Commit() {
	if b.CommitFn != nil {
		b.CommitFn()
	}
}

func (b *SimulatedBackend) StartWithdraw(opts *bind.TransactOpts, user common.Address, token common.Address, amount *big.Int, nonce *big.Int) (*types.Transaction, error) {
	tx, err := b.contract.StartWithdraw(opts, user, token, amount, nonce)
	if err == nil {
		b.Commit()
	}
	return tx, err
}

func (b *SimulatedBackend) FinalizeWithdraw(opts *bind.TransactOpts, withdrawalId [32]byte) (*types.Transaction, error) {
	tx, err := b.contract.FinalizeWithdraw(opts, withdrawalId)
	if err == nil {
		b.Commit()
	}
	return tx, err
}

func (b *SimulatedBackend) RejectWithdraw(opts *bind.TransactOpts, withdrawalId [32]byte) (*types.Transaction, error) {
	tx, err := b.contract.RejectWithdraw(opts, withdrawalId)
	if err == nil {
		b.Commit()
	}
	return tx, err
}

func (b *SimulatedBackend) WatchWithdrawStarted(ctx context.Context, sink chan<- *WithdrawStartedEvent, fromBlock uint64, fromLogIndex uint32) error {
	return b.listener.WatchWithdrawStarted(ctx, sink, fromBlock, fromLogIndex)
}

func (b *SimulatedBackend) WatchWithdrawFinalized(ctx context.Context, sink chan<- *WithdrawFinalizedEvent, fromBlock uint64, fromLogIndex uint32) error {
	return b.listener.WatchWithdrawFinalized(ctx, sink, fromBlock, fromLogIndex)
}

func (b *SimulatedBackend) WatchDeposited(ctx context.Context, sink chan<- *DepositedEvent, fromBlock uint64, fromLogIndex uint32) error {
	return b.listener.WatchDeposited(ctx, sink, fromBlock, fromLogIndex)
}
