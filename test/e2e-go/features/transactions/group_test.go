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

package transactions

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/vincentbdb/go-algorand/data/transactions"
	"github.com/vincentbdb/go-algorand/test/framework/fixtures"
)

func TestGroupTransactions(t *testing.T) {
	t.Parallel()
	a := require.New(t)

	var fixture fixtures.RestClientFixture
	fixture.Setup(t, filepath.Join("nettemplates", "TwoNodes50EachFuture.json"))
	defer fixture.Shutdown()

	client := fixture.LibGoalClient
	accountList, err := fixture.GetWalletsSortedByBalance()
	a.NoError(err)
	account0 := accountList[0].Address
	wh, err := client.GetUnencryptedWalletHandle()
	a.NoError(err)

	account1, err := client.GenerateAddress(wh)
	a.NoError(err)

	account2, err := client.GenerateAddress(wh)
	a.NoError(err)

	// construct transactions for sending money to account1 and account2
	tx1, err := client.ConstructPayment(account0, account1, 0, 1000000, nil, "", [32]byte{}, 0, 0)
	a.NoError(err)

	tx2, err := client.ConstructPayment(account0, account2, 0, 2000000, nil, "", [32]byte{}, 0, 0)
	a.NoError(err)

	// group them
	gid, err := client.GroupID([]transactions.Transaction{tx1, tx2})
	a.NoError(err)

	tx1.Group = gid
	stx1, err := client.SignTransactionWithWallet(wh, nil, tx1)
	a.NoError(err)

	tx2.Group = gid
	stx2, err := client.SignTransactionWithWallet(wh, nil, tx2)
	a.NoError(err)

	// submitting the transactions individually should fail
	_, err = client.BroadcastTransaction(stx1)
	a.Error(err)

	_, err = client.BroadcastTransaction(stx2)
	a.Error(err)

	// wrong order should fail
	err = client.BroadcastTransactionGroup([]transactions.SignedTxn{stx2, stx1})
	a.Error(err)

	// correct order should succeed
	err = client.BroadcastTransactionGroup([]transactions.SignedTxn{stx1, stx2})
	a.NoError(err)

	// wait for the txids and check balance
	txids := make(map[string]string)
	txids[stx1.Txn.ID().String()] = account0
	txids[stx2.Txn.ID().String()] = account0

	_, curRound := fixture.GetBalanceAndRound(account0)
	confirmed := fixture.WaitForAllTxnsToConfirm(curRound+5, txids)
	a.True(confirmed, "txgroup")

	bal1, _ := fixture.GetBalanceAndRound(account1)
	bal2, _ := fixture.GetBalanceAndRound(account2)
	a.Equal(bal1, uint64(1000000))
	a.Equal(bal2, uint64(2000000))
}

func TestGroupTransactionsDifferentSizes(t *testing.T) {
	t.Parallel()
	a := require.New(t)

	var fixture fixtures.RestClientFixture
	fixture.Setup(t, filepath.Join("nettemplates", "TwoNodes50EachFuture.json"))
	defer fixture.Shutdown()

	client := fixture.LibGoalClient
	accountList, err := fixture.GetWalletsSortedByBalance()
	a.NoError(err)
	account0 := accountList[0].Address
	goodGroupSizes := []int{1, 2, 3, 16}
	badGroupSize := 17

	for _, gs := range goodGroupSizes {
		wh, err := client.GetUnencryptedWalletHandle()
		a.NoError(err)

		// Generate gs accounts
		var accts []string
		for i := 0; i < gs; i++ {
			acct, err := client.GenerateAddress(wh)
			a.NoError(err)
			accts = append(accts, acct)
		}

		// construct gx txns sending money from account0 to each account
		var txns []transactions.Transaction
		for i, acct := range accts {
			txn, err := client.ConstructPayment(account0, acct, 0, uint64((i+1)*1000000), nil, "", [32]byte{}, 0, 0)
			a.NoError(err)
			txns = append(txns, txn)
		}

		// compute gid
		gid, err := client.GroupID(txns)
		a.NoError(err)

		// fill in gid and sign and keep track of txids
		var stxns []transactions.SignedTxn
		txids := make(map[string]string)
		for _, txn := range txns {
			txn.Group = gid
			stxn, err := client.SignTransactionWithWallet(wh, nil, txn)
			a.NoError(err)
			stxns = append(stxns, stxn)
			txids[txn.ID().String()] = account0
		}

		// broadcasting group should succeed
		err = client.BroadcastTransactionGroup(stxns)
		a.NoError(err)

		// wait for the txids and check balances
		_, curRound := fixture.GetBalanceAndRound(account0)
		confirmed := fixture.WaitForAllTxnsToConfirm(curRound+5, txids)
		a.True(confirmed, "txgroup")

		for i, acct := range accts {
			bal, _ := fixture.GetBalanceAndRound(acct)
			a.Equal(bal, uint64((i+1)*1000000))
		}
	}

	// Now test a group that's too large
	{
		wh, err := client.GetUnencryptedWalletHandle()
		a.NoError(err)

		// Generate gs accounts
		var accts []string
		for i := 0; i < badGroupSize; i++ {
			acct, err := client.GenerateAddress(wh)
			a.NoError(err)
			accts = append(accts, acct)
		}

		// construct gx txns sending money from account0 to each account
		var txns []transactions.Transaction
		for i, acct := range accts {
			txn, err := client.ConstructPayment(account0, acct, 0, uint64((i+1)*1000000), nil, "", [32]byte{}, 0, 0)
			a.NoError(err)
			txns = append(txns, txn)
		}

		// compute gid
		gid, err := client.GroupID(txns)
		a.NoError(err)

		// fill in gid and sign and keep track of txids
		var stxns []transactions.SignedTxn
		txids := make(map[string]string)
		for _, txn := range txns {
			txn.Group = gid
			stxn, err := client.SignTransactionWithWallet(wh, nil, txn)
			a.NoError(err)
			stxns = append(stxns, stxn)
			txids[txn.ID().String()] = account0
		}

		// broadcasting group should now fail
		err = client.BroadcastTransactionGroup(stxns)
		a.Error(err)
	}
}
