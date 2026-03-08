package custody

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	logging "github.com/ipfs/go-log/v2"
	"github.com/layer-3/clearsync/pkg/debounce"
)

var listenerLogger = logging.Logger("custody-listener")

const (
	maxBackOffCount = 5
)

// Listener handles monitoring the blockchain for events from the custody contract.
type Listener struct {
	client             bind.ContractBackend
	contractAddr       common.Address
	confirmationBlocks uint64
	pollInterval       time.Duration
	withdrawFilterer   *IWithdrawFilterer
	depositFilterer    *IDepositFilterer
}

// NewListener creates a new Listener instance.
// client: an Ethereum client supporting log subscriptions (e.g. *ethclient.Client via WebSocket)
// contractAddr: address of the custody contract
// confirmationBlocks: number of blocks to wait before processing events (must be > 0)
// pollInterval: how often to poll for confirmed blocks (defaults to 12s if <= 0)
// withdraw: bound IWithdraw contract instance
// deposit: bound IDeposit contract instance (can be nil if deposit events are not needed)
const defaultPollInterval = 12 * time.Second

func NewListener(client bind.ContractBackend, contractAddr common.Address, confirmationBlocks uint64, pollInterval time.Duration, withdraw *IWithdraw, deposit *IDeposit) *Listener {
	if pollInterval <= 0 {
		pollInterval = defaultPollInterval
	}
	l := &Listener{
		client:             client,
		contractAddr:       contractAddr,
		confirmationBlocks: confirmationBlocks,
		pollInterval:       pollInterval,
	}
	if withdraw != nil {
		l.withdrawFilterer = &withdraw.IWithdrawFilterer
	}
	if deposit != nil {
		l.depositFilterer = &deposit.IDepositFilterer
	}
	return l
}

// Compile-time check that Listener implements EventListener.
var _ EventListener = (*Listener)(nil)

// WatchWithdrawStarted subscribes to WithdrawStarted events and sends them to the sink channel.
// This function blocks forever; run it in a goroutine. The sink channel is closed when it returns.
// Returns an error if the listener stops unexpectedly (e.g. backoff limit reached).
func (l *Listener) WatchWithdrawStarted(ctx context.Context, sink chan<- *WithdrawStartedEvent, fromBlock uint64, fromLogIndex uint32) error {
	defer close(sink)

	parsedABI, err := IWithdrawMetaData.GetAbi()
	if err != nil {
		return fmt.Errorf("failed to parse IWithdraw ABI: %w", err)
	}
	topic := parsedABI.Events["WithdrawStarted"].ID

	return listenEvents(ctx, l.client, "withdraw-started", l.contractAddr, l.confirmationBlocks, l.pollInterval, fromBlock, fromLogIndex,
		[][]common.Hash{{topic}},
		func(log types.Log) {
			ev, err := l.withdrawFilterer.ParseWithdrawStarted(log)
			if err != nil {
				return
			}
			sink <- &WithdrawStartedEvent{
				WithdrawalID: ev.WithdrawalId,
				User:         ev.User,
				Token:        ev.Token,
				Amount:       ev.Amount,
				Nonce:        ev.Nonce,
				BlockNumber:  ev.Raw.BlockNumber,
				TxHash:       ev.Raw.TxHash,
				LogIndex:     ev.Raw.Index,
			}
		},
	)
}

// WatchWithdrawFinalized subscribes to WithdrawFinalized events and sends them to the sink channel.
// This function blocks forever; run it in a goroutine. The sink channel is closed when it returns.
// Returns an error if the listener stops unexpectedly.
func (l *Listener) WatchWithdrawFinalized(ctx context.Context, sink chan<- *WithdrawFinalizedEvent, fromBlock uint64, fromLogIndex uint32) error {
	defer close(sink)

	parsedABI, err := IWithdrawMetaData.GetAbi()
	if err != nil {
		return fmt.Errorf("failed to parse IWithdraw ABI: %w", err)
	}
	topic := parsedABI.Events["WithdrawFinalized"].ID

	return listenEvents(ctx, l.client, "withdraw-finalized", l.contractAddr, l.confirmationBlocks, l.pollInterval, fromBlock, fromLogIndex,
		[][]common.Hash{{topic}},
		func(log types.Log) {
			ev, err := l.withdrawFilterer.ParseWithdrawFinalized(log)
			if err != nil {
				return
			}
			sink <- &WithdrawFinalizedEvent{
				WithdrawalID: ev.WithdrawalId,
				Success:      ev.Success,
				BlockNumber:  ev.Raw.BlockNumber,
				TxHash:       ev.Raw.TxHash,
				LogIndex:     ev.Raw.Index,
			}
		},
	)
}

// WatchDeposited subscribes to Deposited events and sends them to the sink channel.
// This function blocks forever; run it in a goroutine. The sink channel is closed when it returns.
// Returns an error if the listener stops unexpectedly.
func (l *Listener) WatchDeposited(ctx context.Context, sink chan<- *DepositedEvent, fromBlock uint64, fromLogIndex uint32) error {
	defer close(sink)

	if l.depositFilterer == nil {
		return nil
	}

	parsedABI, err := IDepositMetaData.GetAbi()
	if err != nil {
		return fmt.Errorf("failed to parse IDeposit ABI: %w", err)
	}
	topic := parsedABI.Events["Deposited"].ID

	return listenEvents(ctx, l.client, "deposited", l.contractAddr, l.confirmationBlocks, l.pollInterval, fromBlock, fromLogIndex,
		[][]common.Hash{{topic}},
		func(log types.Log) {
			ev, err := l.depositFilterer.ParseDeposited(log)
			if err != nil {
				return
			}
			sink <- &DepositedEvent{
				User:        ev.User,
				Token:       ev.Token,
				Amount:      ev.Amount,
				BlockNumber: ev.Raw.BlockNumber,
				TxHash:      ev.Raw.TxHash,
				LogIndex:    ev.Raw.Index,
			}
		},
	)
}

// ErrBackoffLimitReached is returned when the listener exhausts its backoff retry budget.
var ErrBackoffLimitReached = errors.New("backoff limit reached")

// listenEvents polls for new confirmed blocks at pollInterval and only
// processes events from blocks that have at least confirmationBlocks on top.
type logHandler func(log types.Log)

func listenEvents(
	ctx context.Context,
	client bind.ContractBackend,
	subID string,
	contractAddress common.Address,
	confirmationBlocks uint64,
	pollInterval time.Duration,
	lastBlock uint64,
	lastIndex uint32,
	topics [][]common.Hash,
	handler logHandler,
) error {
	var backOffCount atomic.Uint64

	listenerLogger.Debugw("starting confirmed-block polling", "subID", subID, "confirmationBlocks", confirmationBlocks, "pollInterval", pollInterval)

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			listenerLogger.Infow("context cancelled, stopping listener", "subID", subID)
			return nil
		case <-ticker.C:
		}

		if !waitForBackOffTimeout(ctx, int(backOffCount.Load()), "confirmed-block poll") {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("%w: listener %s failed after %d consecutive errors", ErrBackoffLimitReached, subID, backOffCount.Load())
		}

		headerCtx, cancel := context.WithTimeout(ctx, 1*time.Minute)
		var header *types.Header
		err := debounce.Debounce(headerCtx, listenerLogger, func(ctx context.Context) error {
			var err error
			header, err = client.HeaderByNumber(ctx, nil)
			return err
		})
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			listenerLogger.Errorw("failed to get latest block", "error", err, "subID", subID)
			backOffCount.Add(1)
			continue
		}

		latestBlock := header.Number.Uint64()
		if latestBlock < confirmationBlocks {
			continue
		}
		safeBlock := latestBlock - confirmationBlocks

		if safeBlock <= lastBlock && lastBlock != 0 {
			continue
		}

		logsCh := make(chan types.Log, 1)
		go reconcileBlockRange(ctx, client, subID, contractAddress, safeBlock, lastBlock, lastIndex, topics, logsCh)

		for ethLog := range logsCh {
			handler(ethLog)
			lastBlock = ethLog.BlockNumber
			lastIndex = uint32(ethLog.Index)
		}

		// Advance the cursor to safeBlock only if no logs were emitted
		// in that block (otherwise lastBlock/lastIndex already point at
		// the precise last-emitted log and resetting lastIndex would
		// cause replays on the next cycle).
		if lastBlock < safeBlock {
			lastBlock = safeBlock
			lastIndex = 0
		}
		backOffCount.Store(0)
	}
}

func reconcileBlockRange(
	ctx context.Context,
	client bind.ContractBackend,
	subID string,
	contractAddress common.Address,
	currentBlock uint64,
	lastBlock uint64,
	lastIndex uint32,
	topics [][]common.Hash,
	historicalCh chan types.Log,
) {
	defer close(historicalCh)

	var backOffCount atomic.Uint64
	const blockStep = 10000
	startBlock := lastBlock
	endBlock := startBlock + blockStep

	for currentBlock > startBlock {
		if ctx.Err() != nil {
			return
		}
		if !waitForBackOffTimeout(ctx, int(backOffCount.Load()), "reconcile block range") {
			return
		}

		if endBlock > currentBlock {
			endBlock = currentBlock
		}

		fetchFQ := ethereum.FilterQuery{
			Addresses: []common.Address{contractAddress},
			FromBlock: new(big.Int).SetUint64(startBlock),
			ToBlock:   new(big.Int).SetUint64(endBlock),
			Topics:    topics,
		}

		var logs []types.Log
		var err error
		logsCtx, cancel := context.WithTimeout(ctx, 1*time.Minute)
		err = debounce.Debounce(logsCtx, listenerLogger, func(ctx context.Context) error {
			logs, err = client.FilterLogs(ctx, fetchFQ)
			return err
		})
		cancel()
		if err != nil {
			if strings.Contains(err.Error(), "Exceeded max range limit for eth_getLogs:") {
				newEndBlock := endBlock - (endBlock-startBlock)/2
				listenerLogger.Infow("eth_getLogs exceeded max range limit, reducing block range", "subID", subID, "startBlock", startBlock, "oldEndBlock", endBlock, "newEndBlock", newEndBlock)
				endBlock = newEndBlock
				continue
			}

			newStartBlock, newEndBlock, extractErr := extractAdvisedBlockRange(err.Error())
			if extractErr != nil {
				listenerLogger.Errorw("failed to filter logs", "error", err, "extractErr", extractErr, "subID", subID, "startBlock", startBlock, "endBlock", endBlock)
				backOffCount.Add(1)
				continue
			}
			startBlock, endBlock = newStartBlock, newEndBlock
			listenerLogger.Infow("retrying with advised block range", "subID", subID, "startBlock", startBlock, "endBlock", endBlock)
			continue
		}
		listenerLogger.Infow("fetched historical logs", "subID", subID, "count", len(logs), "startBlock", startBlock, "endBlock", endBlock)

		for _, ethLog := range logs {
			if ethLog.BlockNumber == lastBlock && ethLog.Index <= uint(lastIndex) {
				listenerLogger.Infow("skipping previously known event", "subID", subID, "blockNumber", ethLog.BlockNumber, "logIndex", ethLog.Index)
				continue
			}

			historicalCh <- ethLog
		}

		startBlock = endBlock + 1
		endBlock += blockStep
	}
}

func extractAdvisedBlockRange(msg string) (startBlock, endBlock uint64, err error) {
	if !strings.Contains(msg, "query returned more than 10000 results") {
		err = errors.New("error message doesn't contain advised block range")
		return
	}

	re := regexp.MustCompile(`\[0x([0-9a-fA-F]+), 0x([0-9a-fA-F]+)\]`)
	match := re.FindStringSubmatch(msg)
	if len(match) != 3 {
		err = errors.New("failed to extract block range from error message")
		return
	}

	startBlock, err = strconv.ParseUint(match[1], 16, 64)
	if err != nil {
		err = fmt.Errorf("failed to parse block range from error message: %w", err)
		return
	}
	endBlock, err = strconv.ParseUint(match[2], 16, 64)
	if err != nil {
		err = fmt.Errorf("failed to parse block range from error message: %w", err)
		return
	}
	return
}

func waitForBackOffTimeout(ctx context.Context, backOffCount int, originator string) bool {
	if backOffCount > maxBackOffCount {
		listenerLogger.Errorw("back off limit reached, exiting", "originator", originator, "backOffCount", backOffCount)
		return false
	}

	if backOffCount > 0 {
		listenerLogger.Infow("backing off", "originator", originator, "backOffCount", backOffCount)
		select {
		case <-time.After(time.Duration((1<<uint(backOffCount))-1) * time.Second):
		case <-ctx.Done():
			return false
		}
	}
	return true
}
