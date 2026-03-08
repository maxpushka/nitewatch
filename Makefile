SOL_SOURCES := $(shell find contracts/evm/src -name '*.sol')
BINDINGS    := custody/iwithdraw.go custody/ideposit.go custody/simple_custody.go custody/quorum_custody.go custody/threshold_custody.go
BINARY      := nitewatch

.PHONY: all build generate test test-unit test-integration test-verbose lint clean help

all: generate build test ## Build and test everything

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*##' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*## "}; {printf "  %-20s %s\n", $$1, $$2}'

# --- Build ---

build: ## Build the nitewatch binary
	go build -o $(BINARY) ./cmd/nitewatch

# --- Code generation ---

generate: $(BINDINGS) ## Generate Go bindings from Solidity contracts

# Sentinel tracks forge build; only re-runs when .sol sources change.
contracts/evm/out/.build-sentinel: $(SOL_SOURCES)
	cd contracts/evm && forge build
	@touch $@

custody/iwithdraw.go: contracts/evm/out/.build-sentinel
	jq .abi contracts/evm/out/IWithdraw.sol/IWithdraw.json > custody/IWithdraw.abi
	abigen --abi custody/IWithdraw.abi --pkg custody --type IWithdraw --out $@

custody/ideposit.go: contracts/evm/out/.build-sentinel
	jq .abi contracts/evm/out/IDeposit.sol/IDeposit.json > custody/IDeposit.abi
	abigen --abi custody/IDeposit.abi --pkg custody --type IDeposit --out $@

custody/simple_custody.go: contracts/evm/out/.build-sentinel
	jq .abi contracts/evm/out/SimpleCustody.sol/SimpleCustody.json > custody/SimpleCustody.abi
	jq -r .bytecode.object contracts/evm/out/SimpleCustody.sol/SimpleCustody.json > custody/SimpleCustody.bin
	abigen --abi custody/SimpleCustody.abi --bin custody/SimpleCustody.bin --pkg custody --type SimpleCustody --out $@

custody/quorum_custody.go: contracts/evm/out/.build-sentinel
	jq .abi contracts/evm/out/QuorumCustody.sol/QuorumCustody.json > custody/QuorumCustody.abi
	jq -r .bytecode.object contracts/evm/out/QuorumCustody.sol/QuorumCustody.json > custody/QuorumCustody.bin
	abigen --abi custody/QuorumCustody.abi --bin custody/QuorumCustody.bin --pkg custody --type QuorumCustody --out $@

custody/threshold_custody.go: contracts/evm/out/.build-sentinel
	jq .abi contracts/evm/out/ThresholdCustody.sol/ThresholdCustody.json > custody/ThresholdCustody.abi
	jq -r .bytecode.object contracts/evm/out/ThresholdCustody.sol/ThresholdCustody.json > custody/ThresholdCustody.bin
	abigen --abi custody/ThresholdCustody.abi --bin custody/ThresholdCustody.bin --pkg custody --type ThresholdCustody --out $@

# --- Testing ---

test: ## Run all tests (unit + integration)
	go test ./... -count=1 -timeout 120s

test-unit: ## Run unit tests only (skip integration tests that use simulated backend)
	go test ./internal/... -count=1 -timeout 60s
	go test ./custody/ -count=1 -timeout 60s
	go test ./service/ -run 'Test[^GWIR]' -count=1 -timeout 60s

test-integration: ## Run integration tests (require simulated Ethereum backend)
	go test ./service/ -run 'Test(WithdrawalFinalized|WithdrawalRejected|GasBuffer|GasEstimate)' -v -count=1 -timeout 120s

test-verbose: ## Run all tests with verbose output
	go test ./... -v -count=1 -timeout 120s

# --- Lint ---

lint: ## Run go vet
	go vet ./...

# --- Clean ---

clean: ## Remove build artifacts
	rm -f $(BINARY)
	rm -f custody/*.abi custody/*.bin
