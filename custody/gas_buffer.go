package custody

import (
	"context"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

// GasEstimateBufferPercent is applied on top of eth_estimateGas results.
// The EVM's 63/64 gas rule (EIP-150) for CALL instructions means the minimum
// gas *limit* can be significantly higher than the gas *consumed*. When
// on-chain state changes between estimation and mining (e.g., another signer's
// approval shifts finalizeWithdraw from the "record approval" path into the
// "execute withdrawal + ETH/ERC20 transfer" path), the gas deficit can reach
// ~33% for ETH and ~67% for ERC20 transfers. A 75% buffer covers both with
// headroom.
const GasEstimateBufferPercent = 75

// GasPriceBufferPercent is applied on top of the suggested gas price to
// increase the chance of timely inclusion when network gas prices are rising.
const GasPriceBufferPercent = 30

// ApplyGasPriceBuffer queries the current suggested gas price and sets it
// on auth with a percentage buffer for timely inclusion.
func ApplyGasPriceBuffer(auth *bind.TransactOpts, client EthBackend) error {
	ctx := auth.Context
	if ctx == nil {
		ctx = context.Background()
	}
	gasPrice, err := client.SuggestGasPrice(ctx)
	if err != nil {
		return err
	}
	buffered := new(big.Int).Mul(gasPrice, big.NewInt(100+GasPriceBufferPercent))
	buffered.Div(buffered, big.NewInt(100))
	auth.GasPrice = buffered
	return nil
}

// bufferGasLimit applies the gas estimate buffer to auth based on the
// estimated transaction's gas.
func bufferGasLimit(auth *bind.TransactOpts, estTx *types.Transaction) {
	auth.GasLimit = estTx.Gas() * (100 + GasEstimateBufferPercent) / 100
}

// StartWithdrawWithGasBuffer sends a StartWithdraw transaction with buffered
// gas limit and gas price.
func StartWithdrawWithGasBuffer(auth *bind.TransactOpts, contract *IWithdraw, client EthBackend, user, token common.Address, amount, nonce *big.Int) (*types.Transaction, error) {
	dryRun := *auth
	dryRun.NoSend = true
	estTx, err := contract.StartWithdraw(&dryRun, user, token, amount, nonce)
	if err != nil {
		return nil, err
	}
	bufferGasLimit(auth, estTx)
	if err := ApplyGasPriceBuffer(auth, client); err != nil {
		return nil, fmt.Errorf("gas price buffer: %w", err)
	}
	return contract.StartWithdraw(auth, user, token, amount, nonce)
}

// FinalizeWithdrawWithGasBuffer sends a FinalizeWithdraw transaction with
// buffered gas limit and gas price.
func FinalizeWithdrawWithGasBuffer(auth *bind.TransactOpts, contract *IWithdraw, client EthBackend, withdrawalID [32]byte) (*types.Transaction, error) {
	dryRun := *auth
	dryRun.NoSend = true
	estTx, err := contract.FinalizeWithdraw(&dryRun, withdrawalID)
	if err != nil {
		return nil, err
	}
	bufferGasLimit(auth, estTx)
	if err := ApplyGasPriceBuffer(auth, client); err != nil {
		return nil, fmt.Errorf("gas price buffer: %w", err)
	}
	return contract.FinalizeWithdraw(auth, withdrawalID)
}

// RejectWithdrawWithGasBuffer sends a RejectWithdraw transaction with
// buffered gas limit and gas price.
func RejectWithdrawWithGasBuffer(auth *bind.TransactOpts, contract *IWithdraw, client EthBackend, withdrawalID [32]byte) (*types.Transaction, error) {
	dryRun := *auth
	dryRun.NoSend = true
	estTx, err := contract.RejectWithdraw(&dryRun, withdrawalID)
	if err != nil {
		return nil, err
	}
	bufferGasLimit(auth, estTx)
	if err := ApplyGasPriceBuffer(auth, client); err != nil {
		return nil, fmt.Errorf("gas price buffer: %w", err)
	}
	return contract.RejectWithdraw(auth, withdrawalID)
}
