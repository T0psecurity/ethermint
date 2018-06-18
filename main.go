package main

import (
	"bytes"
	"fmt"

	eth_common "github.com/ethereum/go-ethereum/common"
	eth_state "github.com/ethereum/go-ethereum/core/state"
	eth_ethdb "github.com/ethereum/go-ethereum/ethdb"
	eth_trie "github.com/ethereum/go-ethereum/trie"

	dbm "github.com/tendermint/tmlibs/db"
	"github.com/tendermint/go-amino"
	"github.com/cosmos/cosmos-sdk/store"
	"github.com/cosmos/cosmos-sdk/types"
)

var (
	// Key for the sub-store with Ethereum accounts
	AccountsKey = types.NewKVStoreKey("account")
	// Key for the sub-store with storage data of Ethereum contracts
	StorageKey = types.NewKVStoreKey("storage")
	// Key for the sub-store with the code for contracts
	CodeKey = types.NewKVStoreKey("code")
)

// This is what stored in the lookupDb
type LookupValue struct {
	VersionId int64
}

// Implementation of eth_state.Database
type OurDatabase struct {
	stateStore        store.CommitMultiStore // For the history of accounts <balance, nonce, storage root hash, code hash>
										     // Also, for the history of contract data (effects of SSTORE instruction)
	lookupDb          dbm.DB // Maping [trie_root_hash] => <version_id>.
	                         // This mapping exists so that we can implement OpenTrie and OpenStorageTrie functions 
	                         // of the state.Database interface
	codeDb            dbm.DB // Mapping [codeHash] -> <code>
	addrPreimageDb    dbm.DB // Mapping [contract_address_hash] -> <contract_address>
	cdc               *amino.Codec // Amino codec to encode the values forthe lookupDb
}

func OurNewDatabase(stateDb, lookupDb, addrPreimageDb, codeDb dbm.DB) (*OurDatabase, error) {
	od := &OurDatabase{}
	od.stateStore = store.NewCommitMultiStore(stateDb)
	od.stateStore.MountStoreWithDB(AccountsKey, types.StoreTypeIAVL, nil)
	od.stateStore.MountStoreWithDB(StorageKey, types.StoreTypeIAVL, nil)
	if err := od.stateStore.LoadLatestVersion(); err != nil {
		return nil, err
	}
	od.lookupDb = lookupDb
	od.addrPreimageDb = addrPreimageDb
	od.codeDb = codeDb
	od.cdc = amino.NewCodec()
	return od, nil
}

func (od *OurDatabase) OpenTrie(root eth_common.Hash) (eth_state.Trie, error) {
	// Look up version id to use
	if root != (eth_common.Hash{}) {
		val := od.lookupDb.Get(root[:])
		if val == nil {
			return nil, fmt.Errorf("Could not find version with root hash %x", root[:])
		}
		var versionId int64
		_, err := od.cdc.UnmarshalBinaryReader(bytes.NewBuffer(val), &versionId, 0)
		if err != nil {
			return nil, err
		}
		od.stateStore.LoadVersion(versionId)
	}
	st := od.stateStore.GetCommitKVStore(AccountsKey)
	return &OurTrie{od: od, st: st, prefix: nil}, nil
}

func (od *OurDatabase) OpenStorageTrie(addrHash, root eth_common.Hash) (eth_state.Trie, error) {
	if root != (eth_common.Hash{}) {
		val := od.lookupDb.Get(root[:])
		if val == nil {
			return nil, fmt.Errorf("Could not find version with root hash %x", root[:])
		}
		var versionId int64
		_, err := od.cdc.UnmarshalBinaryReader(bytes.NewBuffer(val), &versionId, 0)
		if err != nil {
			return nil, err
		}
		od.stateStore.LoadVersion(versionId)     // This might not be required,
		                                        // we just need to check that accounts and storage are consistent
	}
	st := od.stateStore.GetCommitKVStore(StorageKey)
	return &OurTrie{od:od, st: st, prefix: addrHash[:]}, nil
}

func (od *OurDatabase) CopyTrie(eth_state.Trie) eth_state.Trie {
	return nil
}

func (od *OurDatabase) ContractCode(addrHash, codeHash eth_common.Hash) ([]byte, error) {
	code := od.codeDb.Get(codeHash[:])
	return code, nil
}

func (od *OurDatabase) ContractCodeSize(addrHash, codeHash eth_common.Hash) (int, error) {
	code := od.codeDb.Get(codeHash[:])
	return len(code), nil
}

func (od *OurDatabase) TrieDB() *eth_trie.Database {
	return nil
}

// Implementation of eth_state.Trie
type OurTrie struct {
	od *OurDatabase
	// This is essentially part of the KVStore for a specific prefix
	st store.CommitKVStore
	prefix []byte
}

func (ot *OurTrie) makePrefix(key []byte) []byte {
	kk := make([]byte, len(ot.prefix)+len(key))
	copy(kk, ot.prefix)
	copy(kk[len(ot.prefix):], key)
	return kk
}

func (ot *OurTrie) TryGet(key []byte) ([]byte, error) {
	if ot.prefix == nil {
		return ot.st.Get(key), nil
	}
	return ot.st.Get(ot.makePrefix(key)), nil
}

func (ot *OurTrie) TryUpdate(key, value []byte) error {
	if ot.prefix == nil {
		ot.st.Set(key, value)
		return nil
	}
	ot.st.Set(ot.makePrefix(key), value)
	return nil
}

func (ot *OurTrie) TryDelete(key []byte) error {
	if ot.prefix == nil {
		ot.st.Delete(key)
		return nil
	}
	ot.st.Delete(ot.makePrefix(key))
	return nil
}

func (ot *OurTrie) Commit(onleaf eth_trie.LeafCallback) (eth_common.Hash, error) {
	commitId := ot.st.Commit()
	var hash eth_common.Hash
	copy(hash[:], commitId.Hash)
	b, err := ot.od.cdc.MarshalBinary(commitId.Version)
	if err != nil {
		return hash, err
	}
	ot.od.lookupDb.Set(hash[:], b)
	return hash, nil
}

func (ot *OurTrie) Hash() eth_common.Hash {
	return eth_common.Hash{}
}

func (ot *OurTrie) NodeIterator(startKey []byte) eth_trie.NodeIterator {
	return nil
}

func (ot *OurTrie) GetKey([]byte) []byte {
	return nil
}

func (ot *OurTrie) Prove(key []byte, fromLevel uint, proofDb eth_ethdb.Putter) error {
	return nil
}

func main() {
	fmt.Printf("Instantiating state.Database\n")
	stateDb := dbm.NewDB("state" /* name */, dbm.MemDBBackend, "" /* dir */)
	lookupDb := dbm.NewDB("lookup" /* name */, dbm.MemDBBackend, "" /* dir */)
	addrPreimageDb := dbm.NewDB("addrPreimage" /* name */, dbm.MemDBBackend, "" /* dir */)
	codeDb := dbm.NewDB("code" /* name */, dbm.MemDBBackend, "" /* dir */)
	var d eth_state.Database
	var err error
	d, err = OurNewDatabase(stateDb, lookupDb, addrPreimageDb, codeDb)
	if err != nil {
		panic(err)
	}
	fmt.Printf("Instantiating state.StateDB\n")
	// With empty root hash, i.e. empty state
	statedb, err := eth_state.New(eth_common.Hash{}, d)
	if err != nil {
		panic(err)
	}
	// Try something
	b := statedb.GetBalance(eth_common.HexToAddress("0x829BD824B016326A401d083B33D092293333A830"))
	fmt.Printf("Balance: %s\n", b)
}