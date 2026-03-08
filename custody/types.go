package custody

import (
	"context"
	"math/big"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

// WithdrawStartedEvent represents a confirmed WithdrawStarted event from the custody contract.
type WithdrawStartedEvent struct {
	WithdrawalID [32]byte
	User         common.Address
	Token        common.Address
	Amount       *big.Int
	Nonce        *big.Int
	BlockNumber  uint64
	TxHash       common.Hash
	LogIndex     uint
}

// WithdrawFinalizedEvent represents a confirmed WithdrawFinalized event from the custody contract.
type WithdrawFinalizedEvent struct {
	WithdrawalID [32]byte
	Success      bool
	BlockNumber  uint64
	TxHash       common.Hash
	LogIndex     uint
}

// DepositedEvent represents a confirmed Deposited event from the custody contract.
type DepositedEvent struct {
	User        common.Address
	Token       common.Address
	Amount      *big.Int
	BlockNumber uint64
	TxHash      common.Hash
	LogIndex    uint
}

// Withdrawal represents a recorded withdrawal for limit tracking.
type Withdrawal struct {
	WithdrawalID [32]byte
	User         common.Address
	Token        common.Address
	Amount       *big.Int
	BlockNumber  uint64
	TxHash       common.Hash
	Timestamp    time.Time
}

// Custody defines the write operations for the IWithdraw smart contract.
type Custody interface {
	StartWithdraw(opts *bind.TransactOpts, user common.Address, token common.Address, amount *big.Int, nonce *big.Int) (*types.Transaction, error)
	FinalizeWithdraw(opts *bind.TransactOpts, withdrawalId [32]byte) (*types.Transaction, error)
	RejectWithdraw(opts *bind.TransactOpts, withdrawalId [32]byte) (*types.Transaction, error)
}

// EventListener defines the ability to subscribe to custody contract events.
// Each method blocks until the context is cancelled; callers should run them in goroutines.
// The sink channel is closed when the method returns.
// Methods return an error if the listener stops unexpectedly (e.g. backoff limit).
type EventListener interface {
	WatchWithdrawStarted(ctx context.Context, sink chan<- *WithdrawStartedEvent, fromBlock uint64, fromLogIndex uint32) error
	WatchWithdrawFinalized(ctx context.Context, sink chan<- *WithdrawFinalizedEvent, fromBlock uint64, fromLogIndex uint32) error
	WatchDeposited(ctx context.Context, sink chan<- *DepositedEvent, fromBlock uint64, fromLogIndex uint32) error
}

// WithdrawalStore defines the storage operations for tracking withdrawals.
type WithdrawalStore interface {
	Save(w *Withdrawal) error
	GetTotalWithdrawn(token common.Address, since time.Time) (*big.Int, error)
	GetTotalWithdrawnByUser(user common.Address, token common.Address, since time.Time) (*big.Int, error)
}

// EthBackend is the Ethereum client interface required by the service.
// Both *ethclient.Client and simulated.Client satisfy this interface.
type EthBackend interface {
	bind.ContractBackend
	bind.DeployBackend
	ChainID(ctx context.Context) (*big.Int, error)
	Close()
}
