package wallet

import (
	"testing"

	"github.com/NebulousLabs/Sia/consensus"
)

// testFundTransaction funds and completes a transaction using the
// build-your-own transaction functions, checking that a no-refund transaction
// is created that is valid.
func (wt *walletTester) testFundTransaction() {
	// Get a coin address for the wallet and fund the wallet using money from
	// the
	_, unlockConds, err := wt.CoinAddress()
	if err != nil {
		wt.Tester.Fatal(err)
	}
	siacoinOutput, value := wt.FindSpendableSiacoinInput()
	walletFunderTxn := wt.AddSiacoinInputToTransaction(consensus.Transaction{}, siacoinOutput)
	walletFunderOutput := consensus.SiacoinOutput{
		Value:      value,
		UnlockHash: unlockConds.UnlockHash(),
	}
	walletFunderTxn.SiacoinOutputs = append(walletFunderTxn.SiacoinOutputs, walletFunderOutput)
	block, err := wt.MineCurrentBlock([]consensus.Transaction{walletFunderTxn})
	if err != nil {
		wt.Tester.Fatal(err)
	}
	err = wt.State.AcceptBlock(block)
	if err != nil {
		wt.Tester.Fatal(err)
	}
	wt.update()

	// Build a transaction that intentionally needs a refund.
	id, err := wt.RegisterTransaction(consensus.Transaction{})
	if err != nil {
		wt.Tester.Fatal(err)
	}
	err = wt.FundTransaction(id, value.Sub(consensus.NewCurrency64(1)))
	if err != nil {
		wt.Tester.Fatal(err)
	}
	err = wt.AddMinerFee(id, value.Sub(consensus.NewCurrency64(1)))
	if err != nil {
		wt.Tester.Fatal(err)
	}
	t, err := wt.SignTransaction(id, true)
	if err != nil {
		wt.Tester.Fatal(err)
	}
	err = wt.tpool.AcceptTransaction(t)
	if err != nil {
		wt.Tester.Fatal(err)
	}
	wt.update()

	// Check that the length of the created transaction is 1 siacoin, and that
	// the unconfirmed balance of the wallet is 1.
	if len(t.SiacoinOutputs) != 0 {
		wt.Tester.Error("more than zero siacoin outputs created in custom transaction")
	}
	if wt.Balance(true).Cmp(consensus.NewCurrency64(1)) != 0 {
		wt.Tester.Error(wt.Balance(true).MarshalJSON())
		wt.Tester.Error("wallet balance not reporting at one?")
	}

	// Dump the transaction pool into a block and see that the balance still
	// registers correctly.
	txns, err := wt.tpool.TransactionSet()
	if err != nil {
		wt.Tester.Error(err)
	}
	block, err = wt.MineCurrentBlock(txns)
	if err != nil {
		wt.Tester.Error(err)
	}
	err = wt.State.AcceptBlock(block)
	if err != nil {
		wt.Tester.Error(err)
	}

	// Check that the length of the created transaction is 1 siacoin, and that
	// the unconfirmed balance of the wallet is 1.
	if len(t.SiacoinOutputs) != 0 {
		wt.Tester.Error("more than zero siacoin outputs created in custom transaction")
	}
	if wt.Balance(true).Cmp(consensus.NewCurrency64(1)) != 0 {
		wt.Tester.Error(wt.Balance(true).MarshalJSON())
		wt.Tester.Error("wallet balance not reporting at one?")
	}
}

// TestFundTransaction creates a wallet tester and uses it to call
// testFundTransaction.
func TestFundTransaction(t *testing.T) {
	wt := newWalletTester(t)
	wt.testFundTransaction()
}
