/*
Package txprocessor is the module that takes the transactions from the input and
processes them, updating the Balances and Nonces of the Accounts in the StateDB.

It's a package used by 3 other different packages, and its behaviour will differ
depending on the Type of the StateDB of the TxProcessor:

- TypeSynchronizer:
  - The StateDB contains the full State MerkleTree, where the leafs are
    the accounts
  - Updates the StateDB and as output returns: ExitInfos, CreatedAccounts,
    CoordinatorIdxsMap, CollectedFees, UpdatedAccounts
  - Internally computes the ExitTree

- TypeTxSelector:
  - The StateDB contains only the Accounts, which are the equivalent to
    only the leafs of the State MerkleTree
  - Updates the Accounts from the StateDB

- TypeBatchBuilder:
  - The StateDB contains the full State MerkleTree, where the leafs are
    the accounts
  - Updates the StateDB. As output returns: ZKInputs, CoordinatorIdxsMap
  - Internally computes the ZKInputs

Packages dependency overview:

	Outputs: + ExitInfos              +                  +                       +
		 | CreatedAccounts        |                  |                       |
		 | CoordinatorIdxsMap     |                  |    ZKInputs           |
		 | CollectedFees          |                  |    CoordinatorIdxsMap |
		 | UpdatedAccounts        |                  |                       |
		 +------------------------+----------------+ +-----------------------+

		    +------------+           +----------+             +------------+
		    |Synchronizer|           |TxSelector|             |BatchBuilder|
		    +-----+------+           +-----+----+             +-----+------+
			  |                        |                        |
			  v                        v                        v
		     TxProcessor              TxProcessor              TxProcessor
			  +                        +                        +
			  |                        |                        |
		     +----+----+                   v                   +----+----+
		     |         |                StateDB                |         |
		     v         v                   +                   v         v
		  StateDB  ExitTree                |                StateDB  ExitTree
		     +                        +----+----+              +
		     |                        |         |              |
		+----+----+                   v         v         +----+----+
		|         |                 KVDB  AccountsDB      |         |
		v         v                                       v         v
	      KVDB   MerkleTree                                 KVDB   MerkleTree

The structure of the TxProcessor can be understand as:
  - StateDB: where the Rollup state is stored. It contains the Accounts &
    MerkleTree.
  - Config: parameters of the configuration of the circuit
  - ZKInputs: computed inputs for the circuit, depends on the Config parameters
  - ExitTree: only in the TypeSynchronizer & TypeBatchBuilder, contains
    the MerkleTree with the processed Exits of the Batch

The main exposed method of the TxProcessor is `ProcessTxs`, which as general
lines does:
  - if type==(Synchronizer || BatchBuilder), creates an ephemeral ExitTree
  - processes:
  - L1UserTxs --> for each tx calls ProcessL1Tx()
  - L1CoordinatorTxs --> for each tx calls ProcessL1Tx()
  - L2Txs --> for each tx calls ProcessL2Tx()
  - internally, it computes the Fees
  - each transaction processment includes:
  - updating the Account Balances (for sender & receiver, and in
    case that there is fee, updates the fee receiver account)
  - which includes updating the State MerkleTree (except
    for the type==TxSelector, which only updates the
    Accounts (leafs))
  - in case of Synchronizer & BatchBuilder, updates the ExitTree
    for the txs of type Exit (L1 & L2)
  - in case of BatchBuilder, computes the ZKInputs while processing the txs
  - if type==Synchronizer, once all the txs are processed, for each Exit
    it generates the ExitInfo data
*/
package txprocessor

import (
	"bytes"
	"errors"
	"fmt"
	"io/ioutil"
	"math/big"
	"os"
	"tokamak-sybil-resistance/common"
	"tokamak-sybil-resistance/database/statedb"
	"tokamak-sybil-resistance/log"

	"github.com/iden3/go-iden3-crypto/babyjub"
	"github.com/iden3/go-merkletree"
	"github.com/iden3/go-merkletree/db"
	"github.com/iden3/go-merkletree/db/pebble"
)

// TxProcessor represents the TxProcessor object
type TxProcessor struct {
	state *statedb.StateDB
	zki   *common.ZKInputs
	// txIndex is the current transaction index in the ZKInputs generation (zki)
	txIndex int
	// AccumulatedFees contains the accumulated fees for each token (Coord
	// Idx) in the processed batch
	AccumulatedFees map[common.AccountIdx]*big.Int
	// updatedAccounts stores the last version of the account when it has
	// been created/updated by any of the processed transactions.
	updatedAccounts map[common.AccountIdx]*common.Account
	config          Config
}

// Config contains the TxProcessor configuration parameters
type Config struct {
	NLevels uint32
	// MaxFeeTx is the maximum number of coordinator accounts that can receive fees
	MaxFeeTx uint32
	MaxTx    uint32
	MaxL1Tx  uint32
	// ChainID of the blockchain
	ChainID uint16
}

type processedExit struct {
	exit    bool
	newExit bool
	idx     common.AccountIdx
	acc     common.Account
}

// ProcessTxOutput contains the output of the ProcessTxs method
type ProcessTxOutput struct {
	ZKInputs           *common.ZKInputs
	ExitInfos          []common.ExitInfo
	CreatedAccounts    []common.Account
	CoordinatorIdxsMap map[common.TokenID]common.AccountIdx
	CollectedFees      map[common.TokenID]*big.Int
	// UpdatedAccounts returns the current state of each account
	// created/updated by any of the processed transactions.
	UpdatedAccounts map[common.AccountIdx]*common.Account
}

func newErrorNotEnoughBalance(tx common.Tx) error {
	var msg error
	if tx.IsL1 {
		msg = fmt.Errorf("invalid transaction, not enough balance on sender account. "+
			"TxID: %s, TxType: %s, FromIdx: %d, ToIdx: %d, Amount: %d",
			tx.TxID, tx.Type, tx.FromIdx, tx.ToIdx, tx.Amount)
	} else {
		msg = fmt.Errorf("invalid transaction, not enough balance on sender account. "+
			"TxID: %s, TxType: %s, FromIdx: %d, ToIdx: %d, Amount: %d, Fee: %d",
			tx.TxID, tx.Type, tx.FromIdx, tx.ToIdx, tx.Amount, tx.Fee)
	}
	return common.Wrap(msg)
}

// NewTxProcessor returns a new TxProcessor with the given *StateDB & Config
func NewTxProcessor(state *statedb.StateDB, config Config) *TxProcessor {
	return &TxProcessor{
		state:   state,
		zki:     nil,
		txIndex: 0,
		config:  config,
	}
}

// ProcessTxs process the given L1Txs & L2Txs applying the needed updates to
// the StateDB depending on the transaction Type.  If StateDB
// type==TypeBatchBuilder, returns the common.ZKInputs to generate the
// SnarkProof later used by the BatchBuilder.  If StateDB
// type==TypeSynchronizer, assumes that the call is done from the Synchronizer,
// returns common.ExitTreeLeaf that is later used by the Synchronizer to update
// the HistoryDB, and adds Nonce & TokenID to the L2Txs.
// And if TypeSynchronizer returns an array of common.Account with all the
// created accounts.
func (txProcessor *TxProcessor) ProcessTxs(coordIdxs []common.AccountIdx, l1usertxs, l1coordinatortxs []common.L1Tx,
	l2txs []common.PoolL2Tx) (ptOut *ProcessTxOutput, err error) {
	defer func() {
		if err == nil {
			err = txProcessor.state.MakeCheckpoint()
		}
	}()

	var exitTree *merkletree.MerkleTree
	var createdAccounts []common.Account

	if txProcessor.zki != nil {
		return nil, common.Wrap(
			errors.New("expected StateDB.zki==nil, something went wrong and it's not empty"))
	}
	// defer txProcessor.resetZKInputs()

	if len(coordIdxs) > int(txProcessor.config.MaxFeeTx) {
		return nil, common.Wrap(
			fmt.Errorf("CoordIdxs (%d) length must be smaller than MaxFeeTx (%d)",
				len(coordIdxs), txProcessor.config.MaxFeeTx))
	}

	nTx := len(l1usertxs) + len(l1coordinatortxs) + len(l2txs)

	if nTx > int(txProcessor.config.MaxTx) {
		return nil, common.Wrap(
			fmt.Errorf("L1UserTx + L1CoordinatorTx + L2Tx (%d) can not be bigger than MaxTx (%d)",
				nTx, txProcessor.config.MaxTx))
	}
	if len(l1usertxs)+len(l1coordinatortxs) > int(txProcessor.config.MaxL1Tx) {
		return nil,
			common.Wrap(fmt.Errorf("L1UserTx + L1CoordinatorTx (%d) can not be bigger than MaxL1Tx (%d)",
				len(l1usertxs)+len(l1coordinatortxs), txProcessor.config.MaxTx))
	}

	if txProcessor.state.Type() == statedb.TypeSynchronizer {
		txProcessor.updatedAccounts = make(map[common.AccountIdx]*common.Account)
	}

	exits := make([]processedExit, nTx)

	// if txProcessor.state.Type() == statedb.TypeBatchBuilder {
	// 	txProcessor.zki = common.NewZKInputs(txProcessor.config.ChainID, txProcessor.config.MaxTx, txProcessor.config.MaxL1Tx,
	// 		txProcessor.config.MaxFeeTx, txProcessor.config.NLevels, (txProcessor.state.CurrentBatch() + 1).BigInt())
	// 	txProcessor.zki.OldLastIdx = txProcessor.state.CurrentIdx().BigInt()
	// 	txProcessor.zki.OldStateRoot = txProcessor.state.MT.Root().BigInt()
	// 	txProcessor.zki.Metadata.NewLastIdxRaw = txProcessor.state.CurrentIdx()
	// }

	// TBD if ExitTree is only in memory or stored in disk, for the moment
	// is only needed in memory
	if txProcessor.state.Type() == statedb.TypeSynchronizer || txProcessor.state.Type() == statedb.TypeBatchBuilder {
		tmpDir, err := ioutil.TempDir("", "hermez-statedb-exittree")
		if err != nil {
			return nil, common.Wrap(err)
		}
		defer func() {
			if err := os.RemoveAll(tmpDir); err != nil {
				log.Errorw("Deleting statedb temp exit tree", "err", err)
			}
		}()
		sto, err := pebble.NewPebbleStorage(tmpDir, false)
		if err != nil {
			return nil, common.Wrap(err)
		}
		defer sto.Close()
		exitTree, err = merkletree.NewMerkleTree(sto, txProcessor.state.AccountTree.MaxLevels())
		if err != nil {
			return nil, common.Wrap(err)
		}
	}

	// Process L1UserTxs
	for i := 0; i < len(l1usertxs); i++ {
		// assumption: l1usertx are sorted by L1Tx.Position
		exitIdx, exitAccount, newExit, createdAccount, err := txProcessor.ProcessL1Tx(exitTree,
			&l1usertxs[i])
		if err != nil {
			return nil, common.Wrap(err)
		}
		if txProcessor.state.Type() == statedb.TypeSynchronizer {
			if createdAccount != nil {
				createdAccounts = append(createdAccounts, *createdAccount)
				l1usertxs[i].EffectiveFromIdx = createdAccount.Idx
			} else {
				l1usertxs[i].EffectiveFromIdx = l1usertxs[i].FromIdx
			}
		}
		// if txProcessor.zki != nil {
		// 	l1TxData, err := l1usertxs[i].BytesGeneric()
		// 	if err != nil {
		// 		return nil, common.Wrap(err)
		// 	}
		// 	txProcessor.zki.Metadata.L1TxsData = append(txProcessor.zki.Metadata.L1TxsData, l1TxData)

		// 	l1TxDataAvailability, err :=
		// 		l1usertxs[i].BytesDataAvailability(txProcessor.zki.Metadata.NLevels)
		// 	if err != nil {
		// 		return nil, common.Wrap(err)
		// 	}
		// 	txProcessor.zki.Metadata.L1TxsDataAvailability =
		// 		append(txProcessor.zki.Metadata.L1TxsDataAvailability, l1TxDataAvailability)

		// 	txProcessor.zki.ISOutIdx[txProcessor.txIndex] = txProcessor.state.CurrentIdx().BigInt()
		// 	txProcessor.zki.ISStateRoot[txProcessor.txIndex] = txProcessor.state.MT.Root().BigInt()
		// 	if exitIdx == nil {
		// 		txProcessor.zki.ISExitRoot[txProcessor.txIndex] = exitTree.Root().BigInt()
		// 	}
		// }
		if txProcessor.state.Type() == statedb.TypeSynchronizer || txProcessor.state.Type() == statedb.TypeBatchBuilder {
			if exitIdx != nil && exitTree != nil && exitAccount != nil {
				exits[txProcessor.txIndex] = processedExit{
					exit:    true,
					newExit: newExit,
					idx:     *exitIdx,
					acc:     *exitAccount,
				}
			}
			txProcessor.txIndex++
		}
	}

	// // Process L1CoordinatorTxs
	// for i := 0; i < len(l1coordinatortxs); i++ {
	// 	exitIdx, _, _, createdAccount, err := txProcessor.ProcessL1Tx(exitTree, &l1coordinatortxs[i])
	// 	if err != nil {
	// 		return nil, common.Wrap(err)
	// 	}
	// 	if exitIdx != nil {
	// 		log.Error("Unexpected Exit in L1CoordinatorTx")
	// 	}
	// 	if txProcessor.state.Type() == statedb.TypeSynchronizer {
	// 		if createdAccount != nil {
	// 			createdAccounts = append(createdAccounts, *createdAccount)
	// 			l1coordinatortxs[i].EffectiveFromIdx = createdAccount.Idx
	// 		} else {
	// 			l1coordinatortxs[i].EffectiveFromIdx = l1coordinatortxs[i].FromIdx
	// 		}
	// 	}
	// if txProcessor.zki != nil {
	// l1TxData, err := l1coordinatortxs[i].BytesGeneric()
	// 	if err != nil {
	// 		return nil, common.Wrap(err)
	// 	}
	// 	txProcessor.zki.Metadata.L1TxsData = append(txProcessor.zki.Metadata.L1TxsData, l1TxData)
	// 	l1TxDataAvailability, err :=
	// 		l1coordinatortxs[i].BytesDataAvailability(txProcessor.zki.Metadata.NLevels)
	// 	if err != nil {
	// 		return nil, common.Wrap(err)
	// 	}
	// 	txProcessor.zki.Metadata.L1TxsDataAvailability =
	// 		append(txProcessor.zki.Metadata.L1TxsDataAvailability, l1TxDataAvailability)

	// 	txProcessor.zki.ISOutIdx[txProcessor.txIndex] = txProcessor.state.CurrentIdx().BigInt()
	// 	txProcessor.zki.ISStateRoot[txProcessor.txIndex] = txProcessor.state.MT.Root().BigInt()
	// 	txProcessor.zki.ISExitRoot[txProcessor.txIndex] = exitTree.Root().BigInt()
	// 	txProcessor.txIndex++
	// }
	// }

	// // remove repeated CoordIdxs that are for the same TokenID (use the
	// // first occurrence)
	// usedCoordTokenIDs := make(map[common.TokenID]bool)
	// var filteredCoordIdxs []common.Idx
	// for i := 0; i < len(coordIdxs); i++ {
	// 	accCoord, err := txProcessor.state.GetAccount(coordIdxs[i])
	// 	if err != nil {
	// 		return nil, common.Wrap(err)
	// 	}
	// 	if !usedCoordTokenIDs[accCoord.TokenID] {
	// 		usedCoordTokenIDs[accCoord.TokenID] = true
	// 		filteredCoordIdxs = append(filteredCoordIdxs, coordIdxs[i])
	// 	}
	// }
	// coordIdxs = filteredCoordIdxs

	// txProcessor.AccumulatedFees = make(map[common.Idx]*big.Int)
	// for _, idx := range coordIdxs {
	// 	txProcessor.AccumulatedFees[idx] = big.NewInt(0)
	// }

	// // once L1UserTxs & L1CoordinatorTxs are processed, get TokenIDs of
	// // coordIdxs. In this way, if a coordIdx uses an Idx that is being
	// // created in the current batch, at this point the Idx will be created
	// coordIdxsMap, err := txProcessor.state.GetTokenIDsFromIdxs(coordIdxs)
	// if err != nil {
	// 	return nil, common.Wrap(err)
	// }
	// // collectedFees will contain the amount of fee collected for each
	// // TokenID
	// var collectedFees map[common.TokenID]*big.Int
	// if txProcessor.state.Type() == statedb.TypeSynchronizer || txProcessor.state.Type() == statedb.TypeBatchBuilder {
	// 	collectedFees = make(map[common.TokenID]*big.Int)
	// 	for tokenID := range coordIdxsMap {
	// 		collectedFees[tokenID] = big.NewInt(0)
	// 	}
	// }

	// if txProcessor.zki != nil {
	// 	// get the feePlanTokens
	// 	feePlanTokens, err := txProcessor.getFeePlanTokens(coordIdxs)
	// 	if err != nil {
	// 		log.Error(err)
	// 		return nil, common.Wrap(err)
	// 	}
	// 	copy(txProcessor.zki.FeePlanTokens, feePlanTokens)
	// }

	// Process L2Txs
	for i := 0; i < len(l2txs); i++ {
		exitIdx, exitAccount, newExit, err := txProcessor.ProcessL2Tx(exitTree, &l2txs[i])
		if err != nil {
			return nil, common.Wrap(err)
		}
		// if txProcessor.zki != nil {
		// 	l2TxData, err := l2txs[i].L2Tx().BytesDataAvailability(txProcessor.zki.Metadata.NLevels)
		// 	if err != nil {
		// 		return nil, common.Wrap(err)
		// 	}
		// 	txProcessor.zki.Metadata.L2TxsData = append(txProcessor.zki.Metadata.L2TxsData, l2TxData)

		// 	// Intermediate States
		// 	if txProcessor.txIndex < nTx-1 {
		// 		txProcessor.zki.ISOutIdx[txProcessor.txIndex] = txProcessor.state.CurrentIdx().BigInt()
		// 		txProcessor.zki.ISStateRoot[txProcessor.txIndex] = txProcessor.state.MT.Root().BigInt()
		// 		txProcessor.zki.ISAccFeeOut[txProcessor.txIndex] = formatAccumulatedFees(collectedFees, txProcessor.zki.FeePlanTokens, coordIdxs)
		// 		if exitIdx == nil {
		// 			txProcessor.zki.ISExitRoot[txProcessor.txIndex] = exitTree.Root().BigInt()
		// 		}
		// 	}
		// }
		if txProcessor.state.Type() == statedb.TypeSynchronizer || txProcessor.state.Type() == statedb.TypeBatchBuilder {
			if exitIdx != nil && exitTree != nil && exitAccount != nil {
				exits[txProcessor.txIndex] = processedExit{
					exit:    true,
					newExit: newExit,
					idx:     *exitIdx,
					acc:     *exitAccount,
				}
			}
			txProcessor.txIndex++
		}
	}

	// if txProcessor.zki != nil {
	// 	// Fill the empty slots in the ZKInputs remaining after
	// 	// processing all L1 & L2 txs
	// 	txCompressedDataEmpty := common.TxCompressedDataEmpty(txProcessor.config.ChainID)
	// 	last := txProcessor.txIndex - 1
	// 	if txProcessor.txIndex == 0 {
	// 		last = 0
	// 	}
	// 	for i := last; i < int(txProcessor.config.MaxTx); i++ {
	// 		if i < int(txProcessor.config.MaxTx)-1 {
	// 			txProcessor.zki.ISOutIdx[i] = txProcessor.state.CurrentAccountIdx().BigInt()
	// 			txProcessor.zki.ISStateRoot[i] = txProcessor.state.AccountTree.Root().BigInt()
	// 			txProcessor.zki.ISAccFeeOut[i] = formatAccumulatedFees(collectedFees,
	// 				txProcessor.zki.FeePlanTokens, coordIdxs)
	// 			txProcessor.zki.ISExitRoot[i] = exitTree.Root().BigInt()
	// 		}
	// 		if i >= txProcessor.txIndex {
	// 			txProcessor.zki.TxCompressedData[i] = txCompressedDataEmpty
	// 		}
	// 	}
	// 	isFinalAccFee := formatAccumulatedFees(collectedFees, txProcessor.zki.FeePlanTokens, coordIdxs)
	// 	copy(txProcessor.zki.ISFinalAccFee, isFinalAccFee)
	// 	// before computing the Fees txs, set the ISInitStateRootFee
	// 	txProcessor.zki.ISInitStateRootFee = txProcessor.state.AccountTree.Root().BigInt()
	// }

	// // distribute the AccumulatedFees from the processed L2Txs into the
	// // Coordinator Idxs
	// indexFee := 0
	// for _, idx := range coordIdxs {
	// 	accumulatedFee := txProcessor.AccumulatedFees[idx]

	// 	// send the fee to the Idx of the Coordinator for the TokenID
	// 	// (even if the AccumulatedFee==0, as is how the zk circuit
	// 	// works)
	// 	accCoord, err := txProcessor.state.GetAccount(idx)
	// 	if err != nil {
	// 		log.Errorw("Can not distribute accumulated fees to coordinator account: "+
	// 			"No coord Idx to receive fee", "idx", idx)
	// 		return nil, common.Wrap(err)
	// 	}
	// 	// if txProcessor.zki != nil {
	// 	// 	txProcessor.zki.TokenID3[indexFee] = accCoord.TokenID.BigInt()
	// 	// 	txProcessor.zki.Nonce3[indexFee] = accCoord.Nonce.BigInt()
	// 	// 	coordBJJSign, coordBJJY := babyjub.UnpackSignY(accCoord.BJJ)
	// 	// 	if coordBJJSign {
	// 	// 		txProcessor.zki.Sign3[indexFee] = big.NewInt(1)
	// 	// 	}
	// 	// 	txProcessor.zki.Ay3[indexFee] = coordBJJY
	// 	// 	txProcessor.zki.Balance3[indexFee] = accCoord.Balance
	// 	// 	txProcessor.zki.EthAddr3[indexFee] = common.EthAddrToBigInt(accCoord.EthAddr)
	// 	// }
	// 	accCoord.Balance = new(big.Int).Add(accCoord.Balance, accumulatedFee)
	// 	pFee, err := txProcessor.updateAccount(idx, accCoord)
	// 	if err != nil {
	// 		log.Error(err)
	// 		return nil, common.Wrap(err)
	// 	}
	// 	if txProcessor.zki != nil {
	// 		txProcessor.zki.Siblings3[indexFee] = siblingsToZKInputFormat(pFee.Siblings)
	// 		if indexFee < len(txProcessor.zki.ISStateRootFee) {
	// 			txProcessor.zki.ISStateRootFee[indexFee] = txProcessor.state.AccountTree.Root().BigInt()
	// 		}
	// 	}
	// 	indexFee++
	// }
	// if txProcessor.zki != nil {
	// 	for i := len(txProcessor.AccumulatedFees); i < int(txProcessor.config.MaxFeeTx)-1; i++ {
	// 		txProcessor.zki.ISStateRootFee[i] = txProcessor.state.AccountTree.Root().BigInt()
	// 	}
	// 	// add Coord Idx to ZKInputs.FeeTxsData
	// 	for i := 0; i < len(coordIdxs); i++ {
	// 		txProcessor.zki.FeeIdxs[i] = coordIdxs[i].BigInt()
	// 	}
	// }

	if txProcessor.state.Type() == statedb.TypeTxSelector {
		return nil, nil
	}

	if txProcessor.state.Type() == statedb.TypeSynchronizer {
		// once all txs processed (exitTree root frozen), for each Exit,
		// generate common.ExitInfo data
		var exitInfos []common.ExitInfo
		var exitIdxs []common.AccountIdx
		exitInfosByIdx := make(map[common.AccountIdx]*common.ExitInfo)
		for i := 0; i < nTx; i++ {
			if !exits[i].exit {
				continue
			}
			exitIdx := exits[i].idx
			exitAccount := exits[i].acc

			// 0. generate MerkleProof
			p, err := exitTree.GenerateSCVerifierProof(exitIdx.BigInt(), nil)
			if err != nil {
				return nil, common.Wrap(err)
			}

			// 1. generate common.ExitInfo
			ei := common.ExitInfo{
				AccountIdx:  exitIdx,
				MerkleProof: p,
				Balance:     exitAccount.Balance,
			}
			if _, ok := exitInfosByIdx[exitIdx]; !ok {
				exitIdxs = append(exitIdxs, exitIdx)
			}
			exitInfosByIdx[exitIdx] = &ei
		}
		for _, idx := range exitIdxs {
			exitInfos = append(exitInfos, *exitInfosByIdx[idx])
		}
		// return exitInfos, createdAccounts and collectedFees, so Synchronizer will
		// be able to store it into HistoryDB for the concrete BatchNum
		return &ProcessTxOutput{
			ZKInputs:        nil,
			ExitInfos:       exitInfos,
			CreatedAccounts: createdAccounts,
			// CoordinatorIdxsMap: coordIdxsMap,
			// CollectedFees:      collectedFees,
			UpdatedAccounts: txProcessor.updatedAccounts,
		}, nil
	}

	// // compute last ZKInputs parameters
	// txProcessor.zki.GlobalChainID = big.NewInt(int64(txProcessor.config.ChainID))
	// txProcessor.zki.Metadata.NewStateRootRaw = txProcessor.state.AccountTree.Root()
	// txProcessor.zki.Metadata.NewExitRootRaw = exitTree.Root()

	// return ZKInputs as the BatchBuilder will return it to forge the Batch
	return &ProcessTxOutput{
		ZKInputs:        txProcessor.zki,
		ExitInfos:       nil,
		CreatedAccounts: nil,
		// CoordinatorIdxsMap: coordIdxsMap,
		CollectedFees: nil,
	}, nil
}

// ProcessL1Tx process the given L1Tx applying the needed updates to the
// StateDB depending on the transaction Type. It returns the 3 parameters
// related to the Exit (in case of): Idx, ExitAccount, boolean determining if
// the Exit created a new Leaf in the ExitTree.
// And another *common.Account parameter which contains the created account in
// case that has been a new created account and that the StateDB is of type
// TypeSynchronizer.
func (txProcessor *TxProcessor) ProcessL1Tx(exitTree *merkletree.MerkleTree, tx *common.L1Tx) (*common.AccountIdx,
	*common.Account, bool, *common.Account, error) {
	// ZKInputs
	if txProcessor.zki != nil {
		// Txs
		var err error
		txProcessor.zki.TxCompressedData[txProcessor.txIndex], err = tx.TxCompressedData(txProcessor.config.ChainID)
		if err != nil {
			log.Error(err)
			return nil, nil, false, nil, common.Wrap(err)
		}
		txProcessor.zki.FromIdx[txProcessor.txIndex] = tx.FromIdx.BigInt()
		txProcessor.zki.ToIdx[txProcessor.txIndex] = tx.ToIdx.BigInt()
		txProcessor.zki.OnChain[txProcessor.txIndex] = big.NewInt(1)

		// L1Txs
		depositAmountF40, err := common.NewFloat40(tx.DepositAmount)
		if err != nil {
			return nil, nil, false, nil, common.Wrap(err)
		}
		txProcessor.zki.DepositAmountF[txProcessor.txIndex] = big.NewInt(int64(depositAmountF40))
		txProcessor.zki.FromEthAddr[txProcessor.txIndex] = common.EthAddrToBigInt(tx.FromEthAddr)
		if tx.FromBJJ != common.EmptyBJJComp {
			txProcessor.zki.FromBJJCompressed[txProcessor.txIndex] = BJJCompressedTo256BigInts(tx.FromBJJ)
		}

		// Intermediate States, for all the transactions except for the last one
		if txProcessor.txIndex < len(txProcessor.zki.ISOnChain) { // len(txProcessor.zki.ISOnChain) == nTx
			txProcessor.zki.ISOnChain[txProcessor.txIndex] = big.NewInt(1)
		}

		if tx.Type == common.TxTypeForceTransfer ||
			tx.Type == common.TxTypeDepositTransfer ||
			tx.Type == common.TxTypeCreateAccountDepositTransfer ||
			tx.Type == common.TxTypeForceExit {
			// in the cases where at L1Tx there is usage of the
			// Amount parameter, add it at the ZKInputs.AmountF
			// slot
			amountF40, err := common.NewFloat40(tx.Amount)
			if err != nil {
				return nil, nil, false, nil, common.Wrap(err)
			}
			txProcessor.zki.AmountF[txProcessor.txIndex] = big.NewInt(int64(amountF40))
		}
	}

	switch tx.Type {
	case common.TxTypeCreateAccountDeposit:
		txProcessor.computeEffectiveAmounts(tx)

		// add new account to the MT, update balance of the MT account
		err := txProcessor.applyCreateAccount(tx)
		if err != nil {
			log.Error(err)
			return nil, nil, false, nil, common.Wrap(err)
		}
	case common.TxTypeDeposit:
		txProcessor.computeEffectiveAmounts(tx)

		// update balance of the MT account
		err := txProcessor.applyDeposit(tx, false)
		if err != nil {
			log.Error(err)
			return nil, nil, false, nil, common.Wrap(err)
		}
	case common.TxTypeForceExit:
		txProcessor.computeEffectiveAmounts(tx)

		// execute exit flow
		// coordIdxsMap is 'nil', as at L1Txs there is no L2 fees
		exitAccount, newExit, err := txProcessor.applyExit(nil, nil, exitTree, tx.Tx(), tx.Amount)
		if err != nil {
			log.Error(err)
			return nil, nil, false, nil, common.Wrap(err)
		}
		return &tx.FromIdx, exitAccount, newExit, nil, nil
	default:
	}

	var createdAccount *common.Account
	if txProcessor.state.Type() == statedb.TypeSynchronizer &&
		(tx.Type == common.TxTypeCreateAccountDeposit ||
			tx.Type == common.TxTypeCreateAccountDepositTransfer) {
		var err error
		createdAccount, err = txProcessor.state.GetAccount(txProcessor.state.CurrentAccountIdx())
		if err != nil {
			log.Error(err)
			return nil, nil, false, nil, common.Wrap(err)
		}
	}

	return nil, nil, false, createdAccount, nil
}

// ProcessL2Tx process the given L2Tx applying the needed updates to the
// StateDB depending on the transaction Type. It returns the 3 parameters
// related to the Exit (in case of): Idx, ExitAccount, boolean determining if
// the Exit created a new Leaf in the ExitTree.
func (txProcessor *TxProcessor) ProcessL2Tx(exitTree *merkletree.MerkleTree,
	tx *common.PoolL2Tx) (*common.AccountIdx, *common.Account, bool, error) {
	var err error
	// if tx.ToAccountIdx==0, get toAccountIdx by ToEthAddr or ToBJJ
	if tx.ToIdx == common.AccountIdx(0) && tx.AuxToIdx == common.AccountIdx(0) {
		if txProcessor.state.Type() == statedb.TypeSynchronizer {
			// this in TypeSynchronizer should never be reached
			log.Error("WARNING: In StateDB with Synchronizer mode L2.ToIdx can't be 0")
			return nil, nil, false,
				common.Wrap(fmt.Errorf("in StateDB with Synchronizer mode L2.ToIdx can't be 0"))
		}
		// case when tx.Type == common.TxTypeTransferToEthAddr or
		// common.TxTypeTransferToBJJ:
		_, err := txProcessor.state.GetAccount(tx.FromIdx)
		if err != nil {
			return nil, nil, false, common.Wrap(err)
		}
		tx.AuxToIdx, err = txProcessor.state.GetIdxByEthAddrBJJ(tx.ToEthAddr, tx.ToBJJ)
		if err != nil {
			return nil, nil, false, common.Wrap(err)
		}
	}

	// ZKInputs
	if txProcessor.zki != nil {
		// Txs
		txProcessor.zki.TxCompressedData[txProcessor.txIndex], err = tx.TxCompressedData(txProcessor.config.ChainID)
		if err != nil {
			return nil, nil, false, common.Wrap(err)
		}
		txProcessor.zki.TxCompressedDataV2[txProcessor.txIndex], err = tx.TxCompressedDataV2()
		if err != nil {
			return nil, nil, false, common.Wrap(err)
		}
		txProcessor.zki.FromIdx[txProcessor.txIndex] = tx.FromIdx.BigInt()
		txProcessor.zki.ToIdx[txProcessor.txIndex] = tx.ToIdx.BigInt()

		// fill AuxToIdx if needed
		if tx.ToIdx == 0 {
			// use toIdx that can have been filled by tx.ToIdx or
			// if tx.Idx==0 (this case), toIdx is filled by the Idx
			// from db by ToEthAddr&ToBJJ
			txProcessor.zki.AuxToIdx[txProcessor.txIndex] = tx.AuxToIdx.BigInt()
		}

		if tx.ToBJJ != common.EmptyBJJComp {
			_, txProcessor.zki.ToBJJAy[txProcessor.txIndex] = babyjub.UnpackSignY(tx.ToBJJ)
		}
		txProcessor.zki.ToEthAddr[txProcessor.txIndex] = common.EthAddrToBigInt(tx.ToEthAddr)

		txProcessor.zki.OnChain[txProcessor.txIndex] = big.NewInt(0)
		amountF40, err := common.NewFloat40(tx.Amount)
		if err != nil {
			return nil, nil, false, common.Wrap(err)
		}
		txProcessor.zki.AmountF[txProcessor.txIndex] = big.NewInt(int64(amountF40))
		txProcessor.zki.NewAccount[txProcessor.txIndex] = big.NewInt(0)
		txProcessor.zki.MaxNumBatch[txProcessor.txIndex] = big.NewInt(int64(tx.MaxNumBatch))

		// Rq fields: set zki to link the requested tx
		if tx.RqOffset != 0 {
			if tx.RqOffset > 7 { //nolint:gomnd
				return nil, nil, false, common.New(ErrInvalidRqOffset)
			}
			rqOffset := big.NewInt(int64(tx.RqOffset))
			txProcessor.zki.RqOffset[txProcessor.txIndex] = rqOffset
			txProcessor.zki.RqTxCompressedDataV2[txProcessor.txIndex], err = tx.RqTxCompressedDataV2()
			if err != nil {
				return nil, nil, false, common.Wrap(err)
			}
			if tx.RqToBJJ != common.EmptyBJJComp {
				_, txProcessor.zki.RqToBJJAy[txProcessor.txIndex] = babyjub.UnpackSignY(tx.RqToBJJ)
			}
			txProcessor.zki.RqToEthAddr[txProcessor.txIndex] = common.EthAddrToBigInt(tx.RqToEthAddr)
		}

		signature, err := tx.Signature.Decompress()
		if err != nil {
			log.Error(err)
			return nil, nil, false, common.Wrap(err)
		}
		txProcessor.zki.S[txProcessor.txIndex] = signature.S
		txProcessor.zki.R8x[txProcessor.txIndex] = signature.R8.X
		txProcessor.zki.R8y[txProcessor.txIndex] = signature.R8.Y
	}

	// if StateDB type==TypeSynchronizer, will need to add Nonce
	if txProcessor.state.Type() == statedb.TypeSynchronizer {
		// as tType==TypeSynchronizer, always tx.ToIdx!=0
		acc, err := txProcessor.state.GetAccount(tx.FromIdx)
		if err != nil {
			log.Errorw("GetAccount", "fromIdx", tx.FromIdx, "err", err)
			return nil, nil, false, common.Wrap(err)
		}
		tx.Nonce = acc.Nonce
	}

	switch tx.Type {
	case common.TxTypeCreateVouch, common.TxTypeDeleteVouch:
		// go to the MT account of sender and receiver, and update
		// balance & nonce
		err = txProcessor.applyTransfer(nil, nil, tx.Tx(), tx.AuxToIdx)
		if err != nil {
			log.Error(err)
			return nil, nil, false, common.Wrap(err)
		}
	case common.TxTypeExit:
		// execute exit flow
		exitAccount, newExit, err := txProcessor.applyExit(nil, nil, exitTree,
			tx.Tx(), tx.Amount)
		if err != nil {
			log.Error(err)
			return nil, nil, false, common.Wrap(err)
		}
		return &tx.FromIdx, exitAccount, newExit, nil
	default:
	}
	return nil, nil, false, nil
}

// applyCreateAccount creates a new account in the account of the depositer, it
// stores the deposit value
func (txProcessor *TxProcessor) applyCreateAccount(tx *common.L1Tx) error {
	account := &common.Account{
		Nonce:   0,
		Balance: tx.EffectiveDepositAmount,
		BJJ:     tx.FromBJJ,
		EthAddr: tx.FromEthAddr,
	}

	_, err := txProcessor.createAccount(common.AccountIdx(txProcessor.state.CurrentAccountIdx()+1), account)
	if err != nil {
		return common.Wrap(err)
	}
	// if txProcessor.zki != nil {
	// 	txProcessor.zki.TokenID1[txProcessor.txIndex] = tx.TokenID.BigInt()
	// 	txProcessor.zki.Nonce1[txProcessor.txIndex] = big.NewInt(0)
	// 	fromBJJSign, fromBJJY := babyjub.UnpackSignY(tx.FromBJJ)
	// 	if fromBJJSign {
	// 		txProcessor.zki.Sign1[txProcessor.txIndex] = big.NewInt(1)
	// 	}
	// 	txProcessor.zki.Ay1[txProcessor.txIndex] = fromBJJY
	// 	txProcessor.zki.Balance1[txProcessor.txIndex] = tx.EffectiveDepositAmount
	// 	txProcessor.zki.EthAddr1[txProcessor.txIndex] = common.EthAddrToBigInt(tx.FromEthAddr)
	// 	txProcessor.zki.Siblings1[txProcessor.txIndex] = siblingsToZKInputFormat(p.Siblings)
	// 	if p.IsOld0 {
	// 		txProcessor.zki.IsOld0_1[txProcessor.txIndex] = big.NewInt(1)
	// 	}
	// 	txProcessor.zki.OldKey1[txProcessor.txIndex] = p.OldKey.BigInt()
	// 	txProcessor.zki.OldValue1[txProcessor.txIndex] = p.OldValue.BigInt()

	// 	txProcessor.zki.Metadata.NewLastIdxRaw = txProcessor.state.CurrentIdx() + 1

	// 	txProcessor.zki.AuxFromIdx[txProcessor.txIndex] = common.Idx(txProcessor.state.CurrentIdx() + 1).BigInt()
	// 	txProcessor.zki.NewAccount[txProcessor.txIndex] = big.NewInt(1)

	// 	if txProcessor.txIndex < len(txProcessor.zki.ISOnChain) { // len(txProcessor.zki.ISOnChain) == nTx
	// 		// intermediate states
	// 		txProcessor.zki.ISOnChain[txProcessor.txIndex] = big.NewInt(1)
	// 	}
	// }

	return txProcessor.state.SetCurrentAccountIdx(txProcessor.state.CurrentAccountIdx() + 1)
}

// createAccount is a wrapper over the StateDB.CreateAccount method that also
// stores the created account in the updatedAccounts map in case the StateDB is
// of TypeSynchronizer
func (txProcessor *TxProcessor) createAccount(idx common.AccountIdx, account *common.Account) (
	*merkletree.CircomProcessorProof, error) {
	if txProcessor.state.Type() == statedb.TypeSynchronizer {
		account.Idx = idx
		txProcessor.updatedAccounts[idx] = account
	}
	return txProcessor.state.CreateAccount(idx, account)
}

// updateAccount is a wrapper over the StateDB.UpdateAccount method that also
// stores the updated account in the updatedAccounts map in case the StateDB is
// of TypeSynchronizer
func (txProcessor *TxProcessor) updateAccount(idx common.AccountIdx, account *common.Account) (
	*merkletree.CircomProcessorProof, error) {
	if txProcessor.state.Type() == statedb.TypeSynchronizer {
		account.Idx = idx
		txProcessor.updatedAccounts[idx] = account
	}
	return txProcessor.state.UpdateAccount(idx, account)
}

// applyDeposit updates the balance in the account of the depositer, if
// andTransfer parameter is set to true, the method will also apply the
// Transfer of the L1Tx/DepositTransfer
func (txProcessor *TxProcessor) applyDeposit(tx *common.L1Tx, transfer bool) error {
	accSender, err := txProcessor.state.GetAccount(tx.FromIdx)
	if err != nil {
		return common.Wrap(err)
	}

	// if txProcessor.zki != nil {
	// 	txProcessor.zki.TokenID1[txProcessor.txIndex] = accSender.TokenID.BigInt()
	// 	txProcessor.zki.Nonce1[txProcessor.txIndex] = accSender.Nonce.BigInt()
	// 	senderBJJSign, senderBJJY := babyjub.UnpackSignY(accSender.BJJ)
	// 	if senderBJJSign {
	// 		txProcessor.zki.Sign1[txProcessor.txIndex] = big.NewInt(1)
	// 	}
	// 	txProcessor.zki.Ay1[txProcessor.txIndex] = senderBJJY
	// 	txProcessor.zki.Balance1[txProcessor.txIndex] = accSender.Balance
	// 	txProcessor.zki.EthAddr1[txProcessor.txIndex] = common.EthAddrToBigInt(accSender.EthAddr)
	// }

	// add the deposit to the sender
	accSender.Balance = new(big.Int).Add(accSender.Balance, tx.EffectiveDepositAmount)
	// subtract amount to the sender
	accSender.Balance = new(big.Int).Sub(accSender.Balance, tx.EffectiveAmount)
	if accSender.Balance.Cmp(big.NewInt(0)) == -1 { // balance<0
		return newErrorNotEnoughBalance(tx.Tx())
	}

	// update sender account in localStateDB
	p, err := txProcessor.updateAccount(tx.FromIdx, accSender)
	if err != nil {
		return common.Wrap(err)
	}
	if txProcessor.zki != nil {
		txProcessor.zki.Siblings1[txProcessor.txIndex] = siblingsToZKInputFormat(p.Siblings)
		// IsOld0_1, OldKey1, OldValue1 not needed as this is not an insert
	}

	// in case that the tx is a L1Tx>DepositTransfer
	var accReceiver *common.Account
	if transfer {
		if tx.ToIdx == tx.FromIdx {
			accReceiver = accSender
		} else {
			accReceiver, err = txProcessor.state.GetAccount(tx.ToIdx)
			if err != nil {
				return common.Wrap(err)
			}
		}

		// if txProcessor.zki != nil {
		// 	txProcessor.zki.TokenID2[txProcessor.txIndex] = accReceiver.TokenID.BigInt()
		// 	txProcessor.zki.Nonce2[txProcessor.txIndex] = accReceiver.Nonce.BigInt()
		// 	receiverBJJSign, receiverBJJY := babyjub.UnpackSignY(accReceiver.BJJ)
		// 	if receiverBJJSign {
		// 		txProcessor.zki.Sign2[txProcessor.txIndex] = big.NewInt(1)
		// 	}
		// 	txProcessor.zki.Ay2[txProcessor.txIndex] = receiverBJJY
		// 	txProcessor.zki.Balance2[txProcessor.txIndex] = accReceiver.Balance
		// 	txProcessor.zki.EthAddr2[txProcessor.txIndex] = common.EthAddrToBigInt(accReceiver.EthAddr)
		// }

		// add amount to the receiver
		accReceiver.Balance = new(big.Int).Add(accReceiver.Balance, tx.EffectiveAmount)

		// update receiver account in localStateDB
		p, err := txProcessor.updateAccount(tx.ToIdx, accReceiver)
		if err != nil {
			return common.Wrap(err)
		}
		if txProcessor.zki != nil {
			txProcessor.zki.Siblings2[txProcessor.txIndex] = siblingsToZKInputFormat(p.Siblings)
			// IsOld0_2, OldKey2, OldValue2 not needed as this is not an insert
		}
	}

	return nil
}

// applyTransfer updates the balance & nonce in the account of the sender, and
// the balance in the account of the receiver.
// Parameter 'toIdx' should be at 0 if the tx already has tx.ToIdx!=0, if
// tx.ToIdx==0, then toIdx!=0, and will be used the toIdx parameter as Idx of
// the receiver. This parameter is used when the tx.ToIdx is not specified and
// the real ToIdx is found trhrough the ToEthAddr or ToBJJ.
func (txProcessor *TxProcessor) applyTransfer(coordIdxsMap map[common.TokenID]common.AccountIdx,
	collectedFees map[common.TokenID]*big.Int, tx common.Tx, auxToIdx common.AccountIdx) error {
	if auxToIdx == common.AccountIdx(0) {
		auxToIdx = tx.ToIdx
	}
	// get sender and receiver accounts from localStateDB
	accSender, err := txProcessor.state.GetAccount(tx.FromIdx)
	if err != nil {
		log.Error(err)
		return common.Wrap(err)
	}

	// if txProcessor.zki != nil {
	// 	// Set the State1 before updating the Sender leaf
	// 	txProcessor.zki.TokenID1[txProcessor.txIndex] = accSender.TokenID.BigInt()
	// 	txProcessor.zki.Nonce1[txProcessor.txIndex] = accSender.Nonce.BigInt()
	// 	senderBJJSign, senderBJJY := babyjub.UnpackSignY(accSender.BJJ)
	// 	if senderBJJSign {
	// 		txProcessor.zki.Sign1[txProcessor.txIndex] = big.NewInt(1)
	// 	}
	// 	txProcessor.zki.Ay1[txProcessor.txIndex] = senderBJJY
	// 	txProcessor.zki.Balance1[txProcessor.txIndex] = accSender.Balance
	// 	txProcessor.zki.EthAddr1[txProcessor.txIndex] = common.EthAddrToBigInt(accSender.EthAddr)
	// }
	if !tx.IsL1 { // L2
		// increment nonce
		accSender.Nonce++

		// compute fee and subtract it from the accSender
		// fee, err := common.CalcFeeAmount(tx.Amount, *tx.Fee)
		// if err != nil {
		// 	return common.Wrap(err)
		// }
		// feeAndAmount := new(big.Int).Add(tx.Amount, fee)
		// accSender.Balance = new(big.Int).Sub(accSender.Balance, feeAndAmount)
		// if accSender.Balance.Cmp(big.NewInt(0)) == -1 { // balance<0
		// 	return newErrorNotEnoughBalance(tx)
		// }

		// if _, ok := coordIdxsMap[accSender.TokenID]; ok {
		// 	accCoord, err := txProcessor.state.GetAccount(coordIdxsMap[accSender.TokenID])
		// 	if err != nil {
		// 		return common.Wrap(
		// 			fmt.Errorf("Can not use CoordIdx that does not exist in the tree. TokenID: %d, CoordIdx: %d",
		// 				accSender.TokenID, coordIdxsMap[accSender.TokenID]))
		// 	}
		// 	// accumulate the fee for the Coord account
		// 	accumulated, ok := txProcessor.AccumulatedFees[accCoord.Idx]
		// 	if !ok {
		// 		accumulated = big.NewInt(0)
		// 		txProcessor.AccumulatedFees[accCoord.Idx] = accumulated
		// 	}
		// 	accumulated.Add(accumulated, fee)

		// 	if txProcessor.state.Type() == statedb.TypeSynchronizer ||
		// 		txProcessor.state.Type() == statedb.TypeBatchBuilder {
		// 		collected := collectedFees[accCoord.TokenID]
		// 		collected.Add(collected, fee)
		// 	}
		// } else {
		// 	log.Debugw("No coord Idx to receive fee", "tx", tx)
		// }
	} else {
		accSender.Balance = new(big.Int).Sub(accSender.Balance, tx.Amount)
		if accSender.Balance.Cmp(big.NewInt(0)) == -1 { // balance<0
			return newErrorNotEnoughBalance(tx)
		}
	}

	// update sender account in localStateDB
	pSender, err := txProcessor.updateAccount(tx.FromIdx, accSender)
	if err != nil {
		log.Error(err)
		return common.Wrap(err)
	}
	if txProcessor.zki != nil {
		txProcessor.zki.Siblings1[txProcessor.txIndex] = siblingsToZKInputFormat(pSender.Siblings)
	}

	var accReceiver *common.Account
	if auxToIdx == tx.FromIdx {
		// if Sender is the Receiver, reuse 'accSender' pointer,
		// because in the DB the account for 'auxToIdx' won't be
		// updated yet
		accReceiver = accSender
	} else {
		accReceiver, err = txProcessor.state.GetAccount(auxToIdx)
		if err != nil {
			log.Error(err, auxToIdx)
			return common.Wrap(err)
		}
	}
	if txProcessor.zki != nil {
		// Set the State2 before updating the Receiver leaf
		txProcessor.zki.Nonce2[txProcessor.txIndex] = accReceiver.Nonce.BigInt()
		receiverBJJSign, receiverBJJY := babyjub.UnpackSignY(accReceiver.BJJ)
		if receiverBJJSign {
			txProcessor.zki.Sign2[txProcessor.txIndex] = big.NewInt(1)
		}
		txProcessor.zki.Ay2[txProcessor.txIndex] = receiverBJJY
		txProcessor.zki.Balance2[txProcessor.txIndex] = accReceiver.Balance
		txProcessor.zki.EthAddr2[txProcessor.txIndex] = common.EthAddrToBigInt(accReceiver.EthAddr)
	}

	// add amount-feeAmount to the receiver
	// accReceiver.Balance = new(big.Int).Add(accReceiver.Balance, tx.Amount)

	// update receiver account in localStateDB
	pReceiver, err := txProcessor.updateAccount(auxToIdx, accReceiver)
	if err != nil {
		return common.Wrap(err)
	}
	if txProcessor.zki != nil {
		txProcessor.zki.Siblings2[txProcessor.txIndex] = siblingsToZKInputFormat(pReceiver.Siblings)
	}

	return nil
}

// It returns the ExitAccount and a boolean determining if the Exit created a
// new Leaf in the ExitTree.
func (txProcessor *TxProcessor) applyExit(coordIdxsMap map[common.TokenID]common.AccountIdx,
	collectedFees map[common.TokenID]*big.Int, exitTree *merkletree.MerkleTree,
	tx common.Tx, originalAmount *big.Int) (*common.Account, bool, error) {
	// 0. subtract tx.Amount from current Account in StateMT
	// add the tx.Amount into the Account (tx.FromIdx) in the ExitMT
	acc, err := txProcessor.state.GetAccount(tx.FromIdx)
	if err != nil {
		return nil, false, common.Wrap(err)
	}
	// if txProcessor.zki != nil {
	// 	txProcessor.zki.TokenID1[txProcessor.txIndex] = acc.TokenID.BigInt()
	// 	txProcessor.zki.Nonce1[txProcessor.txIndex] = acc.Nonce.BigInt()
	// 	accBJJSign, accBJJY := babyjub.UnpackSignY(acc.BJJ)
	// 	if accBJJSign {
	// 		txProcessor.zki.Sign1[txProcessor.txIndex] = big.NewInt(1)
	// 	}
	// 	txProcessor.zki.Ay1[txProcessor.txIndex] = accBJJY
	// 	txProcessor.zki.Balance1[txProcessor.txIndex] = acc.Balance
	// 	txProcessor.zki.EthAddr1[txProcessor.txIndex] = common.EthAddrToBigInt(acc.EthAddr)
	// }

	if !tx.IsL1 {
		// increment nonce
		acc.Nonce++

		// // compute fee and subtract it from the accSender
		// fee, err := common.CalcFeeAmount(tx.Amount, *tx.Fee)
		// if err != nil {
		// 	return nil, false, common.Wrap(err)
		// }
		// feeAndAmount := new(big.Int).Add(tx.Amount, fee)
		// acc.Balance = new(big.Int).Sub(acc.Balance, feeAndAmount)
		// if acc.Balance.Cmp(big.NewInt(0)) == -1 { // balance<0
		// 	return nil, false, newErrorNotEnoughBalance(tx)
		// }

		// if _, ok := coordIdxsMap[acc.TokenID]; ok {
		// 	accCoord, err := txProcessor.state.GetAccount(coordIdxsMap[acc.TokenID])
		// 	if err != nil {
		// 		return nil, false, common.Wrap(
		// 			fmt.Errorf("Can not use CoordIdx that does not exist in the tree. TokenID: %d, CoordIdx: %d",
		// 				acc.TokenID, coordIdxsMap[acc.TokenID]))
		// 	}

		// 	// accumulate the fee for the Coord account
		// 	accumulated, ok := txProcessor.AccumulatedFees[accCoord.Idx]
		// 	if !ok {
		// 		accumulated = big.NewInt(0)
		// 		txProcessor.AccumulatedFees[accCoord.Idx] = accumulated
		// 	}
		// 	accumulated.Add(accumulated, fee)

		// 	if txProcessor.state.Type() == statedb.TypeSynchronizer ||
		// 		txProcessor.state.Type() == statedb.TypeBatchBuilder {
		// 		collected := collectedFees[accCoord.TokenID]
		// 		collected.Add(collected, fee)
		// 	}
		// } else {
		// 	log.Debugw("No coord Idx to receive fee", "tx", tx)
		// }
	} else {
		acc.Balance = new(big.Int).Sub(acc.Balance, tx.Amount)
		if acc.Balance.Cmp(big.NewInt(0)) == -1 { // balance<0
			return nil, false, newErrorNotEnoughBalance(tx)
		}
	}

	p, err := txProcessor.updateAccount(tx.FromIdx, acc)
	if err != nil {
		return nil, false, common.Wrap(err)
	}
	if txProcessor.zki != nil {
		txProcessor.zki.Siblings1[txProcessor.txIndex] = siblingsToZKInputFormat(p.Siblings)
	}

	if exitTree == nil {
		return nil, false, nil
	}

	// Do not add the Exit when Amount=0, not EffectiveAmount=0. In
	// txprocessor.applyExit function, the tx.Amount is in reality the
	// EffectiveAmount, that's why is used here the originalAmount
	// parameter, which contains the real value of the tx.Amount (not
	// tx.EffectiveAmount).  This is a particularity of the approach of the
	// circuit, the idea will be in the future to update the circuit and
	// when Amount>0 but EffectiveAmount=0, to not add the Exit in the
	// Exits MerkleTree, but for the moment the Go code is adapted to the
	// circuit.
	if originalAmount.Cmp(big.NewInt(0)) == 0 { // Amount == 0
		// if the Exit Amount==0, the Exit is not added to the ExitTree
		return nil, false, nil
	}

	exitAccount, err := statedb.GetAccountInTreeDB(exitTree.DB(), tx.FromIdx)
	if common.Unwrap(err) == db.ErrNotFound {
		// 1a. if idx does not exist in exitTree:
		// add new leaf 'ExitTreeLeaf', where ExitTreeLeaf.Balance =
		// exitAmount (exitAmount=tx.Amount)
		exitAccount := &common.Account{
			// TokenID: acc.TokenID,
			Nonce: common.Nonce(0),
			// as is a common.Tx, the tx.Amount is already an
			// EffectiveAmount
			Balance: tx.Amount,
			BJJ:     acc.BJJ,
			EthAddr: acc.EthAddr,
		}
		// if txProcessor.zki != nil {
		// 	// Set the State2 before creating the Exit leaf
		// 	txProcessor.zki.TokenID2[txProcessor.txIndex] = acc.TokenID.BigInt()
		// 	txProcessor.zki.Nonce2[txProcessor.txIndex] = big.NewInt(0)
		// 	accBJJSign, accBJJY := babyjub.UnpackSignY(acc.BJJ)
		// 	if accBJJSign {
		// 		txProcessor.zki.Sign2[txProcessor.txIndex] = big.NewInt(1)
		// 	}
		// 	txProcessor.zki.Ay2[txProcessor.txIndex] = accBJJY
		// 	// Balance2 contains the ExitLeaf Balance before the
		// 	// leaf update, which is 0
		// 	txProcessor.zki.Balance2[txProcessor.txIndex] = big.NewInt(0)
		// 	txProcessor.zki.EthAddr2[txProcessor.txIndex] = common.EthAddrToBigInt(acc.EthAddr)
		// 	// as Leaf didn't exist in the ExitTree, set NewExit[i]=1
		// 	txProcessor.zki.NewExit[txProcessor.txIndex] = big.NewInt(1)
		// }
		p, err = statedb.CreateAccountInTreeDB(exitTree.DB(), exitTree, tx.FromIdx, exitAccount)
		if err != nil {
			return nil, false, common.Wrap(err)
		}
		if txProcessor.zki != nil {
			txProcessor.zki.Siblings2[txProcessor.txIndex] = siblingsToZKInputFormat(p.Siblings)
			if p.IsOld0 {
				txProcessor.zki.IsOld0_2[txProcessor.txIndex] = big.NewInt(1)
			}
			if txProcessor.txIndex < len(txProcessor.zki.ISExitRoot) {
				txProcessor.zki.ISExitRoot[txProcessor.txIndex] = exitTree.Root().BigInt()
			}
			txProcessor.zki.OldKey2[txProcessor.txIndex] = p.OldKey.BigInt()
			txProcessor.zki.OldValue2[txProcessor.txIndex] = p.OldValue.BigInt()
		}
		return exitAccount, true, nil
	} else if err != nil {
		return nil, false, common.Wrap(err)
	}

	// 1b. if idx already exist in exitTree:
	// if txProcessor.zki != nil {
	// 	// Set the State2 before updating the Exit leaf
	// 	txProcessor.zki.TokenID2[txProcessor.txIndex] = acc.TokenID.BigInt()
	// 	// increment nonce from existing ExitLeaf
	// 	txProcessor.zki.Nonce2[txProcessor.txIndex] = exitAccount.Nonce.BigInt()
	// 	accBJJSign, accBJJY := babyjub.UnpackSignY(acc.BJJ)
	// 	if accBJJSign {
	// 		txProcessor.zki.Sign2[txProcessor.txIndex] = big.NewInt(1)
	// 	}
	// 	txProcessor.zki.Ay2[txProcessor.txIndex] = accBJJY
	// 	// Balance2 contains the ExitLeaf Balance before the leaf
	// 	// update
	// 	txProcessor.zki.Balance2[txProcessor.txIndex] = exitAccount.Balance
	// 	txProcessor.zki.EthAddr2[txProcessor.txIndex] = common.EthAddrToBigInt(acc.EthAddr)
	// }

	// update account, where account.Balance += exitAmount
	exitAccount.Balance = new(big.Int).Add(exitAccount.Balance, tx.Amount)
	p, err = statedb.UpdateAccountInTreeDB(exitTree.DB(), exitTree, tx.FromIdx, exitAccount)
	if err != nil {
		return nil, false, common.Wrap(err)
	}

	if txProcessor.zki != nil {
		txProcessor.zki.Siblings2[txProcessor.txIndex] = siblingsToZKInputFormat(p.Siblings)
		if p.IsOld0 {
			txProcessor.zki.IsOld0_2[txProcessor.txIndex] = big.NewInt(1)
		}
		txProcessor.zki.OldKey2[txProcessor.txIndex] = p.OldKey.BigInt()
		txProcessor.zki.OldValue2[txProcessor.txIndex] = p.OldValue.BigInt()
		if txProcessor.txIndex < len(txProcessor.zki.ISExitRoot) {
			txProcessor.zki.ISExitRoot[txProcessor.txIndex] = exitTree.Root().BigInt()
		}
	}

	return exitAccount, false, nil
}

// computeEffectiveAmounts checks that the L1Tx data is correct
func (txProcessor *TxProcessor) computeEffectiveAmounts(tx *common.L1Tx) {
	tx.EffectiveAmount = tx.Amount
	tx.EffectiveDepositAmount = tx.DepositAmount

	if tx.Type == common.TxTypeCreateAccountDeposit {
		return
	}

	accSender, err := txProcessor.state.GetAccount(tx.FromIdx)
	if err != nil {
		log.Debugf("EffectiveAmount & EffectiveDepositAmount = 0: can not get account for tx.FromIdx: %d",
			tx.FromIdx)
		tx.EffectiveDepositAmount = big.NewInt(0)
		tx.EffectiveAmount = big.NewInt(0)
		return
	}

	// check that Sender has enough balance
	bal := accSender.Balance
	if tx.DepositAmount != nil {
		bal = new(big.Int).Add(bal, tx.EffectiveDepositAmount)
	}
	cmp := bal.Cmp(tx.Amount)
	if cmp == -1 {
		log.Debugf("EffectiveAmount = 0: Not enough funds (%s<%s)", bal.String(), tx.Amount.String())
		tx.EffectiveAmount = big.NewInt(0)
		return
	}

	// check that the tx.FromEthAddr is the same than the EthAddress of the
	// Sender
	if !bytes.Equal(tx.FromEthAddr.Bytes(), accSender.EthAddr.Bytes()) {
		log.Debugf("EffectiveAmount = 0: tx.FromEthAddr (%s) must be the same EthAddr of "+
			"the sender account by the Idx (%s)",
			tx.FromEthAddr.Hex(), accSender.EthAddr.Hex())
		tx.EffectiveAmount = big.NewInt(0)
		return
	}

	if tx.ToIdx == common.AccountIdx(1) || tx.ToIdx == common.AccountIdx(0) {
		// if transfer is Exit type, there are no more checks
		return
	}

}
