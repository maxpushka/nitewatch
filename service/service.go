package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/gin-gonic/gin"
	"golang.org/x/sync/errgroup"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/layer-3/nitewatch/config"
	"github.com/layer-3/nitewatch/custody"
	"github.com/layer-3/nitewatch/internal/checker"
	"github.com/layer-3/nitewatch/internal/store"
)

// gasEstimateBufferPercent is applied on top of eth_estimateGas results.
// The EVM's 63/64 gas rule (EIP-150) for CALL instructions means the minimum
// gas *limit* can be significantly higher than the gas *consumed*. When
// on-chain state changes between estimation and mining (e.g., another signer's
// approval shifts finalizeWithdraw from the "record approval" path into the
// "execute withdrawal + ETH/ERC20 transfer" path), the gas deficit can reach
// ~33% for ETH and ~67% for ERC20 transfers. A 75% buffer covers both with
// headroom.
const gasEstimateBufferPercent = 75

// txMiningTimeout is the maximum time to wait for a transaction to be mined.
const txMiningTimeout = 5 * time.Minute

// maxDeferredRetries is the maximum number of retry attempts for deferred
// rejections and finalizations before marking them as permanently failed.
const maxDeferredRetries = 25

type httpServer struct {
	Engine *gin.Engine
	server *http.Server
}

func newHTTPServer(addr string) *httpServer {
	engine := gin.New()
	engine.Use(gin.Recovery())
	return &httpServer{
		Engine: engine,
		server: &http.Server{Addr: addr, Handler: engine},
	}
}

func (s *httpServer) Run() error                         { return s.server.ListenAndServe() }
func (s *httpServer) Shutdown(ctx context.Context) error { return s.server.Shutdown(ctx) }

type Service struct {
	Config config.Config
	Logger *slog.Logger

	web       *httpServer
	ethClient custody.EthBackend
	contract  *custody.IWithdraw
	listener  *custody.Listener
	auth      *bind.TransactOpts
	checker   *checker.Checker
	store     *store.Adapter

	// txMu serializes transaction submissions to prevent nonce contention.
	txMu sync.Mutex

	workerReady int32
}

// New creates a Service that dials an Ethereum node via the configured RPC URL.
func New(conf config.Config) (*Service, error) {
	client, err := ethclient.Dial(conf.Blockchain.RPCURL)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to Ethereum RPC: %w", err)
	}

	svc, err := NewWithBackend(conf, client)
	if err != nil {
		client.Close()
		return nil, err
	}
	return svc, nil
}

// NewWithBackend creates a Service using a pre-existing Ethereum backend.
// The caller is responsible for closing the backend when done.
func NewWithBackend(conf config.Config, client custody.EthBackend) (*Service, error) {
	if conf.Blockchain.ConfirmationBlocks == 0 {
		return nil, fmt.Errorf("confirmation_blocks must be > 0")
	}

	logLevel := conf.SlogLevel()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: logLevel,
	})).With("service", "nitewatch")

	logger.Info("Logger initialized", "level", logLevel.String())

	srv := newHTTPServer(conf.ListenAddr)

	gormDB, err := gorm.Open(sqlite.Open(conf.DBPath), &gorm.Config{})
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	db, err := store.NewAdapter(gormDB)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize database: %w", err)
	}

	globalLimits, err := parseLimitsConfig(conf.Limits)
	if err != nil {
		return nil, fmt.Errorf("failed to parse global limits: %w", err)
	}

	userOverrides, err := parseUserOverrides(conf.PerUserOverrides)
	if err != nil {
		return nil, fmt.Errorf("failed to parse per-user overrides: %w", err)
	}

	chk := checker.New(globalLimits, userOverrides, db)

	chainID, err := client.ChainID(context.Background())
	if err != nil {
		return nil, fmt.Errorf("failed to get chain ID: %w", err)
	}

	pk := strings.TrimPrefix(conf.Blockchain.PrivateKey, "0x")
	key, err := crypto.HexToECDSA(pk)
	if err != nil {
		return nil, fmt.Errorf("failed to parse private key: %w", err)
	}

	auth, err := bind.NewKeyedTransactorWithChainID(key, chainID)
	if err != nil {
		return nil, fmt.Errorf("failed to create transactor: %w", err)
	}

	addr := common.HexToAddress(conf.Blockchain.ContractAddr)

	// Verify that the configured private key's address is authorized on the
	// contract before starting. This catches misconfiguration early rather
	// than failing on the first finalizeWithdraw/rejectWithdraw call.
	if err := verifySigner(client, addr, auth.From, logger); err != nil {
		return nil, err
	}

	withdrawContract, err := custody.NewIWithdraw(addr, client)
	if err != nil {
		return nil, fmt.Errorf("failed to bind IWithdraw contract: %w", err)
	}

	listener := custody.NewListener(client, addr, conf.Blockchain.ConfirmationBlocks, conf.Blockchain.PollInterval, withdrawContract, nil)

	return &Service{
		Config:    conf,
		Logger:    logger,
		web:       srv,
		ethClient: client,
		contract:  withdrawContract,
		listener:  listener,
		auth:      auth,
		checker:   chk,
		store:     db,
	}, nil
}

func (svc *Service) IsWorkerReady() bool {
	return atomic.LoadInt32(&svc.workerReady) == 1
}

func (svc *Service) setWorkerReady() {
	atomic.StoreInt32(&svc.workerReady, 1)
}

func (svc *Service) RunWorker() error {
	return svc.RunWorkerWithContext(context.Background())
}

func (svc *Service) RunWorkerWithContext(ctx context.Context) error {
	ctx, cancel := signal.NotifyContext(ctx, os.Interrupt)
	defer cancel()

	g, ctx := errgroup.WithContext(ctx)

	g.Go(func() error {
		svc.Logger.Info("Starting health endpoint server")
		return svc.web.Run()
	})

	// Watch WithdrawStarted events and process them.
	g.Go(func() error {
		fromBlock, fromLogIdx, err := svc.store.GetCursor("withdraw_started")
		if err != nil {
			svc.Logger.Warn("Failed to read withdraw_started cursor, starting from head", "error", err)
		}
		if fromBlock == 0 && svc.Config.Blockchain.StartBlock > 0 {
			fromBlock = svc.Config.Blockchain.StartBlock
		}

		svc.Logger.Info("Starting WithdrawStarted event watcher", "from_block", fromBlock, "from_log_index", fromLogIdx)
		withdrawals := make(chan *custody.WithdrawStartedEvent)

		// Capture listener error from goroutine.
		listenerErrCh := make(chan error, 1)
		go func() {
			listenerErrCh <- svc.listener.WatchWithdrawStarted(ctx, withdrawals, fromBlock, fromLogIdx)
		}()

		for event := range withdrawals {
			svc.processWithdrawal(ctx, event)
		}

		// Channel closed — check if it was a listener error or context cancellation.
		if err := <-listenerErrCh; err != nil {
			svc.Logger.Error("WithdrawStarted listener failed", "error", err)
			return fmt.Errorf("withdraw_started listener: %w", err)
		}
		return nil
	})

	// Watch WithdrawFinalized events to track all finalized withdrawals
	// (including those triggered by other signers) for accurate rate limiting.
	g.Go(func() error {
		fromBlock, fromLogIdx, err := svc.store.GetCursor("withdraw_finalized")
		if err != nil {
			svc.Logger.Warn("Failed to read withdraw_finalized cursor, starting from head", "error", err)
		}
		if fromBlock == 0 && svc.Config.Blockchain.StartBlock > 0 {
			fromBlock = svc.Config.Blockchain.StartBlock
		}

		svc.Logger.Info("Starting WithdrawFinalized event watcher", "from_block", fromBlock, "from_log_index", fromLogIdx)
		finalized := make(chan *custody.WithdrawFinalizedEvent)

		listenerErrCh := make(chan error, 1)
		go func() {
			listenerErrCh <- svc.listener.WatchWithdrawFinalized(ctx, finalized, fromBlock, fromLogIdx)
		}()

		for event := range finalized {
			svc.processFinalized(ctx, event)
		}

		if err := <-listenerErrCh; err != nil {
			svc.Logger.Error("WithdrawFinalized listener failed", "error", err)
			return fmt.Errorf("withdraw_finalized listener: %w", err)
		}
		return nil
	})

	// Process deferred rejections and finalizations.
	g.Go(func() error {
		svc.Logger.Info("Starting deferred processor")
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return nil
			case <-ticker.C:
				svc.processDeferredRejections(ctx)
				svc.processDeferredFinalizations(ctx)
			}
		}
	})

	g.Go(func() error {
		<-ctx.Done()
		svc.Logger.Info("Shutting down health endpoint server")
		ctxShutdown, cancelShutdown := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancelShutdown()
		return svc.web.Shutdown(ctxShutdown)
	})

	g.Go(func() error {
		<-ctx.Done()
		svc.Logger.Info("Closing Ethereum client")
		svc.ethClient.Close()
		return nil
	})

	svc.setWorkerReady()
	svc.Logger.Info("Worker started")

	return g.Wait()
}

// sendTx serializes transaction submission through txMu to prevent nonce contention.
func (svc *Service) sendTx(ctx context.Context, send func(auth *bind.TransactOpts) (*types.Transaction, error)) (*types.Transaction, error) {
	svc.txMu.Lock()
	defer svc.txMu.Unlock()

	txAuth := *svc.auth
	txAuth.Context = ctx
	return send(&txAuth)
}

// waitMined waits for a transaction to be mined with a timeout.
func (svc *Service) waitMined(ctx context.Context, tx *types.Transaction) (*types.Receipt, error) {
	mineCtx, cancel := context.WithTimeout(ctx, txMiningTimeout)
	defer cancel()
	return bind.WaitMined(mineCtx, svc.ethClient, tx)
}

// finalizeWithdrawWithGasBuffer sends a FinalizeWithdraw transaction with a
// gas limit buffer above the eth_estimateGas result. This prevents "out of
// gas" reverts when on-chain state changes between estimation and mining.
func (svc *Service) finalizeWithdrawWithGasBuffer(txAuth *bind.TransactOpts, withdrawalID [32]byte) (*types.Transaction, error) {
	dryRun := *txAuth
	dryRun.NoSend = true
	estTx, err := svc.contract.FinalizeWithdraw(&dryRun, withdrawalID)
	if err != nil {
		return nil, err
	}
	txAuth.GasLimit = estTx.Gas() * (100 + gasEstimateBufferPercent) / 100
	return svc.contract.FinalizeWithdraw(txAuth, withdrawalID)
}

// rejectWithdrawWithGasBuffer sends a RejectWithdraw transaction with a gas
// limit buffer. See finalizeWithdrawWithGasBuffer for rationale.
func (svc *Service) rejectWithdrawWithGasBuffer(txAuth *bind.TransactOpts, withdrawalID [32]byte) (*types.Transaction, error) {
	dryRun := *txAuth
	dryRun.NoSend = true
	estTx, err := svc.contract.RejectWithdraw(&dryRun, withdrawalID)
	if err != nil {
		return nil, err
	}
	txAuth.GasLimit = estTx.Gas() * (100 + gasEstimateBufferPercent) / 100
	return svc.contract.RejectWithdraw(txAuth, withdrawalID)
}

func (svc *Service) processWithdrawal(ctx context.Context, event *custody.WithdrawStartedEvent) {
	wID := common.Hash(event.WithdrawalID).Hex()
	logger := svc.Logger.With(
		"withdrawal_id", wID,
		"user", event.User.Hex(),
		"token", event.Token.Hex(),
		"amount", event.Amount,
	)

	if svc.store.HasWithdrawEvent(wID) {
		logger.Debug("Event already processed, skipping")
		return
	}

	logger.Info("Processing withdrawal request")

	baseModel := store.WithdrawEventModel{
		WithdrawalID: wID,
		UserAddress:  event.User.Hex(),
		TokenAddress: event.Token.Hex(),
		Amount:       event.Amount.String(),
		BlockNumber:  event.BlockNumber,
		TxHash:       event.TxHash.Hex(),
		LogIndex:     uint(event.LogIndex),
	}

	if err := svc.checker.Check(event.User, event.Token, event.Amount); err != nil {
		logger.Warn("Withdrawal blocked by policy, rejecting", "reason", err)

		tx, txErr := svc.sendTx(ctx, func(auth *bind.TransactOpts) (*types.Transaction, error) {
			return svc.rejectWithdrawWithGasBuffer(auth, event.WithdrawalID)
		})
		if txErr != nil {
			// Rejection may fail if the contract requires expiry (ThresholdCustody).
			// Schedule a deferred retry.
			logger.Warn("Immediate reject failed, deferring until expiry", "error", txErr)
			pending := &store.PendingRejectionModel{
				WithdrawalID: wID,
				Reason:       err.Error(),
			}
			if dbErr := svc.store.SavePendingRejection(pending); dbErr != nil {
				logger.Error("Failed to save pending rejection", "error", dbErr)
			}
			baseModel.Decision = "rejected"
			baseModel.Reason = err.Error()
			svc.recordEvent(logger, &baseModel)
			return
		}

		logger.Info("Sent reject transaction", "tx_hash", tx.Hash().Hex())
		receipt, txErr := svc.waitMined(ctx, tx)
		if txErr != nil {
			logger.Error("Failed waiting for reject tx to be mined", "error", txErr)
			baseModel.Decision = "error"
			baseModel.Reason = fmt.Sprintf("reject tx mining failed: %v", txErr)
			svc.recordEvent(logger, &baseModel)
			return
		}

		if receipt.Status == 1 {
			baseModel.Decision = "rejected"
			baseModel.Reason = err.Error()
		} else {
			// On-chain revert (e.g. WithdrawalNotExpired). Defer the rejection.
			logger.Warn("Reject tx reverted on-chain, deferring until expiry")
			pending := &store.PendingRejectionModel{
				WithdrawalID: wID,
				Reason:       err.Error(),
			}
			if dbErr := svc.store.SavePendingRejection(pending); dbErr != nil {
				logger.Error("Failed to save pending rejection", "error", dbErr)
			}
			baseModel.Decision = "rejected"
			baseModel.Reason = err.Error()
		}
		svc.recordEvent(logger, &baseModel)
		return
	}

	tx, err := svc.sendTx(ctx, func(auth *bind.TransactOpts) (*types.Transaction, error) {
		return svc.finalizeWithdrawWithGasBuffer(auth, event.WithdrawalID)
	})
	if err != nil {
		logger.Error("Failed to finalize withdrawal", "error", err)
		baseModel.Decision = "error"
		baseModel.Reason = fmt.Sprintf("finalize tx failed: %v", err)

		// Schedule a deferred retry for the finalization.
		pending := &store.PendingFinalizationModel{
			WithdrawalID: wID,
			UserAddress:  event.User.Hex(),
			TokenAddress: event.Token.Hex(),
			Amount:       event.Amount.String(),
		}
		if dbErr := svc.store.SavePendingFinalization(pending); dbErr != nil {
			logger.Error("Failed to save pending finalization", "error", dbErr)
		}

		// Record without advancing cursor so re-processing is possible.
		svc.recordEventRecoverable(logger, &baseModel)
		return
	}

	logger.Info("Sent finalize transaction", "tx_hash", tx.Hash().Hex())

	receipt, err := svc.waitMined(ctx, tx)
	if err != nil {
		logger.Error("Transaction mining failed", "error", err)
		baseModel.Decision = "error"
		baseModel.Reason = fmt.Sprintf("finalize tx mining failed: %v", err)

		// Schedule a deferred retry.
		pending := &store.PendingFinalizationModel{
			WithdrawalID: wID,
			UserAddress:  event.User.Hex(),
			TokenAddress: event.Token.Hex(),
			Amount:       event.Amount.String(),
		}
		if dbErr := svc.store.SavePendingFinalization(pending); dbErr != nil {
			logger.Error("Failed to save pending finalization", "error", dbErr)
		}

		svc.recordEventRecoverable(logger, &baseModel)
		return
	}

	if receipt.Status != 1 {
		logger.Error("Withdrawal finalization tx reverted")
		baseModel.Decision = "error"
		baseModel.Reason = "finalize tx reverted on-chain"

		// Schedule retry — may have been a transient state issue.
		pending := &store.PendingFinalizationModel{
			WithdrawalID: wID,
			UserAddress:  event.User.Hex(),
			TokenAddress: event.Token.Hex(),
			Amount:       event.Amount.String(),
		}
		if dbErr := svc.store.SavePendingFinalization(pending); dbErr != nil {
			logger.Error("Failed to save pending finalization", "error", dbErr)
		}

		svc.recordEventRecoverable(logger, &baseModel)
		return
	}

	// Check receipt logs for WithdrawFinalized event to confirm actual execution.
	// In ThresholdCustody, finalizeWithdraw adds an approval; the withdrawal only
	// executes when the threshold is met and emits WithdrawFinalized.
	executed := false
	for _, log := range receipt.Logs {
		finalized, parseErr := svc.contract.ParseWithdrawFinalized(*log)
		if parseErr != nil {
			continue
		}
		if finalized.WithdrawalId == event.WithdrawalID && finalized.Success {
			executed = true
			break
		}
	}

	if executed {
		logger.Info("Withdrawal finalized successfully on-chain")

		record := &custody.Withdrawal{
			WithdrawalID: event.WithdrawalID,
			User:         event.User,
			Token:        event.Token,
			Amount:       event.Amount,
			BlockNumber:  receipt.BlockNumber.Uint64(),
			TxHash:       tx.Hash(),
			Timestamp:    time.Now(),
		}
		if err := svc.checker.Record(record); err != nil {
			logger.Error("Failed to record withdrawal in DB", "error", err)
		}

		baseModel.Decision = "approved"
		svc.recordEvent(logger, &baseModel)
	} else {
		logger.Info("Approval recorded on-chain, threshold not yet met")
		baseModel.Decision = "pending"
		baseModel.Reason = "approval added, awaiting threshold"
		svc.recordEvent(logger, &baseModel)
	}
}

// processFinalized handles WithdrawFinalized events from any signer.
// This ensures the local DB accurately tracks all finalized withdrawals
// for rate limiting, and cleans up stale deferred rejections.
func (svc *Service) processFinalized(_ context.Context, event *custody.WithdrawFinalizedEvent) {
	wID := common.Hash(event.WithdrawalID).Hex()
	logger := svc.Logger.With("withdrawal_id", wID, "success", event.Success)

	logger.Debug("Processing WithdrawFinalized event")

	// Cancel any pending deferred rejection for this withdrawal.
	if err := svc.store.CompletePendingRejection(wID); err != nil {
		logger.Debug("No pending rejection to cancel", "error", err)
	}

	// Cancel any pending deferred finalization.
	if err := svc.store.CompletePendingFinalization(wID); err != nil {
		logger.Debug("No pending finalization to cancel", "error", err)
	}

	// If the withdrawal was finalized successfully and we had it as pending,
	// update the event decision.
	if event.Success {
		if err := svc.store.UpdateWithdrawEventDecision(wID, "approved", "finalized by quorum"); err != nil {
			logger.Debug("No pending event to update", "error", err)
		}
	}

	// Update cursor for withdraw_finalized stream.
	ev := &store.WithdrawEventModel{
		WithdrawalID: wID,
		BlockNumber:  event.BlockNumber,
		TxHash:       event.TxHash.Hex(),
		LogIndex:     event.LogIndex,
	}
	// Use a separate cursor for this stream via a direct upsert.
	if err := svc.store.UpsertFinalizedCursor(event.BlockNumber, event.LogIndex); err != nil {
		logger.Error("Failed to update withdraw_finalized cursor", "error", err)
	}
	_ = ev // cursor updated above
}

func (svc *Service) processDeferredRejections(ctx context.Context) {
	pending, err := svc.store.GetPendingRejections()
	if err != nil {
		svc.Logger.Error("Failed to get pending rejections", "error", err)
		return
	}

	for _, p := range pending {
		logger := svc.Logger.With("withdrawal_id", p.WithdrawalID, "reason", p.Reason, "retry_count", p.RetryCount)

		if p.RetryCount >= maxDeferredRetries {
			logger.Error("Deferred rejection exceeded max retries, marking completed")
			if err := svc.store.CompletePendingRejection(p.WithdrawalID); err != nil {
				logger.Error("Failed to mark pending rejection as completed", "error", err)
			}
			continue
		}

		var wID [32]byte
		copy(wID[:], common.FromHex(p.WithdrawalID))

		tx, txErr := svc.sendTx(ctx, func(auth *bind.TransactOpts) (*types.Transaction, error) {
			return svc.rejectWithdrawWithGasBuffer(auth, wID)
		})
		if txErr != nil {
			// Will retry on next tick; may still be before expiry or already finalized.
			logger.Debug("Deferred reject tx failed (may not be expired yet)", "error", txErr)
			if err := svc.store.IncrementRejectionRetry(p.WithdrawalID); err != nil {
				logger.Error("Failed to increment rejection retry count", "error", err)
			}
			continue
		}

		logger.Info("Sent deferred reject transaction", "tx_hash", tx.Hash().Hex())
		receipt, txErr := svc.waitMined(ctx, tx)
		if txErr != nil {
			logger.Error("Deferred reject tx mining failed", "error", txErr)
			if err := svc.store.IncrementRejectionRetry(p.WithdrawalID); err != nil {
				logger.Error("Failed to increment rejection retry count", "error", err)
			}
			continue
		}

		if receipt.Status == 1 {
			logger.Info("Deferred rejection finalized on-chain")
		} else {
			logger.Warn("Deferred rejection tx reverted (may already be finalized)")
		}

		if err := svc.store.CompletePendingRejection(p.WithdrawalID); err != nil {
			logger.Error("Failed to mark pending rejection as completed", "error", err)
		}
	}
}

// processDeferredFinalizations retries failed finalization attempts.
func (svc *Service) processDeferredFinalizations(ctx context.Context) {
	pending, err := svc.store.GetPendingFinalizations()
	if err != nil {
		svc.Logger.Error("Failed to get pending finalizations", "error", err)
		return
	}

	for _, p := range pending {
		logger := svc.Logger.With("withdrawal_id", p.WithdrawalID, "retry_count", p.RetryCount)

		if p.RetryCount >= maxDeferredRetries {
			logger.Error("Deferred finalization exceeded max retries, marking completed")
			if err := svc.store.CompletePendingFinalization(p.WithdrawalID); err != nil {
				logger.Error("Failed to mark pending finalization as completed", "error", err)
			}
			continue
		}

		var wID [32]byte
		copy(wID[:], common.FromHex(p.WithdrawalID))

		tx, txErr := svc.sendTx(ctx, func(auth *bind.TransactOpts) (*types.Transaction, error) {
			return svc.finalizeWithdrawWithGasBuffer(auth, wID)
		})
		if txErr != nil {
			if isContractRevert(txErr) {
				// Contract reverted — withdrawal may already be finalized or expired.
				logger.Info("Deferred finalization reverted, marking completed", "error", txErr)
				if err := svc.store.CompletePendingFinalization(p.WithdrawalID); err != nil {
					logger.Error("Failed to mark pending finalization as completed", "error", err)
				}
				continue
			}
			logger.Warn("Deferred finalize tx failed", "error", txErr)
			if err := svc.store.IncrementFinalizationRetry(p.WithdrawalID); err != nil {
				logger.Error("Failed to increment finalization retry count", "error", err)
			}
			continue
		}

		logger.Info("Sent deferred finalize transaction", "tx_hash", tx.Hash().Hex())
		receipt, txErr := svc.waitMined(ctx, tx)
		if txErr != nil {
			logger.Error("Deferred finalize tx mining failed", "error", txErr)
			if err := svc.store.IncrementFinalizationRetry(p.WithdrawalID); err != nil {
				logger.Error("Failed to increment finalization retry count", "error", err)
			}
			continue
		}

		if receipt.Status == 1 {
			logger.Info("Deferred finalization succeeded on-chain")

			// Check if this tx actually triggered the withdrawal execution.
			for _, log := range receipt.Logs {
				finalized, parseErr := svc.contract.ParseWithdrawFinalized(*log)
				if parseErr != nil {
					continue
				}
				if finalized.WithdrawalId == wID && finalized.Success {
					amount, ok := new(big.Int).SetString(p.Amount, 10)
					if ok {
						record := &custody.Withdrawal{
							WithdrawalID: wID,
							User:         common.HexToAddress(p.UserAddress),
							Token:        common.HexToAddress(p.TokenAddress),
							Amount:       amount,
							BlockNumber:  receipt.BlockNumber.Uint64(),
							TxHash:       tx.Hash(),
							Timestamp:    time.Now(),
						}
						if err := svc.checker.Record(record); err != nil {
							logger.Error("Failed to record withdrawal in DB", "error", err)
						}
					}
					break
				}
			}

			// Update the event record if it exists.
			if err := svc.store.UpdateWithdrawEventDecision(p.WithdrawalID, "approved", "deferred finalization succeeded"); err != nil {
				logger.Debug("No event to update for deferred finalization", "error", err)
			}
		} else {
			logger.Warn("Deferred finalization tx reverted on-chain")
			if err := svc.store.IncrementFinalizationRetry(p.WithdrawalID); err != nil {
				logger.Error("Failed to increment finalization retry count", "error", err)
			}
			continue
		}

		if err := svc.store.CompletePendingFinalization(p.WithdrawalID); err != nil {
			logger.Error("Failed to mark pending finalization as completed", "error", err)
		}
	}
}

func (svc *Service) recordEvent(logger *slog.Logger, ev *store.WithdrawEventModel) {
	if err := svc.store.RecordWithdrawEvent(ev); err != nil {
		logger.Error("Failed to record withdraw event", "error", err)
	}
}

// recordEventRecoverable records the event without advancing the cursor,
// allowing re-processing on restart for recoverable errors.
func (svc *Service) recordEventRecoverable(logger *slog.Logger, ev *store.WithdrawEventModel) {
	if err := svc.store.RecordWithdrawEventOnly(ev); err != nil {
		logger.Error("Failed to record withdraw event", "error", err)
	}
}

func parseLimitsConfig(lc config.LimitsConfig) (map[common.Address]checker.Limit, error) {
	limits := make(map[common.Address]checker.Limit)
	for addrStr, conf := range lc {
		if !common.IsHexAddress(addrStr) {
			return nil, fmt.Errorf("invalid address: %s", addrStr)
		}
		addr := common.HexToAddress(addrStr)

		l := checker.Limit{}
		if conf.Hourly != "" {
			val, ok := new(big.Int).SetString(conf.Hourly, 10)
			if !ok {
				return nil, fmt.Errorf("invalid hourly limit for %s: %s", addrStr, conf.Hourly)
			}
			l.Hourly = val
		}
		if conf.Daily != "" {
			val, ok := new(big.Int).SetString(conf.Daily, 10)
			if !ok {
				return nil, fmt.Errorf("invalid daily limit for %s: %s", addrStr, conf.Daily)
			}
			l.Daily = val
		}
		limits[addr] = l
	}
	return limits, nil
}

// verifySigner checks that signerAddr is authorized on the custody contract
// by calling isSigner(address) via the ThresholdCustody binding.
func verifySigner(client custody.EthBackend, contract common.Address, signerAddr common.Address, logger *slog.Logger) error {
	caller, err := custody.NewThresholdCustodyCaller(contract, client)
	if err != nil {
		return fmt.Errorf("failed to bind ThresholdCustody caller: %w", err)
	}

	ok, err := caller.IsSigner0(nil, signerAddr)
	if err != nil {
		if isContractRevert(err) {
			// The contract does not support isSigner (e.g. SimpleCustody in tests).
			logger.Warn("Contract does not support isSigner, skipping signer verification",
				"address", signerAddr.Hex(), "contract", contract.Hex())
			return nil
		}

		// Transient RPC/network error. Retry once before giving up.
		logger.Warn("isSigner call failed, retrying once", "error", err)
		time.Sleep(2 * time.Second)

		ok, err = caller.IsSigner0(nil, signerAddr)
		if err != nil {
			return fmt.Errorf("failed to verify signer address (RPC error): %w", err)
		}
	}

	if !ok {
		return fmt.Errorf("address %s is not a registered signer on contract %s", signerAddr.Hex(), contract.Hex())
	}

	logger.Info("Signer address verified via isSigner", "address", signerAddr.Hex())
	return nil
}

// isContractRevert reports whether err indicates the EVM reverted execution
// (JSON-RPC error code 3). This covers unrecognized function selectors,
// explicit require/revert failures, and any other on-chain revert.
func isContractRevert(err error) bool {
	var rpcErr rpc.Error
	return errors.As(err, &rpcErr) && rpcErr.ErrorCode() == 3
}

func parseUserOverrides(overrides map[string]config.LimitsConfig) (map[common.Address]map[common.Address]checker.Limit, error) {
	result := make(map[common.Address]map[common.Address]checker.Limit)
	for userAddrStr, tokenLimits := range overrides {
		if !common.IsHexAddress(userAddrStr) {
			return nil, fmt.Errorf("invalid user address in per_user_overrides: %s", userAddrStr)
		}
		userAddr := common.HexToAddress(userAddrStr)
		parsed, err := parseLimitsConfig(tokenLimits)
		if err != nil {
			return nil, fmt.Errorf("per-user overrides for %s: %w", userAddrStr, err)
		}
		result[userAddr] = parsed
	}
	return result, nil
}
