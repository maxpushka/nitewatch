package main

import (
	"context"
	"fmt"
	"log/slog"
	"math/big"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"golang.org/x/term"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	nitewatch "github.com/layer-3/nitewatch"
	"github.com/layer-3/nitewatch/config"
	"github.com/layer-3/nitewatch/custody"
	"github.com/layer-3/nitewatch/internal/store"
	"github.com/layer-3/nitewatch/service"
)

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	switch os.Args[1] {
	case "version":
		fmt.Println(nitewatch.Version)
	case "worker":
		runWorker()
	case "deploy":
		cmdDeploy()
	case "deposit":
		cmdDeposit()
	case "start-withdraw":
		cmdStartWithdraw()
	case "finalize":
		cmdFinalize()
	case "reject":
		cmdReject()
	case "info":
		cmdInfo()
	case "withdrawal":
		cmdWithdrawal()
	case "balance":
		cmdBalance()
	case "signers":
		cmdSigners()
	case "is-signer":
		cmdIsSigner()
	case "rate-limit":
		cmdRateLimit()
	case "events":
		cmdEvents()
	case "db":
		cmdDB()
	default:
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Fprintln(os.Stderr, `usage: nitewatch <command> [args]

Service:
  worker                              Run the nitewatch daemon
  version                             Print version

Contract operations (require --rpc, --contract, --key):
  deploy    --rpc <url> --key <hex> --signers <addr,...> --threshold <n>
  deposit   --rpc <url> --contract <addr> --key <hex> --amount <wei> [--token <addr>]
  start-withdraw --rpc <url> --contract <addr> --key <hex> --user <addr> --amount <wei> --nonce <n> [--token <addr>]
  finalize  --rpc <url> --contract <addr> --key <hex> --id <bytes32>
  reject    --rpc <url> --contract <addr> --key <hex> --id <bytes32>

Read operations (require --rpc, --contract):
  info      --rpc <url> --contract <addr>
  withdrawal --rpc <url> --contract <addr> --id <bytes32>
  balance   --rpc <url> --contract <addr> [--token <addr>]
  signers   --rpc <url> --contract <addr>
  is-signer --rpc <url> --contract <addr> --address <addr>
  rate-limit --rpc <url> --contract <addr>
  events    --rpc <url> --contract <addr> [--from <block>]

Database helpers (require --db):
  db withdrawals --db <path>
  db cursors     --db <path>
  db events      --db <path>
  db pending     --db <path>`)
}

// ----------- flag helpers -----------

func flag(name string) string {
	for i, a := range os.Args {
		if a == "--"+name && i+1 < len(os.Args) {
			return os.Args[i+1]
		}
	}
	return ""
}

func requireFlag(name string) string {
	v := flag(name)
	if v == "" {
		fatal("missing required flag: --%s", name)
	}
	return v
}

func flagBigInt(name string) *big.Int {
	s := requireFlag(name)
	v, ok := new(big.Int).SetString(s, 10)
	if !ok {
		fatal("invalid integer for --%s: %s", name, s)
	}
	return v
}

func flagBytes32(name string) [32]byte {
	s := requireFlag(name)
	b := common.FromHex(s)
	if len(b) != 32 {
		fatal("--%s must be 32 bytes (got %d)", name, len(b))
	}
	var out [32]byte
	copy(out[:], b)
	return out
}

func flagAddress(name string) common.Address {
	s := requireFlag(name)
	if !common.IsHexAddress(s) {
		fatal("invalid address for --%s: %s", name, s)
	}
	return common.HexToAddress(s)
}

func tokenFlag() common.Address {
	s := flag("token")
	if s == "" {
		return common.Address{}
	}
	return common.HexToAddress(s)
}

func fatal(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, "error: "+format+"\n", args...)
	os.Exit(1)
}

// ----------- connection helpers -----------

func dial(rpcURL string) *ethclient.Client {
	client, err := ethclient.Dial(rpcURL)
	if err != nil {
		fatal("failed to connect to %s: %v", rpcURL, err)
	}
	return client
}

func makeAuth(client *ethclient.Client, keyHex string) *bind.TransactOpts {
	pk := strings.TrimPrefix(keyHex, "0x")
	key, err := crypto.HexToECDSA(pk)
	if err != nil {
		fatal("invalid private key: %v", err)
	}
	chainID, err := client.ChainID(context.Background())
	if err != nil {
		fatal("failed to get chain ID: %v", err)
	}
	auth, err := bind.NewKeyedTransactorWithChainID(key, chainID)
	if err != nil {
		fatal("failed to create transactor: %v", err)
	}
	return auth
}

// (waitTx removed — each command handles its own receipt)

// ----------- worker -----------

func runWorker() {
	conf, err := loadConfig()
	if err != nil {
		slog.Error("Failed to load configuration", "error", err)
		os.Exit(1)
	}

	if conf.Blockchain.PrivateKey == "" {
		fmt.Print("Enter private key: ")
		bytePassword, err := term.ReadPassword(int(syscall.Stdin))
		if err != nil {
			slog.Error("Failed to read private key", "error", err)
			os.Exit(1)
		}
		fmt.Println()
		conf.Blockchain.PrivateKey = strings.TrimSpace(string(bytePassword))
		if conf.Blockchain.PrivateKey == "" {
			slog.Error("Private key cannot be empty")
			os.Exit(1)
		}
	}

	logLevel := conf.SlogLevel()
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: logLevel,
	})))

	svc, err := service.New(*conf)
	if err != nil {
		slog.Error("Failed to create service", "error", err)
		os.Exit(1)
	}

	if err := svc.RunWorker(); err != nil {
		slog.Error("Worker failed", "error", err)
		os.Exit(1)
	}
}

func loadConfig() (*config.Config, error) {
	if raw := os.Getenv("NITEWATCH_CONFIG"); raw != "" {
		return config.LoadFromEnv(raw)
	}
	if configPath := os.Getenv("NITEWATCH_CONFIG_PATH"); configPath != "" {
		return config.Load(configPath)
	}
	return config.LoadFromEnv(string(config.DefaultConfig))
}

// ----------- deploy -----------

func cmdDeploy() {
	rpc := requireFlag("rpc")
	keyHex := requireFlag("key")
	signersStr := requireFlag("signers")
	thresholdStr := requireFlag("threshold")

	var threshold uint64
	if _, err := fmt.Sscanf(thresholdStr, "%d", &threshold); err != nil {
		fatal("invalid threshold: %s", thresholdStr)
	}

	var signers []common.Address
	for _, s := range strings.Split(signersStr, ",") {
		s = strings.TrimSpace(s)
		if !common.IsHexAddress(s) {
			fatal("invalid signer address: %s", s)
		}
		signers = append(signers, common.HexToAddress(s))
	}

	client := dial(rpc)
	defer client.Close()
	auth := makeAuth(client, keyHex)

	fmt.Printf("Deploying ThresholdCustody with %d signers, threshold=%d...\n", len(signers), threshold)
	addr, tx, _, err := custody.DeployThresholdCustody(auth, client, signers, threshold)
	if err != nil {
		fatal("deploy failed: %v", err)
	}
	fmt.Printf("tx: %s\n", tx.Hash().Hex())

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	receipt, err := bind.WaitMined(ctx, client, tx)
	if err != nil {
		fatal("deploy tx mining failed: %v", err)
	}
	if receipt.Status != 1 {
		fatal("deploy tx reverted")
	}
	fmt.Printf("contract: %s\n", addr.Hex())
	fmt.Printf("block:    %d\n", receipt.BlockNumber)
}

// ----------- deposit -----------

func cmdDeposit() {
	rpc := requireFlag("rpc")
	contractAddr := flagAddress("contract")
	keyHex := requireFlag("key")
	amount := flagBigInt("amount")
	token := tokenFlag()

	client := dial(rpc)
	defer client.Close()
	auth := makeAuth(client, keyHex)

	contract, err := custody.NewThresholdCustody(contractAddr, client)
	if err != nil {
		fatal("failed to bind contract: %v", err)
	}

	if token == (common.Address{}) {
		auth.Value = amount
	}

	tx, err := contract.Deposit(auth, token, amount)
	if err != nil {
		fatal("deposit failed: %v", err)
	}
	fmt.Printf("tx: %s\n", tx.Hash().Hex())

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	receipt, err := bind.WaitMined(ctx, client, tx)
	if err != nil {
		fatal("deposit tx mining failed: %v", err)
	}
	if receipt.Status != 1 {
		fatal("deposit tx reverted")
	}
	fmt.Printf("deposited %s wei (token=%s) in block %d\n", amount, token.Hex(), receipt.BlockNumber)
}

// ----------- start-withdraw -----------

func cmdStartWithdraw() {
	rpc := requireFlag("rpc")
	contractAddr := flagAddress("contract")
	keyHex := requireFlag("key")
	user := flagAddress("user")
	amount := flagBigInt("amount")
	nonce := flagBigInt("nonce")
	token := tokenFlag()

	client := dial(rpc)
	defer client.Close()
	auth := makeAuth(client, keyHex)

	contract, err := custody.NewIWithdraw(contractAddr, client)
	if err != nil {
		fatal("failed to bind contract: %v", err)
	}

	tx, err := contract.StartWithdraw(auth, user, token, amount, nonce)
	if err != nil {
		fatal("startWithdraw failed: %v", err)
	}
	fmt.Printf("tx: %s\n", tx.Hash().Hex())

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	receipt, err := bind.WaitMined(ctx, client, tx)
	if err != nil {
		fatal("tx mining failed: %v", err)
	}
	if receipt.Status != 1 {
		fatal("startWithdraw tx reverted")
	}

	// Parse withdrawal ID from logs.
	for _, log := range receipt.Logs {
		ev, parseErr := contract.ParseWithdrawStarted(*log)
		if parseErr == nil {
			fmt.Printf("withdrawal_id: 0x%x\n", ev.WithdrawalId)
			fmt.Printf("block: %d\n", receipt.BlockNumber)
			return
		}
	}
	fmt.Println("warning: WithdrawStarted event not found in receipt")
}

// ----------- finalize -----------

func cmdFinalize() {
	rpc := requireFlag("rpc")
	contractAddr := flagAddress("contract")
	keyHex := requireFlag("key")
	wID := flagBytes32("id")

	client := dial(rpc)
	defer client.Close()
	auth := makeAuth(client, keyHex)

	contract, err := custody.NewIWithdraw(contractAddr, client)
	if err != nil {
		fatal("failed to bind contract: %v", err)
	}

	tx, err := contract.FinalizeWithdraw(auth, wID)
	if err != nil {
		fatal("finalizeWithdraw failed: %v", err)
	}
	fmt.Printf("tx: %s\n", tx.Hash().Hex())

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	receipt, err := bind.WaitMined(ctx, client, tx)
	if err != nil {
		fatal("tx mining failed: %v", err)
	}
	if receipt.Status != 1 {
		fatal("finalizeWithdraw tx reverted")
	}

	// Check if withdrawal was executed.
	for _, log := range receipt.Logs {
		ev, parseErr := contract.ParseWithdrawFinalized(*log)
		if parseErr == nil {
			if ev.Success {
				fmt.Println("withdrawal executed (threshold met)")
			} else {
				fmt.Println("withdrawal rejected")
			}
			return
		}
	}
	fmt.Println("approval recorded (threshold not yet met)")
}

// ----------- reject -----------

func cmdReject() {
	rpc := requireFlag("rpc")
	contractAddr := flagAddress("contract")
	keyHex := requireFlag("key")
	wID := flagBytes32("id")

	client := dial(rpc)
	defer client.Close()
	auth := makeAuth(client, keyHex)

	contract, err := custody.NewIWithdraw(contractAddr, client)
	if err != nil {
		fatal("failed to bind contract: %v", err)
	}

	tx, err := contract.RejectWithdraw(auth, wID)
	if err != nil {
		fatal("rejectWithdraw failed: %v", err)
	}
	fmt.Printf("tx: %s\n", tx.Hash().Hex())

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	receipt, err := bind.WaitMined(ctx, client, tx)
	if err != nil {
		fatal("tx mining failed: %v", err)
	}
	if receipt.Status != 1 {
		fatal("rejectWithdraw tx reverted")
	}
	fmt.Println("withdrawal rejected")
}

// ----------- info -----------

func cmdInfo() {
	rpc := requireFlag("rpc")
	contractAddr := flagAddress("contract")

	client := dial(rpc)
	defer client.Close()

	contract, err := custody.NewThresholdCustodyCaller(contractAddr, client)
	if err != nil {
		fatal("failed to bind contract: %v", err)
	}

	threshold, err := contract.Threshold(nil)
	if err != nil {
		fatal("failed to get threshold: %v", err)
	}

	signerCount, err := contract.GetSignerCount(nil)
	if err != nil {
		fatal("failed to get signer count: %v", err)
	}

	nonce, err := contract.SignerNonce(nil)
	if err != nil {
		fatal("failed to get signer nonce: %v", err)
	}

	chainID, err := client.ChainID(context.Background())
	if err != nil {
		fatal("failed to get chain ID: %v", err)
	}

	fmt.Printf("contract:     %s\n", contractAddr.Hex())
	fmt.Printf("chain_id:     %s\n", chainID)
	fmt.Printf("threshold:    %d\n", threshold)
	fmt.Printf("signer_count: %s\n", signerCount)
	fmt.Printf("signer_nonce: %s\n", nonce)
}

// ----------- withdrawal -----------

func cmdWithdrawal() {
	rpc := requireFlag("rpc")
	contractAddr := flagAddress("contract")
	wID := flagBytes32("id")

	client := dial(rpc)
	defer client.Close()

	contract, err := custody.NewThresholdCustodyCaller(contractAddr, client)
	if err != nil {
		fatal("failed to bind contract: %v", err)
	}

	w, err := contract.Withdrawals(nil, wID)
	if err != nil {
		fatal("failed to get withdrawal: %v", err)
	}

	fmt.Printf("withdrawal_id:      0x%x\n", wID)
	fmt.Printf("user:               %s\n", w.User.Hex())
	fmt.Printf("token:              %s\n", w.Token.Hex())
	fmt.Printf("amount:             %s\n", w.Amount)
	fmt.Printf("finalized:          %t\n", w.Finalized)
	fmt.Printf("created_at:         %d\n", w.CreatedAt)
	fmt.Printf("required_threshold: %d\n", w.RequiredThreshold)
}

// ----------- balance -----------

func cmdBalance() {
	rpc := requireFlag("rpc")
	contractAddr := flagAddress("contract")
	token := tokenFlag()

	client := dial(rpc)
	defer client.Close()

	if token == (common.Address{}) {
		bal, err := client.BalanceAt(context.Background(), contractAddr, nil)
		if err != nil {
			fatal("failed to get ETH balance: %v", err)
		}
		fmt.Printf("ETH balance: %s wei\n", bal)
	} else {
		// balanceOf(address) selector = 0x70a08231
		selector := common.FromHex("70a08231")
		callData := append(selector, common.LeftPadBytes(contractAddr.Bytes(), 32)...)
		result, err := client.CallContract(context.Background(), ethereum.CallMsg{
			To:   &token,
			Data: callData,
		}, nil)
		if err != nil {
			fatal("failed to get token balance: %v", err)
		}
		bal := new(big.Int).SetBytes(result)
		fmt.Printf("token %s balance: %s\n", token.Hex(), bal)
	}
}

// ----------- signers -----------

func cmdSigners() {
	rpc := requireFlag("rpc")
	contractAddr := flagAddress("contract")

	client := dial(rpc)
	defer client.Close()

	contract, err := custody.NewThresholdCustodyCaller(contractAddr, client)
	if err != nil {
		fatal("failed to bind contract: %v", err)
	}

	count, err := contract.GetSignerCount(nil)
	if err != nil {
		fatal("failed to get signer count: %v", err)
	}

	signers, err := contract.GetSigners(nil, 0, ^uint64(0))
	if err != nil {
		fatal("failed to get signers: %v", err)
	}

	fmt.Printf("signer_count: %s\n", count)
	for i, s := range signers {
		if len(s) >= 20 {
			addr := common.BytesToAddress(s[:20])
			fmt.Printf("  [%d] %s\n", i, addr.Hex())
		} else {
			fmt.Printf("  [%d] 0x%x\n", i, s)
		}
	}
}

// ----------- is-signer -----------

func cmdIsSigner() {
	rpc := requireFlag("rpc")
	contractAddr := flagAddress("contract")
	addr := flagAddress("address")

	client := dial(rpc)
	defer client.Close()

	contract, err := custody.NewThresholdCustodyCaller(contractAddr, client)
	if err != nil {
		fatal("failed to bind contract: %v", err)
	}

	ok, err := contract.IsSigner0(nil, addr)
	if err != nil {
		fatal("isSigner call failed: %v", err)
	}

	if ok {
		fmt.Printf("%s is a signer\n", addr.Hex())
	} else {
		fmt.Printf("%s is NOT a signer\n", addr.Hex())
		os.Exit(1)
	}
}

// ----------- rate-limit -----------

func cmdRateLimit() {
	rpc := requireFlag("rpc")
	contractAddr := flagAddress("contract")

	client := dial(rpc)
	defer client.Close()

	contract, err := custody.NewThresholdCustodyCaller(contractAddr, client)
	if err != nil {
		fatal("failed to bind contract: %v", err)
	}

	capacity, _ := contract.BucketCapacity(nil)
	interval, _ := contract.RefillInterval(nil)
	available, _ := contract.AvailableTokens(nil)
	lastRefill, _ := contract.LastRefillTime(nil)

	fmt.Printf("bucket_capacity:  %s\n", capacity)
	fmt.Printf("refill_interval:  %s seconds\n", interval)
	fmt.Printf("available_tokens: %s\n", available)
	fmt.Printf("last_refill_time: %s\n", lastRefill)
}

// ----------- events -----------

func cmdEvents() {
	rpc := requireFlag("rpc")
	contractAddr := flagAddress("contract")

	fromBlock := uint64(0)
	if s := flag("from"); s != "" {
		if _, err := fmt.Sscanf(s, "%d", &fromBlock); err != nil {
			fatal("invalid --from block: %s", s)
		}
	}

	client := dial(rpc)
	defer client.Close()

	contract, err := custody.NewIWithdraw(contractAddr, client)
	if err != nil {
		fatal("failed to bind contract: %v", err)
	}

	opts := &bind.FilterOpts{Start: fromBlock, Context: context.Background()}

	// WithdrawStarted events.
	startIter, err := contract.FilterWithdrawStarted(opts, nil, nil, nil)
	if err != nil {
		fatal("failed to filter WithdrawStarted: %v", err)
	}
	for startIter.Next() {
		ev := startIter.Event
		fmt.Printf("WithdrawStarted  block=%d id=0x%x user=%s token=%s amount=%s nonce=%s\n",
			ev.Raw.BlockNumber, ev.WithdrawalId, ev.User.Hex(), ev.Token.Hex(), ev.Amount, ev.Nonce)
	}
	startIter.Close()

	// WithdrawFinalized events.
	finIter, err := contract.FilterWithdrawFinalized(opts, nil)
	if err != nil {
		fatal("failed to filter WithdrawFinalized: %v", err)
	}
	for finIter.Next() {
		ev := finIter.Event
		fmt.Printf("WithdrawFinalized block=%d id=0x%x success=%t\n",
			ev.Raw.BlockNumber, ev.WithdrawalId, ev.Success)
	}
	finIter.Close()
}

// ----------- db -----------

func cmdDB() {
	if len(os.Args) < 3 {
		fatal("usage: nitewatch db <withdrawals|cursors|events|pending> --db <path>")
	}
	sub := os.Args[2]
	dbPath := requireFlag("db")

	gormDB, err := gorm.Open(sqlite.Open(dbPath), &gorm.Config{})
	if err != nil {
		fatal("failed to open database: %v", err)
	}
	db, err := store.NewAdapter(gormDB)
	if err != nil {
		fatal("failed to init database: %v", err)
	}

	switch sub {
	case "withdrawals":
		dbWithdrawals(db)
	case "cursors":
		dbCursors(gormDB)
	case "events":
		dbEvents(gormDB)
	case "pending":
		dbPending(db)
	default:
		fatal("unknown db subcommand: %s", sub)
	}
}

func dbWithdrawals(db *store.Adapter) {
	// Query via gorm directly since Adapter doesn't expose list-all.
	var models []store.WithdrawalModel
	sqlDB := extractGormDB(db)
	if err := sqlDB.Order("timestamp desc").Limit(100).Find(&models).Error; err != nil {
		fatal("query failed: %v", err)
	}
	if len(models) == 0 {
		fmt.Println("no withdrawals recorded")
		return
	}
	for _, m := range models {
		fmt.Printf("id=%s user=%s token=%s amount=%s block=%d time=%s\n",
			m.WithdrawalID, m.User, m.Token, m.Amount, m.BlockNumber, m.Timestamp.Format(time.RFC3339))
	}
}

func dbCursors(db *gorm.DB) {
	var cursors []store.BlockCursorModel
	if err := db.Find(&cursors).Error; err != nil {
		fatal("query failed: %v", err)
	}
	if len(cursors) == 0 {
		fmt.Println("no cursors")
		return
	}
	for _, c := range cursors {
		fmt.Printf("stream=%s block=%d log_index=%d updated=%s\n",
			c.StreamName, c.BlockNumber, c.LogIndex, c.UpdatedAt.Format(time.RFC3339))
	}
}

func dbEvents(db *gorm.DB) {
	var events []store.WithdrawEventModel
	if err := db.Order("id desc").Limit(100).Find(&events).Error; err != nil {
		fatal("query failed: %v", err)
	}
	if len(events) == 0 {
		fmt.Println("no events")
		return
	}
	for _, e := range events {
		fmt.Printf("id=%d wid=%s decision=%s user=%s token=%s amount=%s reason=%q\n",
			e.ID, e.WithdrawalID, e.Decision, e.UserAddress, e.TokenAddress, e.Amount, e.Reason)
	}
}

func dbPending(db *store.Adapter) {
	rejections, err := db.GetPendingRejections()
	if err != nil {
		fatal("query failed: %v", err)
	}
	finalizations, err := db.GetPendingFinalizations()
	if err != nil {
		fatal("query failed: %v", err)
	}

	if len(rejections) == 0 && len(finalizations) == 0 {
		fmt.Println("no pending operations")
		return
	}

	for _, r := range rejections {
		fmt.Printf("pending_rejection  wid=%s reason=%q retries=%d\n", r.WithdrawalID, r.Reason, r.RetryCount)
	}
	for _, f := range finalizations {
		fmt.Printf("pending_finalization wid=%s user=%s amount=%s retries=%d\n",
			f.WithdrawalID, f.UserAddress, f.Amount, f.RetryCount)
	}
}

// extractGormDB gets the underlying *gorm.DB from an Adapter via JSON round-trip hack.
// This is only used for the CLI db commands.
func extractGormDB(db *store.Adapter) *gorm.DB {
	// Use the exported Save method to verify the adapter works, then access via reflection-free approach.
	// For CLI purposes, we just re-open the DB.
	_ = db
	dbPath := requireFlag("db")
	gormDB, err := gorm.Open(sqlite.Open(dbPath), &gorm.Config{})
	if err != nil {
		fatal("failed to reopen database: %v", err)
	}
	return gormDB
}

