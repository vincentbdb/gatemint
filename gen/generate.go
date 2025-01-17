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

package gen

import (
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"sort"

	"github.com/vincentbdb/go-algorand/config"
	"github.com/vincentbdb/go-algorand/data/account"
	"github.com/vincentbdb/go-algorand/data/basics"
	"github.com/vincentbdb/go-algorand/data/bookkeeping"
	"github.com/vincentbdb/go-algorand/protocol"
	"github.com/vincentbdb/go-algorand/util"
	"github.com/vincentbdb/go-algorand/util/db"
)

// Genesis.json SchemaID
var schemaID = "v1"

var defaultSinkAddr = basics.Address{0x7, 0xda, 0xcb, 0x4b, 0x6d, 0x9e, 0xd1, 0x41, 0xb1, 0x75, 0x76, 0xbd, 0x45, 0x9a, 0xe6, 0x42, 0x1d, 0x48, 0x6d, 0xa3, 0xd4, 0xef, 0x22, 0x47, 0xc4, 0x9, 0xa3, 0x96, 0xb8, 0x2e, 0xa2, 0x21}
var defaultPoolAddr = basics.Address{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}

// The number of MicroAlgos in the incentive pool at genesis.
var defaultIncentivePoolBalanceAtInception uint64 = 125e6 * 1e6

// TotalMoney represents the total amount of MicroAlgos in the system
const TotalMoney uint64 = 10 * 1e9 * 1e6

type genesisAllocation struct {
	Name   string
	Stake  uint64
	Online basics.Status
}

// GenerateGenesisFiles generates the genesis.json file and wallet files for a give genesis configuration.
func GenerateGenesisFiles(genesisData GenesisData, outDir string, verbose bool) error {
	err := os.Mkdir(outDir, os.ModeDir|os.FileMode(0777))
	if err != nil && os.IsNotExist(err) {
		return fmt.Errorf("couldn't make output directory '%s': %v", outDir, err.Error())
	}

	var sum uint64
	allocation := make([]genesisAllocation, len(genesisData.Wallets))

	for i, wallet := range genesisData.Wallets {
		acct := genesisAllocation{
			Name:   wallet.Name,
			Stake:  uint64(float64(TotalMoney/100)*wallet.Stake + .5),
			Online: basics.Online,
		}
		if !wallet.Online {
			acct.Online = basics.Offline
		}
		allocation[i] = acct
		sum += acct.Stake
	}

	if sum != TotalMoney {
		panic(fmt.Sprintf("Amounts don't add up to TotalMoney - off by %v", int64(TotalMoney)-int64(sum)))
	}

	// Backwards compatibility with older genesis files: if the consensus
	// protocol version is not specified, default to V0.
	proto := genesisData.ConsensusProtocol
	if proto == protocol.ConsensusVersion("") {
		proto = protocol.ConsensusCurrentVersion
	}

	// Backwards compatibility with older genesis files: if the fee sink
	// or the rewards pool is not specified, set their defaults.
	if (genesisData.FeeSink == basics.Address{}) {
		genesisData.FeeSink = defaultSinkAddr
	}
	if (genesisData.RewardsPool == basics.Address{}) {
		genesisData.RewardsPool = defaultPoolAddr
	}

	return generateGenesisFiles(outDir, proto, genesisData.NetworkName, genesisData.VersionModifier, allocation, genesisData.FirstPartKeyRound, genesisData.LastPartKeyRound, genesisData.PartKeyDilution, genesisData.FeeSink, genesisData.RewardsPool, genesisData.Comment, verbose)
}

func generateGenesisFiles(outDir string, proto protocol.ConsensusVersion, netName string, schemaVersionModifier string,
	allocation []genesisAllocation, firstWalletValid uint64, lastWalletValid uint64, partKeyDilution uint64, feeSink, rewardsPool basics.Address, comment string, verbose bool) (err error) {

	genesisAddrs := make(map[string]basics.Address)
	records := make(map[string]basics.AccountData)

	params, ok := config.Consensus[proto]
	if !ok {
		return fmt.Errorf("protocol %s not supported", proto)
	}
	if partKeyDilution == 0 {
		partKeyDilution = params.DefaultKeyDilution
	}

	// Sort account names alphabetically
	sort.SliceStable(allocation, func(i, j int) bool {
		return allocation[i].Name < allocation[j].Name
	})

	for _, wallet := range allocation {
		var root account.Root
		var part account.Participation

		wfilename := filepath.Join(outDir, config.RootKeyFilename(wallet.Name))
		pfilename := filepath.Join(outDir, config.PartKeyFilename(wallet.Name, firstWalletValid, lastWalletValid))

		root, rootDB, rootkeyErr := loadRootKey(wfilename)
		if rootkeyErr != nil && !os.IsNotExist(rootkeyErr) {
			return rootkeyErr
		}

		part, partDB, partkeyErr := loadPartKeys(pfilename)
		if partkeyErr != nil && !os.IsNotExist(partkeyErr) && partkeyErr != account.ErrUnsupportedSchema {
			return partkeyErr
		}

		if rootkeyErr == nil && partkeyErr == nil {
			if verbose {
				fmt.Println("Reusing existing wallet:", wfilename, pfilename)
			}
		} else {
			// At this point either rootKeys is valid or rootkeyErr != nil
			// Likewise, either partkey is valid or partkeyErr != nil
			if rootkeyErr != nil {
				os.Remove(wfilename)

				rootDB, err = db.MakeErasableAccessor(wfilename)
				if err != nil {
					err = fmt.Errorf("couldn't open root DB accessor %s: %v", wfilename, err)
				} else {
					root, err = account.GenerateRoot(rootDB)
				}
				if err != nil {
					os.Remove(wfilename)
					return
				}
				fmt.Printf("Created new rootkey: %s\n", wfilename)
			}

			if partkeyErr != nil && wallet.Online == basics.Online {
				os.Remove(pfilename)

				partDB, err = db.MakeErasableAccessor(pfilename)
				if err != nil {
					err = fmt.Errorf("couldn't open participation DB accessor %s: %v", pfilename, err)
					os.Remove(pfilename)
					return
				}

				part, err = account.FillDBWithParticipationKeys(partDB, root.Address(), basics.Round(firstWalletValid), basics.Round(lastWalletValid), partKeyDilution)
				if err != nil {
					err = fmt.Errorf("could not generate new participation file %s: %v", pfilename, err)
					os.Remove(pfilename)
					return
				}
				fmt.Printf("Created new partkey: %s\n", pfilename)
			}
		}

		var data basics.AccountData
		data.Status = wallet.Online
		data.MicroAlgos.Raw = wallet.Stake
		if wallet.Online == basics.Online {
			data.VoteID = part.VotingSecrets().OneTimeSignatureVerifier
			data.SelectionID = part.VRFSecrets().PK
			data.VoteFirstValid = part.FirstValid
			data.VoteLastValid = part.LastValid
			data.VoteKeyDilution = part.KeyDilution
		}

		records[wallet.Name] = data

		genesisAddrs[wallet.Name] = root.Address()

		rootDB.Close()
		if wallet.Online == basics.Online {
			partDB.Close()
		}
	}

	genesisAddrs["FeeSink"] = feeSink
	genesisAddrs["RewardsPool"] = rewardsPool

	if verbose {
		fmt.Println(proto, config.Consensus[proto].MinBalance)
	}

	records["FeeSink"] = basics.AccountData{
		Status:     basics.NotParticipating,
		MicroAlgos: basics.MicroAlgos{Raw: config.Consensus[proto].MinBalance},
	}
	records["RewardsPool"] = basics.AccountData{
		Status:     basics.NotParticipating,
		MicroAlgos: basics.MicroAlgos{Raw: defaultIncentivePoolBalanceAtInception},
	}

	sinkAcct := genesisAllocation{
		Name:   "FeeSink",
		Stake:  config.Consensus[proto].MinBalance,
		Online: basics.NotParticipating,
	}
	poolAcct := genesisAllocation{
		Name:   "RewardsPool",
		Stake:  defaultIncentivePoolBalanceAtInception,
		Online: basics.NotParticipating,
	}

	alloc2 := make([]genesisAllocation, 0, len(allocation)+2)
	alloc2 = append(alloc2, poolAcct, sinkAcct)
	alloc2 = append(alloc2, allocation...)
	allocation = alloc2

	g := bookkeeping.Genesis{
		SchemaID:    schemaID + schemaVersionModifier,
		Proto:       proto,
		Network:     protocol.NetworkID(netName),
		Timestamp:   0,
		FeeSink:     feeSink.String(),
		RewardsPool: rewardsPool.String(),
		Comment:     comment,
	}

	for _, wallet := range allocation {
		walletData := records[wallet.Name]

		g.Allocation = append(g.Allocation, bookkeeping.GenesisAllocation{
			Address: genesisAddrs[wallet.Name].String(),
			Comment: wallet.Name,
			State:   walletData,
		})
	}

	jsonData := protocol.EncodeJSON(g)
	err = ioutil.WriteFile(filepath.Join(outDir, config.GenesisJSONFile), append(jsonData, '\n'), 0666)
	return
}

// If err != nil, rootDB needs to be closed.
func loadRootKey(filename string) (root account.Root, rootDB db.Accessor, err error) {
	if !util.FileExists(filename) {
		err = os.ErrNotExist
		return
	}
	rootDB, err = db.MakeAccessor(filename, true, false)
	if err != nil {
		err = fmt.Errorf("couldn't load existing root file %s: %v", filename, err)
		return
	}

	root, err = account.RestoreRoot(rootDB)
	if err == nil {
		return
	}

	err = fmt.Errorf("could not restore existing root file %s: %v", filename, err)
	rootDB.Close()
	return
}

// If err != nil, partDB needs to be closed.
func loadPartKeys(filename string) (part account.Participation, partDB db.Accessor, err error) {
	if !util.FileExists(filename) {
		err = os.ErrNotExist
		return
	}
	partDB, err = db.MakeAccessor(filename, true, false)
	if err != nil {
		err = fmt.Errorf("couldn't load existing participation file %s: %v", filename, err)
		return
	}

	part, err = account.RestoreParticipation(partDB)
	if err == nil {
		return
	}

	// Don't override 'unsupported schema' error
	if err != account.ErrUnsupportedSchema {
		err = fmt.Errorf("couldn't restore existing participation file %s: %v", filename, err)
	}
	partDB.Close()
	return
}
