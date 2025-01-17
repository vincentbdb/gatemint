// Copyright (C) 2019 Algorand, Inc.
// This file is part of go-algorand
//
// go-algorand is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as
// published by the Free Software Foundation, either version 3 of the
// License, or (at your option) any later version.
//
// go-algorand is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with go-algorand.  If not, see <https://www.gnu.org/licenses/>.

package node

import (
	"fmt"
	"io/ioutil"
	"math/rand"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/vincentbdb/go-algorand/config"
	"github.com/vincentbdb/go-algorand/crypto"
	"github.com/vincentbdb/go-algorand/data"
	"github.com/vincentbdb/go-algorand/data/account"
	"github.com/vincentbdb/go-algorand/data/basics"
	"github.com/vincentbdb/go-algorand/data/bookkeeping"
	"github.com/vincentbdb/go-algorand/logging"
	"github.com/vincentbdb/go-algorand/protocol"
	"github.com/vincentbdb/go-algorand/util"
	"github.com/vincentbdb/go-algorand/util/db"
	"github.com/vincentbdb/go-algorand/util/execpool"
)

var expectedAgreementTime = 2*config.Protocol.BigLambda + 3*config.Protocol.SmallLambda + 2*time.Second

var sinkAddr = basics.Address{0x7, 0xda, 0xcb, 0x4b, 0x6d, 0x9e, 0xd1, 0x41, 0xb1, 0x75, 0x76, 0xbd, 0x45, 0x9a, 0xe6, 0x42, 0x1d, 0x48, 0x6d, 0xa3, 0xd4, 0xef, 0x22, 0x47, 0xc4, 0x9, 0xa3, 0x96, 0xb8, 0x2e, 0xa2, 0x21}
var poolAddr = basics.Address{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}

var defaultConfig = config.Local{
	Archival:                 false,
	GossipFanout:             4,
	NetAddress:               "",
	BaseLoggerDebugLevel:     1,
	IncomingConnectionsLimit: -1,
}

func setupFullNodes(t *testing.T, proto protocol.ConsensusVersion, verificationPool execpool.BacklogPool) ([]*AlgorandFullNode, []string, []string) {
	util.RaiseRlimit(1000)
	f, _ := os.Create(t.Name() + ".log")
	logging.Base().SetJSONFormatter()
	logging.Base().SetOutput(f)
	logging.Base().SetLevel(logging.Debug)

	numAccounts := 10
	minMoneyAtStart := 10000
	maxMoneyAtStart := 100000

	firstRound := basics.Round(0)
	lastRound := basics.Round(200)

	genesis := make(map[basics.Address]basics.AccountData)
	gen := rand.New(rand.NewSource(2))
	neighbors := make([]string, numAccounts)
	for i := range neighbors {
		neighbors[i] = "127.0.0.1:" + strconv.Itoa(10000+i)
	}

	wallets := make([]string, numAccounts)
	nodes := make([]*AlgorandFullNode, numAccounts)
	rootDirs := make([]string, 0)

	// The genesis configuration is missing allocations, but that's OK
	// because we explicitly generated the sqlite database above (in
	// installFullNode).
	g := bookkeeping.Genesis{
		SchemaID: "go-test-node-genesis",
		Proto:    proto,
		Network:  config.Devtestnet,
	}

	for i := range wallets {
		rootDirectory, err := ioutil.TempDir("", "testdir"+t.Name()+strconv.Itoa(i))
		rootDirs = append(rootDirs, rootDirectory)
		require.NoError(t, err)

		defaultConfig.NetAddress = "127.0.0.1:0"
		defaultConfig.SaveToDisk(rootDirectory)

		// Save empty phonebook - we'll add peers after they've been assigned listening ports
		err = config.SavePhonebookToDisk(make([]string, 0), rootDirectory)
		require.NoError(t, err)

		genesisDir := filepath.Join(rootDirectory, g.ID())
		os.Mkdir(genesisDir, 0700)

		wname := config.RootKeyFilename(t.Name() + "wallet" + strconv.Itoa(i))
		pname := config.PartKeyFilename(t.Name()+"wallet"+strconv.Itoa(i), uint64(firstRound), uint64(lastRound))

		wallets[i] = wname

		filename := filepath.Join(genesisDir, wname)
		access, err := db.MakeAccessor(filename, false, false)
		if err != nil {
			panic(err)
		}
		root, err := account.GenerateRoot(access)
		if err != nil {
			panic(err)
		}

		filename = filepath.Join(genesisDir, pname)
		access, err = db.MakeAccessor(filename, false, false)
		if err != nil {
			panic(err)
		}
		part, err := account.FillDBWithParticipationKeys(access, root.Address(), firstRound, lastRound, config.Consensus[protocol.ConsensusCurrentVersion].DefaultKeyDilution)
		if err != nil {
			panic(err)
		}

		data := basics.AccountData{
			Status:      basics.Online,
			MicroAlgos:  basics.MicroAlgos{Raw: uint64(minMoneyAtStart + (gen.Int() % (maxMoneyAtStart - minMoneyAtStart)))},
			SelectionID: part.VRFSecrets().PK,
			VoteID:      part.VotingSecrets().OneTimeSignatureVerifier,
		}
		short := root.Address()
		genesis[short] = data
	}

	bootstrap := data.MakeGenesisBalances(genesis, sinkAddr, poolAddr)

	for i, rootDirectory := range rootDirs {
		genesisDir := filepath.Join(rootDirectory, g.ID())
		ledgerFilenamePrefix := filepath.Join(genesisDir, config.LedgerFilenamePrefix)
		nodeID := fmt.Sprintf("Node%d", i)
		const inMem = false
		const archival = true
		_, err := data.LoadLedger(logging.Base().With("name", nodeID), ledgerFilenamePrefix, inMem, g.Proto, bootstrap, "", crypto.Digest{}, nil, archival)
		require.NoError(t, err)
	}

	for i := range nodes {
		rootDirectory := rootDirs[i]
		cfg, err := config.LoadConfigFromDisk(rootDirectory)
		require.NoError(t, err)

		node, err := MakeFull(logging.Base().With("source", t.Name()+strconv.Itoa(i)), rootDirectory, cfg, "", g)
		nodes[i] = node
		require.NoError(t, err)
	}

	return nodes, wallets, rootDirs
}

func TestSyncingFullNode(t *testing.T) {
	t.Skip("This is failing randomly again - PLEASE FIX!")

	backlogPool := execpool.MakeBacklog(nil, 0, execpool.LowPriority, nil)
	defer backlogPool.Shutdown()

	nodes, wallets, rootDirs := setupFullNodes(t, protocol.ConsensusCurrentVersion, backlogPool)
	for i := 0; i < len(nodes); i++ {
		defer os.Remove(wallets[i])
		defer os.RemoveAll(rootDirs[i])
		defer nodes[i].Stop()
	}

	initialRound := nodes[0].ledger.NextRound()

	startAndConnectNodes(nodes, true)

	counter := 0
	for tests := uint64(0); tests < 16; tests++ {
		timer := time.NewTimer(30*time.Second + 2*expectedAgreementTime)
		for i := range wallets {
			select {
			case <-nodes[i].ledger.Wait(initialRound + basics.Round(tests)):
				if i == 0 {
					counter++
					if counter == 5 {
						go func() {
							// after 5 blocks, have this node partitioned
							nodes[0].net.DisconnectPeers()
							time.Sleep(20 * time.Second)
							nodes[0].net.RequestConnectOutgoing(false, nil)
						}()
					}
				}
			case <-timer.C:
				require.Fail(t, fmt.Sprintf("no block notification for account: %d - %v. Iteration: %v", i, wallets[i], tests))
				return
			}
		}
	}

	roundsCompleted := nodes[0].ledger.LastRound()
	for i := basics.Round(0); i < roundsCompleted; i++ {
		for wallet := range wallets {
			e0, err := nodes[0].ledger.Block(i)
			if err != nil {
				panic(err)
			}
			ei, err := nodes[wallet].ledger.Block(i)
			if err != nil {
				panic(err)
			}
			require.Equal(t, e0.Hash(), ei.Hash())
		}
	}
}

func TestInitialSync(t *testing.T) {
	t.Skip("flaky TestInitialSync ")

	backlogPool := execpool.MakeBacklog(nil, 0, execpool.LowPriority, nil)
	defer backlogPool.Shutdown()

	nodes, wallets, rootdirs := setupFullNodes(t, protocol.ConsensusCurrentVersion, backlogPool)
	for i := 0; i < len(nodes); i++ {
		defer os.Remove(wallets[i])
		defer os.RemoveAll(rootdirs[i])
		defer nodes[i].Stop()
	}
	initialRound := nodes[0].ledger.NextRound()

	startAndConnectNodes(nodes, true)

	select {
	case <-nodes[0].ledger.Wait(initialRound):
		e0, err := nodes[0].ledger.Block(initialRound)
		if err != nil {
			panic(err)
		}
		e1, err := nodes[1].ledger.Block(initialRound)
		if err != nil {
			panic(err)
		}
		require.Equal(t, e1.Hash(), e0.Hash())
	case <-time.After(60 * time.Second):
		require.Fail(t, fmt.Sprintf("no block notification for wallet: %v.", wallets[0]))
		return
	}
}

func TestSimpleUpgrade(t *testing.T) {
	t.Skip("Randomly failing: node_test.go:~283 : no block notification for account. Re-enable after agreement bug-fix pass")

	backlogPool := execpool.MakeBacklog(nil, 0, execpool.LowPriority, nil)
	defer backlogPool.Shutdown()

	nodes, wallets, rootDirs := setupFullNodes(t, protocol.ConsensusTest0, backlogPool)
	for i := 0; i < len(nodes); i++ {
		defer os.Remove(wallets[i])
		defer os.RemoveAll(rootDirs[i])
		defer nodes[i].Stop()
	}

	initialRound := nodes[0].ledger.NextRound()

	startAndConnectNodes(nodes, false)

	maxRounds := basics.Round(16)
	roundsCheckedForUpgrade := 0

	for tests := basics.Round(0); tests < maxRounds; tests++ {
		blocks := make([]bookkeeping.Block, len(wallets), len(wallets))
		for i := range wallets {
			select {
			case <-nodes[i].ledger.Wait(initialRound + tests):
				blk, err := nodes[i].ledger.Block(initialRound + tests)
				if err != nil {
					panic(err)
				}
				blocks[i] = blk
			case <-time.After(60 * time.Second):
				require.Fail(t, fmt.Sprintf("no block notification for account: %v. Iteration: %v", wallets[i], tests))
				return
			}
		}

		blockDigest := blocks[0].Hash()

		for i := range wallets {
			require.Equal(t, blockDigest, blocks[i].Hash())
		}

		// On the first round, check that we did not upgrade
		if tests == 0 {
			roundsCheckedForUpgrade++

			for i := range wallets {
				require.Equal(t, protocol.ConsensusTest0, blocks[i].CurrentProtocol)
			}
		}

		// On the last round, check that we upgraded
		if tests == maxRounds-1 {
			roundsCheckedForUpgrade++

			for i := range wallets {
				require.Equal(t, protocol.ConsensusTest1, blocks[i].CurrentProtocol)
			}
		}
	}

	require.Equal(t, 2, roundsCheckedForUpgrade)
}

func startAndConnectNodes(nodes []*AlgorandFullNode, delayStartFirstNode bool) {
	var wg sync.WaitGroup
	for i := range nodes {
		if delayStartFirstNode && i == 0 {
			continue
		}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			nodes[i].Start()
		}(i)
	}
	wg.Wait()

	if delayStartFirstNode {
		connectPeers(nodes[1:])
		delayStartNode(nodes[0], nodes[1:], 20*time.Second)
	} else {
		connectPeers(nodes)
	}
}

func connectPeers(nodes []*AlgorandFullNode) {
	neighbors := make([]string, 0)
	for _, node := range nodes {
		neighbors = append(neighbors, node.config.NetAddress)
	}

	for _, node := range nodes {
		node.ExtendPeerList(neighbors...)
		node.net.RequestConnectOutgoing(false, nil)
	}
}

func delayStartNode(node *AlgorandFullNode, peers []*AlgorandFullNode, delay time.Duration) {
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		time.Sleep(delay)
		node.Start()
	}()
	wg.Wait()

	node0Addr := node.config.NetAddress
	for _, peer := range peers {
		peer.ExtendPeerList(node0Addr)
		peer.net.RequestConnectOutgoing(false, nil)
	}
}

func TestStatusReport_TimeSinceLastRound(t *testing.T) {
	type fields struct {
		LastRoundTimestamp time.Time
	}

	tests := []struct {
		name      string
		fields    fields
		want      time.Duration
		wantError bool
	}{
		// test cases
		{
			name: "test1",
			fields: fields{
				LastRoundTimestamp: time.Time{},
			},
			want:      time.Duration(0),
			wantError: false,
		},
		{
			name: "test2",
			fields: fields{
				LastRoundTimestamp: time.Now(),
			},
			want:      time.Duration(0),
			wantError: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status := StatusReport{
				LastRoundTimestamp: tt.fields.LastRoundTimestamp,
			}
			if got := status.TimeSinceLastRound(); got != tt.want {
				if !tt.wantError {
					t.Errorf("StatusReport.TimeSinceLastRound() = %v, want = %v", got, tt.want)
				}
			} else if tt.wantError {
				t.Errorf("StatusReport.TimeSinceLastRound() = %v, want != %v", got, tt.want)
			}
		})
	}
}
