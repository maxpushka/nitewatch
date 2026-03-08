//go:build !short

package custody_test

import (
	"context"
	"crypto/ecdsa"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient/simulated"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/layer-3/nitewatch/custody"
)

const simChainID = 1337

// simClient wraps simulated.Client to satisfy custody.EthBackend.
type simClient struct {
	simulated.Client
	backend *simulated.Backend
}

func (c simClient) Close() { c.backend.Close() }

// thresholdEnv holds everything needed for ThresholdCustody integration tests.
type thresholdEnv struct {
	sim      *simulated.Backend
	client   custody.EthBackend
	keys     []*ecdsa.PrivateKey
	addrs    []common.Address
	auths    []*bind.TransactOpts
	contract *custody.ThresholdCustody
	addr     common.Address
	backend  *custody.SimulatedBackend
}

// newThresholdEnv deploys ThresholdCustody with the given number of signers and threshold.
// The last account (index = numSigners) is the user account.
func newThresholdEnv(t *testing.T, numSigners int, threshold uint64) *thresholdEnv {
	t.Helper()

	numAccounts := numSigners + 1 // signers + 1 user
	keys := make([]*ecdsa.PrivateKey, numAccounts)
	addrs := make([]common.Address, numAccounts)
	auths := make([]*bind.TransactOpts, numAccounts)
	alloc := make(types.GenesisAlloc)
	balance := new(big.Int).Mul(big.NewInt(1000), big.NewInt(1e18))

	for i := range numAccounts {
		key, err := crypto.GenerateKey()
		require.NoError(t, err)
		keys[i] = key
		addrs[i] = crypto.PubkeyToAddress(key.PublicKey)
		auth, err := bind.NewKeyedTransactorWithChainID(key, big.NewInt(simChainID))
		require.NoError(t, err)
		auths[i] = auth
		alloc[addrs[i]] = types.Account{Balance: balance}
	}

	sim := simulated.NewBackend(alloc)
	t.Cleanup(func() { sim.Close() })
	client := simClient{Client: sim.Client(), backend: sim}

	signerAddrs := addrs[:numSigners]
	contractAddr, tx, tcContract, err := custody.DeployThresholdCustody(
		copyAuth(auths[0]), client, signerAddrs, threshold,
	)
	require.NoError(t, err)
	sim.Commit()

	receipt, err := client.TransactionReceipt(context.Background(), tx.Hash())
	require.NoError(t, err)
	require.Equal(t, uint64(1), receipt.Status, "ThresholdCustody deployment failed")

	// Bind as IWithdraw for the SimulatedBackend adapter.
	iwContract, err := custody.NewIWithdraw(contractAddr, client)
	require.NoError(t, err)
	idContract, err := custody.NewIDeposit(contractAddr, client)
	require.NoError(t, err)

	backend := custody.NewSimulatedBackend(client, custody.BackendConfig{
		ContractAddress:    contractAddr,
		ConfirmationBlocks: 0,
	}, iwContract, idContract, sim.Commit)

	return &thresholdEnv{
		sim:      sim,
		client:   client,
		keys:     keys,
		addrs:    addrs,
		auths:    auths,
		contract: tcContract,
		addr:     contractAddr,
		backend:  backend,
	}
}

func (e *thresholdEnv) userAddr() common.Address { return e.addrs[len(e.addrs)-1] }
func (e *thresholdEnv) userAuth() *bind.TransactOpts {
	return e.auths[len(e.auths)-1]
}
func (e *thresholdEnv) signerAuth(i int) *bind.TransactOpts { return e.auths[i] }

// depositETH deposits native ETH into the custody contract from the user account.
func (e *thresholdEnv) depositETH(t *testing.T, amount *big.Int) {
	t.Helper()
	auth := copyAuth(e.userAuth())
	auth.Value = amount
	tx, err := e.contract.Deposit(auth, common.Address{}, amount)
	require.NoError(t, err)
	e.sim.Commit()
	receipt, err := e.client.TransactionReceipt(context.Background(), tx.Hash())
	require.NoError(t, err)
	require.Equal(t, uint64(1), receipt.Status, "deposit failed")
}

// extractWithdrawalID parses WithdrawStarted from a receipt to get the withdrawal ID.
func (e *thresholdEnv) extractWithdrawalID(t *testing.T, tx *types.Transaction) [32]byte {
	t.Helper()
	e.sim.Commit()
	receipt, err := e.client.TransactionReceipt(context.Background(), tx.Hash())
	require.NoError(t, err)
	require.Equal(t, uint64(1), receipt.Status, "startWithdraw tx failed")

	iwContract, err := custody.NewIWithdraw(e.addr, e.client)
	require.NoError(t, err)

	for _, log := range receipt.Logs {
		ev, parseErr := iwContract.ParseWithdrawStarted(*log)
		if parseErr == nil {
			return ev.WithdrawalId
		}
	}
	t.Fatal("WithdrawStarted event not found in receipt")
	return [32]byte{}
}

// ---------------------------------------------------------------------------
// Integration test: 2/3 signer happy path
// ---------------------------------------------------------------------------

func TestThresholdCustody_HappyPath_2of3(t *testing.T) {
	env := newThresholdEnv(t, 3, 2) // 3 signers, threshold=2

	depositAmount := big.NewInt(1e18)
	withdrawAmount := new(big.Int).Div(depositAmount, big.NewInt(2)) // 0.5 ETH

	// Step 1: User deposits 1 ETH.
	env.depositETH(t, depositAmount)

	// Record user balance before withdrawal.
	userBalBefore, err := env.client.(ethereum.ChainStateReader).BalanceAt(
		context.Background(), env.userAddr(), nil)
	require.NoError(t, err)

	// Step 2: Signer0 starts withdrawal via the Backend adapter.
	// StartWithdraw counts as the 1st approval (1/2).
	tx, err := env.backend.StartWithdraw(
		copyAuth(env.signerAuth(0)),
		env.userAddr(),
		common.Address{}, // native ETH
		withdrawAmount,
		big.NewInt(1),
	)
	require.NoError(t, err)

	withdrawalID := env.extractWithdrawalID(t, tx)
	t.Logf("withdrawalID: %x", withdrawalID)

	// Step 3: Signer1 finalizes via the Backend adapter.
	// This is the 2nd approval (2/2) → meets threshold → executes withdrawal.
	tx, err = env.backend.FinalizeWithdraw(
		copyAuth(env.signerAuth(1)),
		withdrawalID,
	)
	require.NoError(t, err)
	env.sim.Commit()

	receipt, err := env.client.TransactionReceipt(context.Background(), tx.Hash())
	require.NoError(t, err)
	require.Equal(t, uint64(1), receipt.Status, "finalizeWithdraw should succeed")

	// Step 4: Verify WithdrawFinalized(success=true) was emitted.
	iwContract, err := custody.NewIWithdraw(env.addr, env.client)
	require.NoError(t, err)

	iter, err := iwContract.FilterWithdrawFinalized(&bind.FilterOpts{
		Start:   0,
		Context: context.Background(),
	}, nil)
	require.NoError(t, err)
	defer iter.Close()

	require.True(t, iter.Next(), "expected WithdrawFinalized event")
	assert.True(t, iter.Event.Success, "withdrawal should be finalized as success")
	assert.Equal(t, withdrawalID, iter.Event.WithdrawalId)

	// Step 5: Verify user received the ETH.
	userBalAfter, err := env.client.(ethereum.ChainStateReader).BalanceAt(
		context.Background(), env.userAddr(), nil)
	require.NoError(t, err)

	expected := new(big.Int).Add(userBalBefore, withdrawAmount)
	assert.Equal(t, expected.String(), userBalAfter.String(),
		"user balance should increase by withdrawn amount")

	// Step 6: Verify the backend mode.
	assert.Equal(t, "simulated", env.backend.Mode())
}

// TestThresholdCustody_SingleSignerInsufficient verifies that a single
// approval does NOT trigger the withdrawal when threshold=2.
func TestThresholdCustody_SingleSignerInsufficient(t *testing.T) {
	env := newThresholdEnv(t, 3, 2)

	env.depositETH(t, big.NewInt(1e18))

	// Signer0 starts withdrawal (1/2 approvals).
	tx, err := env.backend.StartWithdraw(
		copyAuth(env.signerAuth(0)),
		env.userAddr(),
		common.Address{},
		big.NewInt(5e17),
		big.NewInt(1),
	)
	require.NoError(t, err)
	env.extractWithdrawalID(t, tx)

	// No WithdrawFinalized event should exist yet — only 1/2 approvals.
	iwContract, err := custody.NewIWithdraw(env.addr, env.client)
	require.NoError(t, err)

	iter, err := iwContract.FilterWithdrawFinalized(&bind.FilterOpts{
		Start:   0,
		Context: context.Background(),
	}, nil)
	require.NoError(t, err)
	defer iter.Close()

	assert.False(t, iter.Next(), "should NOT have WithdrawFinalized with only 1/2 approvals")
}

// TestThresholdCustody_ThirdSignerRedundant verifies that a 3rd signer
// calling FinalizeWithdraw on an already-finalized withdrawal reverts.
func TestThresholdCustody_ThirdSignerRedundant(t *testing.T) {
	env := newThresholdEnv(t, 3, 2)

	env.depositETH(t, big.NewInt(1e18))

	// Signer0 starts, signer1 finalizes.
	tx, err := env.backend.StartWithdraw(
		copyAuth(env.signerAuth(0)),
		env.userAddr(),
		common.Address{},
		big.NewInt(5e17),
		big.NewInt(1),
	)
	require.NoError(t, err)
	wID := env.extractWithdrawalID(t, tx)

	_, err = env.backend.FinalizeWithdraw(copyAuth(env.signerAuth(1)), wID)
	require.NoError(t, err)

	// Signer2 tries to finalize the already-finalized withdrawal — should revert.
	_, err = env.backend.FinalizeWithdraw(copyAuth(env.signerAuth(2)), wID)
	require.Error(t, err, "finalizing an already-finalized withdrawal should fail")
}

// TestSimulatedBackend_AutoCommit verifies that the SimulatedBackend
// auto-commits after successful write operations.
func TestSimulatedBackend_AutoCommit(t *testing.T) {
	env := newThresholdEnv(t, 3, 2)
	env.depositETH(t, big.NewInt(1e18))

	// StartWithdraw through the backend should auto-commit.
	tx, err := env.backend.StartWithdraw(
		copyAuth(env.signerAuth(0)),
		env.userAddr(),
		common.Address{},
		big.NewInt(5e17),
		big.NewInt(1),
	)
	require.NoError(t, err)

	// Receipt should be available immediately (auto-committed).
	receipt, err := env.client.TransactionReceipt(context.Background(), tx.Hash())
	require.NoError(t, err)
	assert.Equal(t, uint64(1), receipt.Status)
}

func copyAuth(auth *bind.TransactOpts) *bind.TransactOpts {
	cp := *auth
	return &cp
}
