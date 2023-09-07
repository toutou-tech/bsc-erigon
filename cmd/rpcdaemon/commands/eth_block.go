package commands

import (
	"context"
	"encoding/hex"
	"fmt"
	"github.com/ethereum/go-ethereum/log"
	"golang.org/x/crypto/sha3"
	"math/big"
	"time"

	"github.com/ledgerwatch/erigon-lib/common"
	"github.com/ledgerwatch/erigon-lib/common/hexutility"
	"github.com/ledgerwatch/erigon-lib/kv"
	"github.com/ledgerwatch/erigon/cl/clparams"
	"github.com/ledgerwatch/erigon/common/hexutil"
	"github.com/ledgerwatch/erigon/common/math"
	"github.com/ledgerwatch/erigon/core"
	"github.com/ledgerwatch/erigon/core/rawdb"
	"github.com/ledgerwatch/erigon/core/state"
	"github.com/ledgerwatch/erigon/core/types"
	"github.com/ledgerwatch/erigon/core/vm"
	"github.com/ledgerwatch/erigon/crypto/cryptopool"
	"github.com/ledgerwatch/erigon/rpc"
	"github.com/ledgerwatch/erigon/turbo/adapter/ethapi"
	"github.com/ledgerwatch/erigon/turbo/rpchelper"
	"github.com/ledgerwatch/erigon/turbo/transactions"
)

func (api *APIImpl) CallBundle(ctx context.Context, txHashes []common.Hash, stateBlockNumberOrHash rpc.BlockNumberOrHash, timeoutMilliSecondsPtr *int64) (map[string]interface{}, error) {
	tx, err := api.db.BeginRo(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	chainConfig, err := api.chainConfig(tx)
	if err != nil {
		return nil, err
	}
	engine := api.engine()

	if len(txHashes) == 0 {
		return nil, nil
	}

	var txs types.Transactions

	for _, txHash := range txHashes {
		blockNum, ok, err := api.txnLookup(ctx, tx, txHash)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, nil
		}
		block, err := api.blockByNumberWithSenders(tx, blockNum)
		if err != nil {
			return nil, err
		}
		if block == nil {
			return nil, nil
		}
		var txn types.Transaction
		for _, transaction := range block.Transactions() {
			if transaction.Hash() == txHash {
				txn = transaction
				break
			}
		}
		if txn == nil {
			return nil, nil // not error, see https://github.com/ledgerwatch/turbo-geth/issues/1645
		}
		txs = append(txs, txn)
	}
	defer func(start time.Time) { log.Trace("Executing EVM call finished", "runtime", time.Since(start)) }(time.Now())

	stateBlockNumber, hash, latest, err := rpchelper.GetBlockNumber(stateBlockNumberOrHash, tx, api.filters)
	if err != nil {
		return nil, err
	}
	var stateReader state.StateReader
	if latest {
		cacheView, err := api.stateCache.View(ctx, tx)
		if err != nil {
			return nil, err
		}
		stateReader = state.NewCachedReader2(cacheView, tx)
	} else {
		stateReader, err = rpchelper.CreateHistoryStateReader(tx, stateBlockNumber+1, 0, api.historyV3(tx), chainConfig.ChainName)
		if err != nil {
			return nil, err
		}
	}
	ibs := state.New(stateReader)

	parent := rawdb.ReadHeader(tx, hash, stateBlockNumber)
	if parent == nil {
		return nil, fmt.Errorf("block %d(%x) not found", stateBlockNumber, hash)
	}

	blockNumber := stateBlockNumber + 1

	timestamp := parent.Time + clparams.MainnetBeaconConfig.SecondsPerSlot

	coinbase := parent.Coinbase
	header := &types.Header{
		ParentHash: parent.Hash(),
		Number:     big.NewInt(int64(blockNumber)),
		GasLimit:   parent.GasLimit,
		Time:       timestamp,
		Difficulty: parent.Difficulty,
		Coinbase:   coinbase,
	}

	signer := types.MakeSigner(chainConfig, blockNumber)
	rules := chainConfig.Rules(blockNumber, timestamp)
	firstMsg, err := txs[0].AsMessage(*signer, nil, rules)
	if err != nil {
		return nil, err
	}

	blockCtx := transactions.NewEVMBlockContext(engine, header, stateBlockNumberOrHash.RequireCanonical, tx, api._blockReader)
	txCtx := core.NewEVMTxContext(firstMsg)
	// Get a new instance of the EVM
	evm := vm.NewEVM(blockCtx, txCtx, ibs, chainConfig, vm.Config{Debug: false})

	timeoutMilliSeconds := int64(5000)
	if timeoutMilliSecondsPtr != nil {
		timeoutMilliSeconds = *timeoutMilliSecondsPtr
	}
	timeout := time.Millisecond * time.Duration(timeoutMilliSeconds)
	// Setup context so it may be cancelled the call has completed
	// or, in case of unmetered gas, setup a context with a timeout.
	var cancel context.CancelFunc
	if timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, timeout)
	} else {
		ctx, cancel = context.WithCancel(ctx)
	}
	// Make sure the context is cancelled when the call has completed
	// this makes sure resources are cleaned up.
	defer cancel()

	// Wait for the context to be done and cancel the evm. Even if the
	// EVM has finished, cancelling may be done (repeatedly)
	go func() {
		<-ctx.Done()
		evm.Cancel()
	}()

	// Setup the gas pool (also for unmetered requests)
	// and apply the message.
	gp := new(core.GasPool).AddGas(math.MaxUint64)

	results := []map[string]interface{}{}

	bundleHash := cryptopool.NewLegacyKeccak256()
	defer cryptopool.ReturnToPoolKeccak256(bundleHash)

	for _, txn := range txs {
		msg, err := txn.AsMessage(*signer, nil, rules)
		msg.SetCheckNonce(false)
		if err != nil {
			return nil, err
		}
		// Execute the transaction message
		result, err := core.ApplyMessage(evm, msg, gp, true /* refunds */, false /* gasBailout */)
		if err != nil {
			return nil, err
		}
		// If the timer caused an abort, return an appropriate error message
		if evm.Cancelled() {
			return nil, fmt.Errorf("execution aborted (timeout = %v)", timeout)
		}

		txHash := txn.Hash().String()
		jsonResult := map[string]interface{}{
			"txHash":  txHash,
			"gasUsed": result.UsedGas,
		}
		bundleHash.Write(txn.Hash().Bytes())
		if result.Err != nil {
			jsonResult["error"] = result.Err.Error()
		} else {
			jsonResult["value"] = common.BytesToHash(result.Return())
		}

		results = append(results, jsonResult)
	}

	ret := map[string]interface{}{}
	ret["results"] = results
	ret["bundleHash"] = hexutility.Encode(bundleHash.Sum(nil))
	return ret, nil
}

// CallBundleArgs represents the arguments for a call.
type CallBundleArgs struct {
	Txs                    []hexutil.Bytes        `json:"txs"`
	BlockNumber            rpc.BlockNumber        `json:"blockNumber"`
	StateBlockNumberOrHash rpc.BlockNumberOrHash  `json:"stateBlockNumber"`
	Coinbase               *string                `json:"coinbase"`
	Timestamp              *uint64                `json:"timestamp"`
	Timeout                *int64                 `json:"timeout"`
	GasLimit               *uint64                `json:"gasLimit"`
	Difficulty             *big.Int               `json:"difficulty"`
	SimulationLogs         bool                   `json:"simulationLogs"`
	StateOverrides         *ethapi.StateOverrides `json:"stateOverrides"`
}

// CallBundle will simulate a bundle of transactions at the top of a given block
// number with the state of another (or the same) block. This can be used to
// simulate future blocks with the current state, or it can be used to simulate
// a past block.
// The sender is responsible for signing the transactions and using the correct
// nonce and ensuring validity
func (api *APIImpl) CallBundleWithArgs(ctx context.Context, args CallBundleArgs) (map[string]interface{}, error) {
	tx, err := api.db.BeginRo(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	chainConfig, err := api.chainConfig(tx)
	if err != nil {
		return nil, err
	}
	engine := api.engine()

	if len(args.Txs) == 0 {
		return nil, fmt.Errorf("bundle missing txs")
	}
	if args.BlockNumber == 0 {
		return nil, fmt.Errorf("bundle missing blockNumber")
	}

	var txs types.Transactions

	for _, encodedTx := range args.Txs {
		tx, err := types.UnmarshalTransactionFromBinary(encodedTx)
		if err != nil {
			return nil, err
		}
		txs = append(txs, tx)
	}
	defer func(start time.Time) { log.Debug("Executing EVM call finished", "runtime", time.Since(start)) }(time.Now())

	timeoutMilliSeconds := int64(5000)
	if args.Timeout != nil {
		timeoutMilliSeconds = *args.Timeout
	}
	timeout := time.Millisecond * time.Duration(timeoutMilliSeconds)

	stateBlockNumber, hash, latest, err := rpchelper.GetBlockNumber(args.StateBlockNumberOrHash, tx, api.filters)
	if err != nil {
		return nil, err
	}
	var stateReader state.StateReader
	if latest {
		cacheView, err := api.stateCache.View(ctx, tx)
		if err != nil {
			return nil, err
		}
		stateReader = state.NewCachedReader2(cacheView, tx)
	} else {
		stateReader, err = rpchelper.CreateHistoryStateReader(tx, stateBlockNumber+1, 0, api.historyV3(tx), chainConfig.ChainName)
		if err != nil {
			return nil, err
		}
	}
	ibs := state.New(stateReader)

	parent := rawdb.ReadHeader(tx, hash, stateBlockNumber)
	if parent == nil {
		return nil, fmt.Errorf("block %d(%x) not found", stateBlockNumber, hash)
	}

	if args.StateOverrides != nil {
		if err := args.StateOverrides.Override(ibs); err != nil {
			return nil, err
		}
	}

	blockNumber := args.BlockNumber

	timestamp := parent.Time + 1
	if args.Timestamp != nil {
		timestamp = *args.Timestamp
	}
	coinbase := parent.Coinbase
	if args.Coinbase != nil {
		coinbase = common.HexToAddress(*args.Coinbase)
	}
	difficulty := parent.Difficulty
	if args.Difficulty != nil {
		difficulty = args.Difficulty
	}
	gasLimit := parent.GasLimit
	if args.GasLimit != nil {
		gasLimit = *args.GasLimit
	}
	header := &types.Header{
		ParentHash: parent.Hash(),
		Number:     big.NewInt(int64(blockNumber)),
		GasLimit:   gasLimit,
		Time:       timestamp,
		Difficulty: difficulty,
		Coinbase:   coinbase,
	}

	signer := types.MakeSigner(chainConfig, uint64(blockNumber))
	// Setup context so it may be cancelled the call has completed
	// or, in case of unmetered gas, setup a context with a timeout.
	var cancel context.CancelFunc
	if timeout > 0 {
		_, cancel = context.WithTimeout(ctx, timeout)
	} else {
		_, cancel = context.WithCancel(ctx)
	}
	// Make sure the context is cancelled when the call has completed
	// this makes sure resources are cleaned up.
	defer cancel()

	vmConfig := vm.Config{}

	// Setup the gas pool (also for unmetered requests)
	// and apply the message.
	gp := new(core.GasPool).AddGas(math.MaxUint64)

	results := []map[string]interface{}{}

	bundleHash := sha3.NewLegacyKeccak256()
	var totalGasUsed uint64
	gasFees := new(big.Int)

	block, err := api.blockByNumber(ctx, blockNumber, tx)
	if err != nil {
		return nil, err
	}

	getHeader := func(hash common.Hash, number uint64) *types.Header {
		h, e := api._blockReader.Header(ctx, tx, hash, number)
		if e != nil {
			log.Error("getHeader error", "number", number, "hash", hash, "err", e)
		}
		return h
	}

	noopWriter := state.NewNoopWriter()

	for i, tx := range txs {
		ibs.Prepare(tx.Hash(), block.Hash(), i)

		receipt, result, err := core.ApplyTransactionWithResult(chainConfig, core.GetHashFn(header, getHeader), engine, nil, gp, ibs, noopWriter, header, tx, &header.GasUsed, vmConfig, parent.ExcessDataGas)
		if err != nil {
			return nil, fmt.Errorf("could not apply tx %d [%x] failed: %w", i, tx.Hash(), err)
		}

		txHash := tx.Hash().String()
		from, err := signer.Sender(tx)
		if err != nil {
			return nil, fmt.Errorf("err: %w; txhash %s", err, tx.Hash())
		}
		to := "0x"
		if tx.GetTo() != nil {
			to = tx.GetTo().String()
		}
		jsonResult := map[string]interface{}{
			"txHash":      txHash,
			"gasUsed":     receipt.GasUsed,
			"fromAddress": from.String(),
			"toAddress":   to,
		}
		totalGasUsed += receipt.GasUsed
		gasFeesTx := new(big.Int).Mul(big.NewInt(int64(receipt.GasUsed)), tx.GetPrice().ToBig())
		gasFees.Add(gasFees, gasFeesTx)
		bundleHash.Write(tx.Hash().Bytes())
		if result.Err != nil {
			jsonResult["error"] = result.Err.Error()
			revert := result.Revert()
			if len(revert) > 0 {
				jsonResult["revert"] = string(revert)
			}
		} else {
			dst := make([]byte, hex.EncodedLen(len(result.Return())))
			hex.Encode(dst, result.Return())
			jsonResult["value"] = "0x" + string(dst)
		}
		// if simulation logs are requested append it to logs
		if args.SimulationLogs {
			jsonResult["logs"] = receipt.Logs
		}
		jsonResult["gasFees"] = gasFeesTx.String()
		jsonResult["gasPrice"] = tx.GetPrice().String()
		jsonResult["gasUsed"] = receipt.GasUsed
		results = append(results, jsonResult)
	}

	ret := map[string]interface{}{}
	ret["results"] = results
	ret["gasFees"] = gasFees.String()
	ret["bundleGasPrice"] = new(big.Int).Div(gasFees, big.NewInt(int64(totalGasUsed))).String()
	ret["totalGasUsed"] = totalGasUsed
	ret["stateBlockNumber"] = parent.Number.Int64()

	ret["bundleHash"] = hexutility.Encode(bundleHash.Sum(nil))
	return ret, nil
}

// GetBlockByNumber implements eth_getBlockByNumber. Returns information about a block given the block's number.
func (api *APIImpl) GetBlockByNumber(ctx context.Context, number rpc.BlockNumber, fullTx bool) (map[string]interface{}, error) {
	tx, err := api.db.BeginRo(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	b, err := api.blockByNumber(ctx, number, tx)
	if err != nil {
		return nil, err
	}
	if b == nil {
		return nil, nil
	}
	additionalFields := make(map[string]interface{})
	td, err := rawdb.ReadTd(tx, b.Hash(), b.NumberU64())
	if err != nil {
		return nil, err
	}
	if td != nil {
		additionalFields["totalDifficulty"] = (*hexutil.Big)(td)
	}

	chainConfig, err := api.chainConfig(tx)
	if err != nil {
		return nil, err
	}
	var borTx types.Transaction
	var borTxHash common.Hash
	if chainConfig.Bor != nil {
		borTx, _, _, _ = rawdb.ReadBorTransactionForBlock(tx, b)
		if borTx != nil {
			borTxHash = types.ComputeBorTxHash(b.NumberU64(), b.Hash())
		}
	}

	response, err := ethapi.RPCMarshalBlockEx(b, true, fullTx, borTx, borTxHash, additionalFields)
	if err == nil && number == rpc.PendingBlockNumber {
		// Pending blocks need to nil out a few fields
		for _, field := range []string{"hash", "nonce", "miner"} {
			response[field] = nil
		}
	}
	return response, err
}

// GetBlockByHash implements eth_getBlockByHash. Returns information about a block given the block's hash.
func (api *APIImpl) GetBlockByHash(ctx context.Context, numberOrHash rpc.BlockNumberOrHash, fullTx bool) (map[string]interface{}, error) {
	if numberOrHash.BlockHash == nil {
		// some web3.js based apps (like ethstats client) for some reason call
		// eth_getBlockByHash with a block number as a parameter
		// so no matter how weird that is, we would love to support that.
		if numberOrHash.BlockNumber == nil {
			return nil, nil // not error, see https://github.com/ledgerwatch/erigon/issues/1645
		}
		return api.GetBlockByNumber(ctx, *numberOrHash.BlockNumber, fullTx)
	}

	hash := *numberOrHash.BlockHash
	tx, err := api.db.BeginRo(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	additionalFields := make(map[string]interface{})

	block, err := api.blockByHashWithSenders(tx, hash)
	if err != nil {
		return nil, err
	}
	if block == nil {
		return nil, nil // not error, see https://github.com/ledgerwatch/erigon/issues/1645
	}
	number := block.NumberU64()

	td, err := rawdb.ReadTd(tx, hash, number)
	if err != nil {
		return nil, err
	}
	additionalFields["totalDifficulty"] = (*hexutil.Big)(td)

	chainConfig, err := api.chainConfig(tx)
	if err != nil {
		return nil, err
	}
	var borTx types.Transaction
	var borTxHash common.Hash
	if chainConfig.Bor != nil {
		borTx, _, _, _ = rawdb.ReadBorTransactionForBlock(tx, block)
		if borTx != nil {
			borTxHash = types.ComputeBorTxHash(block.NumberU64(), block.Hash())
		}
	}

	response, err := ethapi.RPCMarshalBlockEx(block, true, fullTx, borTx, borTxHash, additionalFields)

	if chainConfig.Bor != nil {
		response["miner"], _ = ecrecover(block.Header(), chainConfig.Bor)
	}

	if err == nil && int64(number) == rpc.PendingBlockNumber.Int64() {
		// Pending blocks need to nil out a few fields
		for _, field := range []string{"hash", "nonce", "miner"} {
			response[field] = nil
		}
	}
	return response, err
}

// GetBlockTransactionCountByNumber implements eth_getBlockTransactionCountByNumber. Returns the number of transactions in a block given the block's block number.
func (api *APIImpl) GetBlockTransactionCountByNumber(ctx context.Context, blockNr rpc.BlockNumber) (*hexutil.Uint, error) {
	tx, err := api.db.BeginRo(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	if blockNr == rpc.PendingBlockNumber {
		b, err := api.blockByRPCNumber(blockNr, tx)
		if err != nil {
			return nil, err
		}
		if b == nil {
			return nil, nil
		}
		n := hexutil.Uint(len(b.Transactions()))
		return &n, nil
	}

	blockNum, blockHash, _, err := rpchelper.GetBlockNumber(rpc.BlockNumberOrHashWithNumber(blockNr), tx, api.filters)
	if err != nil {
		return nil, err
	}
	latestBlockNumber, err := rpchelper.GetLatestBlockNumber(tx)
	if err != nil {
		return nil, err
	}
	if blockNum > latestBlockNumber {
		// (Compatibility) Every other node just returns `null` for when the block does not exist.
		return nil, nil
	}

	_, txAmount, err := api._blockReader.Body(ctx, tx, blockHash, blockNum)
	if err != nil {
		return nil, err
	}
	numOfTx := hexutil.Uint(txAmount)

	return &numOfTx, nil
}

// GetBlockTransactionCountByHash implements eth_getBlockTransactionCountByHash. Returns the number of transactions in a block given the block's block hash.
func (api *APIImpl) GetBlockTransactionCountByHash(ctx context.Context, blockHash common.Hash) (*hexutil.Uint, error) {
	tx, err := api.db.BeginRo(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	blockNum, _, _, err := rpchelper.GetBlockNumber(rpc.BlockNumberOrHash{BlockHash: &blockHash}, tx, nil)
	if err != nil {
		// (Compatibility) Every other node just return `null` for when the block does not exist.
		log.Debug("eth_getBlockTransactionCountByHash GetBlockNumber failed", "err", err)
		return nil, nil
	}

	_, txAmount, err := api._blockReader.Body(ctx, tx, blockHash, blockNum)
	if err != nil {
		return nil, err
	}
	numOfTx := hexutil.Uint(txAmount)

	return &numOfTx, nil
}

func (api *APIImpl) blockByNumber(ctx context.Context, number rpc.BlockNumber, tx kv.Tx) (*types.Block, error) {
	if number != rpc.PendingBlockNumber {
		return api.blockByRPCNumber(number, tx)
	}

	if block := api.pendingBlock(); block != nil {
		return block, nil
	}

	block, err := api.ethBackend.PendingBlock(ctx)
	if err != nil {
		return nil, err
	}
	if block != nil {
		return block, nil
	}

	return api.blockByRPCNumber(number, tx)
}
