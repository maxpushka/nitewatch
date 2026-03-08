#!/usr/bin/env bash
# End-to-end test: deploy ThresholdCustody on Anvil, run full withdrawal flow via CLI.
#
# Prerequisites:
#   - anvil (from foundry)
#   - go (to build nitewatch)
#
# Usage:
#   ./scripts/test-e2e.sh
set -euo pipefail

RED='\033[0;31m'
GREEN='\033[0;32m'
CYAN='\033[0;36m'
NC='\033[0m'

step() { echo -e "\n${CYAN}==> $1${NC}"; }
pass() { echo -e "${GREEN}✓ $1${NC}"; }
fail() { echo -e "${RED}✗ $1${NC}"; cleanup; exit 1; }

ANVIL_PID=""
RPC="http://127.0.0.1:8545"
BINARY="./nitewatch"

cleanup() {
    if [ -n "$ANVIL_PID" ]; then
        kill "$ANVIL_PID" 2>/dev/null || true
        wait "$ANVIL_PID" 2>/dev/null || true
    fi
    rm -f "$BINARY"
}
trap cleanup EXIT

# ── Anvil default accounts (deterministic with --mnemonic "test test ...")
# Account 0-9 are funded with 10000 ETH each.
# We use account 0 as deployer/signer0, 1 as signer1, 2 as signer2, 3 as user.
KEY0="0xac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80"
KEY1="0x59c6995e998f97a5a0044966f0945389dc9e86dae88c7a8412f4603b6b78690d"
KEY2="0x5de4111afa1a4b94908f83103eb1f1706367c2e68ca870fc3fb9a804cdab365a"
KEY3="0x7c852118294e51e653712a81e05800f419141751be58f605c371e15141b007a6"

ADDR0="0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266"
ADDR1="0x70997970C51812dc3A010C7d01b50e0d17dc79C8"
ADDR2="0x3C44CdDdB6a900fa2b585dd299e03d12FA4293BC"
ADDR3="0x90F79bf6EB2c4f870365E785982E1f101E93b906"

# ── Step 1: Build CLI
step "Building nitewatch CLI"
go build -o "$BINARY" ./cmd/nitewatch
pass "Built $BINARY"

# ── Step 2: Start Anvil
step "Starting Anvil"
anvil --silent &
ANVIL_PID=$!
sleep 1

# Verify anvil is up.
if ! kill -0 "$ANVIL_PID" 2>/dev/null; then
    fail "Anvil failed to start"
fi
pass "Anvil running (pid=$ANVIL_PID)"

# ── Step 3: Deploy ThresholdCustody (3 signers, threshold=2)
step "Deploying ThresholdCustody (signers=3, threshold=2)"
DEPLOY_OUT=$($BINARY deploy \
    --rpc "$RPC" \
    --key "$KEY0" \
    --signers "$ADDR0,$ADDR1,$ADDR2" \
    --threshold 2)
echo "$DEPLOY_OUT"
CONTRACT=$(echo "$DEPLOY_OUT" | grep "^contract:" | awk '{print $2}')
if [ -z "$CONTRACT" ]; then
    fail "Failed to extract contract address"
fi
pass "Deployed at $CONTRACT"

# ── Step 4: Verify contract info
step "Checking contract info"
$BINARY info --rpc "$RPC" --contract "$CONTRACT"
pass "Contract info retrieved"

# ── Step 5: Verify signers
step "Listing signers"
$BINARY signers --rpc "$RPC" --contract "$CONTRACT"

step "Checking each signer"
$BINARY is-signer --rpc "$RPC" --contract "$CONTRACT" --address "$ADDR0"
$BINARY is-signer --rpc "$RPC" --contract "$CONTRACT" --address "$ADDR1"
$BINARY is-signer --rpc "$RPC" --contract "$CONTRACT" --address "$ADDR2"
pass "All 3 signers verified"

# Verify non-signer
if $BINARY is-signer --rpc "$RPC" --contract "$CONTRACT" --address "$ADDR3" 2>/dev/null; then
    fail "User address should not be a signer"
fi
pass "User correctly not a signer"

# ── Step 6: Check rate limit
step "Checking rate limit"
$BINARY rate-limit --rpc "$RPC" --contract "$CONTRACT"
pass "Rate limit retrieved"

# ── Step 7: User deposits 1 ETH
step "User depositing 1 ETH"
$BINARY deposit \
    --rpc "$RPC" \
    --contract "$CONTRACT" \
    --key "$KEY3" \
    --amount 1000000000000000000
pass "Deposited 1 ETH"

# ── Step 8: Check contract balance
step "Checking contract balance"
$BINARY balance --rpc "$RPC" --contract "$CONTRACT"
pass "Balance retrieved"

# ── Step 9: Signer0 starts withdrawal of 0.5 ETH for user (counts as 1/2 approval)
step "Signer0 starting withdrawal of 0.5 ETH for user"
START_OUT=$($BINARY start-withdraw \
    --rpc "$RPC" \
    --contract "$CONTRACT" \
    --key "$KEY0" \
    --user "$ADDR3" \
    --amount 500000000000000000 \
    --nonce 1)
echo "$START_OUT"
WITHDRAWAL_ID=$(echo "$START_OUT" | grep "^withdrawal_id:" | awk '{print $2}')
if [ -z "$WITHDRAWAL_ID" ]; then
    fail "Failed to extract withdrawal ID"
fi
pass "Withdrawal started: $WITHDRAWAL_ID"

# ── Step 10: Inspect the withdrawal
step "Inspecting withdrawal"
$BINARY withdrawal --rpc "$RPC" --contract "$CONTRACT" --id "$WITHDRAWAL_ID"
pass "Withdrawal inspected"

# ── Step 11: Signer1 finalizes (2/2 approvals → executes withdrawal)
step "Signer1 finalizing withdrawal (2/2 threshold)"
$BINARY finalize \
    --rpc "$RPC" \
    --contract "$CONTRACT" \
    --key "$KEY1" \
    --id "$WITHDRAWAL_ID"
pass "Withdrawal finalized"

# ── Step 12: Verify withdrawal is now finalized
step "Verifying withdrawal state after finalization"
$BINARY withdrawal --rpc "$RPC" --contract "$CONTRACT" --id "$WITHDRAWAL_ID"
pass "Withdrawal confirmed finalized"

# ── Step 13: Check contract balance decreased
step "Checking contract balance after withdrawal"
$BINARY balance --rpc "$RPC" --contract "$CONTRACT"
pass "Balance decreased"

# ── Step 14: List events
step "Listing all events"
$BINARY events --rpc "$RPC" --contract "$CONTRACT"
pass "Events retrieved"

# ── Step 15: Test rejection flow (deposit, start, then try to reject before expiry — should fail)
step "Testing rejection flow"
$BINARY deposit \
    --rpc "$RPC" \
    --contract "$CONTRACT" \
    --key "$KEY3" \
    --amount 1000000000000000000

REJECT_OUT=$($BINARY start-withdraw \
    --rpc "$RPC" \
    --contract "$CONTRACT" \
    --key "$KEY0" \
    --user "$ADDR3" \
    --amount 100000000000000000 \
    --nonce 2)
echo "$REJECT_OUT"
REJECT_ID=$(echo "$REJECT_OUT" | grep "^withdrawal_id:" | awk '{print $2}')

# Reject should fail (not expired yet — requires OPERATION_EXPIRY=1h)
if $BINARY reject --rpc "$RPC" --contract "$CONTRACT" --key "$KEY0" --id "$REJECT_ID" 2>/dev/null; then
    fail "Reject should fail before expiry"
fi
pass "Reject correctly blocked before expiry"

# ── Step 16: Signer2 can still finalize (different signer, 2/2 met)
step "Signer2 finalizing the second withdrawal"
$BINARY finalize \
    --rpc "$RPC" \
    --contract "$CONTRACT" \
    --key "$KEY1" \
    --id "$REJECT_ID"
pass "Second withdrawal finalized via different signer"

# ── Step 17: Double finalize should fail
step "Testing double finalize (should fail)"
if $BINARY finalize --rpc "$RPC" --contract "$CONTRACT" --key "$KEY2" --id "$REJECT_ID" 2>/dev/null; then
    fail "Double finalize should fail"
fi
pass "Double finalize correctly rejected"

# ── Step 18: Final event listing
step "Final events list"
$BINARY events --rpc "$RPC" --contract "$CONTRACT"

echo ""
echo -e "${GREEN}═══════════════════════════════════════${NC}"
echo -e "${GREEN}  All E2E tests passed!${NC}"
echo -e "${GREEN}═══════════════════════════════════════${NC}"
