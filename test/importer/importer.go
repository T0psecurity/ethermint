package importer

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"time"

	"github.com/cosmos/ethermint/core"
	"github.com/cosmos/ethermint/state"

	ethcommon "github.com/ethereum/go-ethereum/common"
	ethmisc "github.com/ethereum/go-ethereum/consensus/misc"
	ethcore "github.com/ethereum/go-ethereum/core"
	ethstate "github.com/ethereum/go-ethereum/core/state"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	ethvm "github.com/ethereum/go-ethereum/core/vm"
	ethparams "github.com/ethereum/go-ethereum/params"
	ethrlp "github.com/ethereum/go-ethereum/rlp"
)

var (
	miner501    = ethcommon.HexToAddress("0x35e8e5dC5FBd97c5b421A80B596C030a2Be2A04D")
	genInvestor = ethcommon.HexToAddress("0x756F45E3FA69347A9A973A725E3C98bC4db0b5a0")
)

// Importer implements a structure to facilitate testing of importing mainnet
// Ethereum blocks and transactions using the Cosmos SDK components and the
// EVM.
type Importer struct {
	EthermintDB    *state.Database
	BlockchainFile string
	Datadir        string
	InterruptCh    <-chan bool
}

// Import performs an import given an Importer that has a Geth stateDB
// implementation and a blockchain exported file.
func (imp *Importer) Import() {
	// only create genesis if it is a brand new database
	if imp.EthermintDB.LatestVersion() == 0 {
		// start with empty root hash (i.e. empty state)
		gethStateDB, err := ethstate.New(ethcommon.Hash{}, imp.EthermintDB)
		if err != nil {
			panic(fmt.Sprintf("failed to instantiate geth state.StateDB: %v", err))
		}

		genBlock := ethcore.DefaultGenesisBlock()
		for addr, account := range genBlock.Alloc {
			gethStateDB.AddBalance(addr, account.Balance)
			gethStateDB.SetCode(addr, account.Code)
			gethStateDB.SetNonce(addr, account.Nonce)

			for key, value := range account.Storage {
				gethStateDB.SetState(addr, key, value)
			}
		}

		// get balance of one of the genesis account having 200 ETH
		b := gethStateDB.GetBalance(genInvestor)
		fmt.Printf("balance of %s: %s\n", genInvestor.String(), b)

		// commit the geth stateDB with 'false' to delete empty objects
		genRoot, err := gethStateDB.Commit(false)
		if err != nil {
			panic(err)
		}

		commitID := imp.EthermintDB.Commit()

		fmt.Printf("commitID after genesis: %v\n", commitID)
		fmt.Printf("genesis state root hash: %x\n", genRoot[:])
	}

	// file with blockchain data exported from geth by using "geth exportdb"
	// command.
	input, err := os.Open(imp.BlockchainFile)
	if err != nil {
		panic(err)
	}

	defer input.Close()

	// ethereum mainnet config
	chainConfig := ethparams.MainnetChainConfig

	// create RLP stream for exported blocks
	stream := ethrlp.NewStream(input, 0)

	var (
		block   ethtypes.Block
		root500 ethcommon.Hash // root hash after block 500
		root501 ethcommon.Hash // root hash after block 501
	)

	var prevRoot ethcommon.Hash
	binary.BigEndian.PutUint64(prevRoot[:8], uint64(imp.EthermintDB.LatestVersion()))

	imp.EthermintDB.Tracing = true
	chainContext := core.NewChainContext()
	vmConfig := ethvm.Config{}

	n := 0
	startTime := time.Now()
	interrupt := false

	var lastSkipped uint64
	for !interrupt {
		if err = stream.Decode(&block); err == io.EOF {
			err = nil
			break
		} else if err != nil {
			panic(fmt.Errorf("failed to decode at block %d: %s", block.NumberU64(), err))
		}

		// don't import blocks already imported
		if block.NumberU64() < uint64(imp.EthermintDB.LatestVersion()) {
			lastSkipped = block.NumberU64()
			continue
		}

		if lastSkipped > 0 {
			fmt.Printf("skipped blocks up to %d\n", lastSkipped)
			lastSkipped = 0
		}

		header := block.Header()
		chainContext.Coinbase = header.Coinbase
		chainContext.SetHeader(block.NumberU64(), header)

		gethStateDB, err := ethstate.New(prevRoot, imp.EthermintDB)
		if err != nil {
			panic(fmt.Errorf("failed to instantiate geth state.StateDB at block %d: %v", block.NumberU64(), err))
		}

		var (
			receipts ethtypes.Receipts
			usedGas  = new(uint64)
			allLogs  []*ethtypes.Log
			gp       = new(ethcore.GasPool).AddGas(block.GasLimit())
		)

		if chainConfig.DAOForkSupport && chainConfig.DAOForkBlock != nil && chainConfig.DAOForkBlock.Cmp(block.Number()) == 0 {
			ethmisc.ApplyDAOHardFork(gethStateDB)
		}

		for i, tx := range block.Transactions() {
			gethStateDB.Prepare(tx.Hash(), block.Hash(), i)

			txHash := tx.Hash()
			if bytes.Equal(txHash[:], ethcommon.FromHex("0xc438cfcc3b74a28741bda361032f1c6362c34aa0e1cedff693f31ec7d6a12717")) {
				vmConfig.Tracer = ethvm.NewStructLogger(&ethvm.LogConfig{})
				vmConfig.Debug = true
			}

			receipt, _, err := ethcore.ApplyTransaction(chainConfig, chainContext, nil, gp, gethStateDB, header, tx, usedGas, vmConfig)
			if vmConfig.Tracer != nil {
				w, err := os.Create(path.Join(imp.Datadir, "structlogs.txt"))
				if err != nil {
					panic(fmt.Sprintf("failed to create file for VM tracing: %v", err))
				}

				encoder := json.NewEncoder(w)
				logs := FormatLogs(vmConfig.Tracer.(*ethvm.StructLogger).StructLogs())

				if err := encoder.Encode(logs); err != nil {
					panic(err)
				}

				if err := w.Close(); err != nil {
					panic(err)
				}

				vmConfig.Debug = false
				vmConfig.Tracer = nil
			}

			if err != nil {
				panic(fmt.Errorf("at block %d, tx %x: %v", block.NumberU64(), tx.Hash(), err))
			}

			receipts = append(receipts, receipt)
			allLogs = append(allLogs, receipt.Logs...)
		}

		// apply mining rewards to the geth stateDB
		accumulateRewards(chainConfig, gethStateDB, header, block.Uncles())

		// commit block in geth
		prevRoot, err = gethStateDB.Commit(chainConfig.IsEIP158(block.Number()))
		if err != nil {
			panic(fmt.Errorf("at block %d: %v", block.NumberU64(), err))
		}

		// commit block in Ethermint
		imp.EthermintDB.Commit()

		switch block.NumberU64() {
		case 500:
			root500 = prevRoot
		case 501:
			root501 = prevRoot
		}

		n++
		if (n % 10000) == 0 {
			fmt.Printf("processed %d blocks, time so far: %v\n", n, time.Since(startTime))
		}

		// Check for interrupts
		select {
		case interrupt = <-imp.InterruptCh:
			fmt.Println("interrupted, please wait for cleanup...")
		default:
		}
	}

	fmt.Printf("processed %d blocks\n", n)

	imp.EthermintDB.Tracing = true

	// try to create a new geth stateDB from root of the block 500
	fmt.Printf("root at block 500: %x\n", root500[:])

	state500, err := ethstate.New(root500, imp.EthermintDB)
	if err != nil {
		panic(err)
	}

	miner501BalanceAt500 := state500.GetBalance(miner501)

	state501, err := ethstate.New(root501, imp.EthermintDB)
	if err != nil {
		panic(err)
	}

	miner501BalanceAt501 := state501.GetBalance(miner501)

	fmt.Printf("investor's balance after block 500: %d\n", state500.GetBalance(genInvestor))
	fmt.Printf("miner of block 501's balance after block 500: %d\n", miner501BalanceAt500)
	fmt.Printf("miner of block 501's balance after block 501: %d\n", miner501BalanceAt501)
}
