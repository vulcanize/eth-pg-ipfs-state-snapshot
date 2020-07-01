// Copyright © 2020 Vulcanize, Inc
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program. If not, see <http://www.gnu.org/licenses/>.

package snapshot

import (
	"bytes"
	"fmt"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/trie"

	"github.com/vulcanize/ipfs-blockchain-watcher/pkg/eth"
	"github.com/vulcanize/ipfs-blockchain-watcher/pkg/postgres"
)

var (
	nullHash          = common.HexToHash("0x0000000000000000000000000000000000000000000000000000000000000000")
	emptyNode, _      = rlp.EncodeToBytes([]byte{})
	emptyContractRoot = crypto.Keccak256Hash(emptyNode)
)

type Service struct {
	ethDB         ethdb.Database
	stateDB       state.Database
	ipfsPublisher *Publisher
}

func NewSnapshotService(con Config) (*Service, error) {
	pgdb, err := postgres.NewDB(con.DBConfig, con.Node)
	if err != nil {
		return nil, err
	}
	edb, err := rawdb.NewLevelDBDatabase(con.LevelDBPath, 256, 0, "")
	if err != nil {
		return nil, err
	}
	return &Service{
		ethDB:         edb,
		stateDB:       state.NewDatabase(edb),
		ipfsPublisher: NewPublisher(pgdb),
	}, nil
}

func (s *Service) CreateSnapshot(height uint64, hash common.Hash) error {
	// extract header from lvldb and publish to PG-IPFS
	// hold onto the headerID so that we can link the state nodes to this header
	header := rawdb.ReadHeader(s.ethDB, hash, height)
	headerID, err := s.ipfsPublisher.PublishHeader(header)
	if err != nil {
		return err
	}
	t, err := s.stateDB.OpenTrie(header.Root)
	if err != nil {
		return err
	}
	trieDB := s.stateDB.TrieDB()
	return s.createSnapshot(t.NodeIterator([]byte{}), trieDB, headerID)
}

func (s *Service) createSnapshot(it trie.NodeIterator, trieDB *trie.Database, headerID int64) error {
	for it.Next(true) {
		if it.Leaf() { // "leaf" nodes are actually "value" nodes, whose parents are the actual leaves
			continue
		}
		if bytes.Equal(nullHash.Bytes(), it.Hash().Bytes()) {
			continue
		}
		nodePath := make([]byte, len(it.Path()))
		copy(nodePath, it.Path())
		node, err := trieDB.Node(it.Hash())
		if err != nil {
			return err
		}
		var nodeElements []interface{}
		if err := rlp.DecodeBytes(node, &nodeElements); err != nil {
			return err
		}
		ty, err := CheckKeyType(nodeElements)
		if err != nil {
			return err
		}
		switch ty {
		case Leaf:
			var account state.Account
			if err := rlp.DecodeBytes(nodeElements[1].([]byte), &account); err != nil {
				return fmt.Errorf("error decoding account for leaf node at path %x nerror: %v", nodePath, err)
			}
			partialPath := trie.CompactToHex(nodeElements[0].([]byte))
			valueNodePath := append(nodePath, partialPath...)
			encodedPath := trie.HexToCompact(valueNodePath)
			leafKey := encodedPath[1:]
			// publish state node
			stateNode := eth.StateNodeModel{}
			if err := s.storageSnapshot(account.Root, stateID); err != nil {
				return fmt.Errorf("failed building eventual storage diffs for account %+v\r\nerror: %v", account, err)
			}
		case Extension, Branch:
			// publish state node
			stateNode := eth.StateNodeModel{}
		default:
			return fmt.Errorf("unexpected node type %s", ty)
		}
	}
}

// buildStorageNodesEventual builds the storage diff node objects for a created account
// i.e. it returns all the storage nodes at this state, since there is no previous state
func (s *Service) storageSnapshot(sr common.Hash, stateID int64) error {
	if bytes.Equal(sr.Bytes(), emptyContractRoot.Bytes()) {
		return nil
	}
	log.Debug("Storage Root For Eventual Diff", "root", sr.Hex())
	sTrie, err := s.stateDB.OpenTrie(sr)
	if err != nil {
		log.Info("error in build storage diff eventual", "error", err)
		return err
	}
	it := sTrie.NodeIterator(make([]byte, 0))
	for it.Next(true) {
		// skip value nodes
		if it.Leaf() {
			continue
		}
		if bytes.Equal(nullHash.Bytes(), it.Hash().Bytes()) {
			continue
		}
		nodePath := make([]byte, len(it.Path()))
		copy(nodePath, it.Path())
		node, err := s.stateDB.TrieDB().Node(it.Hash())
		if err != nil {
			return err
		}
		var nodeElements []interface{}
		if err := rlp.DecodeBytes(node, &nodeElements); err != nil {
			return err
		}
		ty, err := CheckKeyType(nodeElements)
		if err != nil {
			return err
		}
		switch ty {
		case Leaf:
			partialPath := trie.CompactToHex(nodeElements[0].([]byte))
			valueNodePath := append(nodePath, partialPath...)
			encodedPath := trie.HexToCompact(valueNodePath)
			leafKey := encodedPath[1:]
			storageNode := eth.StorageNodeModel{}

		case Extension, Branch:
			storageNode := eth.StorageNodeModel{}
		default:
			return fmt.Errorf("unexpected node type %s", ty)
		}
	}
	return nil
}