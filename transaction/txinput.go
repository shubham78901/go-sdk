package transaction

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"fmt"

	"github.com/bitcoin-sv/go-sdk/crypto"
	"github.com/bitcoin-sv/go-sdk/script"
	"github.com/bitcoin-sv/go-sdk/sighash"
	"github.com/bitcoin-sv/go-sdk/util"
	"github.com/pkg/errors"
)

// UTXOGetterFunc is used for tx.Fund(...). It provides the amount of satoshis required
// for funding as `deficit`, and expects []*bt.UTXO to be returned containing
// utxos of which *bt.Input's can be built.
// If the returned []*bt.UTXO does not cover the deficit after fee recalculation, then
// this UTXOGetterFunc is called again, with the newly calculated deficit passed in.
//
// It is expected that bt.ErrNoUTXO will be returned once the utxo source is depleted.
type UTXOGetterFunc func(ctx context.Context, deficit uint64) ([]*UTXO, error)

// TotalInputSatoshis returns the total Satoshis inputted to the transaction.
func (tx *Transaction) TotalInputSatoshis() (total uint64) {
	for _, in := range tx.Inputs {
		total += in.PreviousTxSatoshis
	}
	return
}

func (tx *Transaction) addInput(input *Input) {
	tx.Inputs = append(tx.Inputs, input)
}

// AddP2PKHInputsFromTx will add all Outputs of given previous transaction
// that match a specific public key to your transaction.
func (tx *Transaction) AddP2PKHInputsFromTx(pvsTx *Transaction, matchPK []byte) error {
	// Given that the prevTxID never changes, calculate it once up front.
	prevTxIDBytes := pvsTx.TxIDBytes()
	for i, utxo := range pvsTx.Outputs {
		utxoPkHASH160, err := utxo.LockingScript.PublicKeyHash()
		if err != nil {
			return err
		}

		if bytes.Equal(utxoPkHASH160, crypto.Hash160(matchPK)) {
			if err := tx.FromUTXOs(&UTXO{
				TxID:          prevTxIDBytes,
				Vout:          uint32(i),
				Satoshis:      utxo.Satoshis,
				LockingScript: utxo.LockingScript,
			}); err != nil {
				return err
			}
		}
	}

	return nil
}

// From adds a new input to the transaction from the specified UTXO fields, using the default
// finalised sequence number (0xFFFFFFFF). If you want a different nSeq, change it manually
// afterwards.
func (tx *Transaction) From(prevTxID string, vout uint32, prevTxLockingScript string, satoshis uint64) error {
	pts, err := script.NewFromHexString(prevTxLockingScript)
	if err != nil {
		return err
	}
	pti, err := hex.DecodeString(prevTxID)
	if err != nil {
		return err
	}

	return tx.FromUTXOs(&UTXO{
		TxID:          pti,
		Vout:          vout,
		LockingScript: pts,
		Satoshis:      satoshis,
	})
}

// FromUTXOs adds a new input to the transaction from the specified *bt.UTXO fields, using the default
// finalised sequence number (0xFFFFFFFF). If you want a different nSeq, change it manually
// afterwards.
func (tx *Transaction) FromUTXOs(utxos ...*UTXO) error {
	for _, utxo := range utxos {
		i := &Input{
			PreviousTxOutIndex: utxo.Vout,
			PreviousTxSatoshis: utxo.Satoshis,
			PreviousTxScript:   utxo.LockingScript,
			SequenceNumber:     DefaultSequenceNumber, // use default finalised sequence number
		}
		if err := i.PreviousTxIDAdd(utxo.TxID); err != nil {
			return err
		}

		tx.addInput(i)
	}

	return nil
}

// Fund continuously calls the provided bt.UTXOGetterFunc, adding each returned input
// as an input via tx.From(...), until it is estimated that inputs cover the outputs + fees.
//
// After completion, the receiver is ready for `Change(...)` to be called, and then be signed.
// Note, this function works under the assumption that receiver *bt.Tx already has all the outputs
// which need covered.
//
// If insufficient utxos are provided from the UTXOGetterFunc, a bt.ErrInsufficientFunds is returned.
//
// Example usage:
//
//	if err := tx.Fund(ctx, bt.NewFeeQuote(), func(ctx context.Context, deficit satoshis) ([]*bt.UTXO, error) {
//	    utxos := make([]*bt.UTXO, 0)
//	    for _, f := range funds {
//	        deficit -= satoshis
//	        utxos := append(utxos, &bt.UTXO{
//	            TxID: f.TxID,
//	            Vout: f.Vout,
//	            LockingScript: f.Script,
//	            Satoshis: f.Satoshis,
//	        })
//	        if deficit == 0 {
//	            return utxos, nil
//	        }
//	    }
//	    return nil, bt.ErrNoUTXO
//	}); err != nil {
//	    if errors.Is(err, bt.ErrInsufficientFunds) { /* handle */ }
//	    return err
//	}
func (tx *Transaction) Fund(ctx context.Context, fq *FeeQuote, next UTXOGetterFunc) error {
	deficit, err := tx.estimateDeficit(fq)
	if err != nil {
		return err
	}
	for deficit != 0 {
		utxos, err := next(ctx, deficit)
		if err != nil {
			if errors.Is(err, ErrNoUTXO) {
				break
			}

			return err
		}

		if err = tx.FromUTXOs(utxos...); err != nil {
			return err
		}

		deficit, err = tx.estimateDeficit(fq)
		if err != nil {
			return err
		}
	}
	if deficit != 0 {
		return ErrInsufficientFunds
	}

	return nil
}

// InputCount returns the number of transaction Inputs.
func (tx *Transaction) InputCount() int {
	return len(tx.Inputs)
}

// PreviousOutHash returns a byte slice of inputs outpoints, for creating a signature hash
func (tx *Transaction) PreviousOutHash() []byte {
	buf := make([]byte, 0)

	for _, in := range tx.Inputs {
		buf = append(buf, util.ReverseBytes(in.PreviousTxID())...)
		oi := make([]byte, 4)
		binary.LittleEndian.PutUint32(oi, in.PreviousTxOutIndex)
		buf = append(buf, oi...)
	}

	return crypto.Sha256d(buf)
}

// SequenceHash returns a byte slice of inputs SequenceNumber, for creating a signature hash
func (tx *Transaction) SequenceHash() []byte {
	buf := make([]byte, 0)

	for _, in := range tx.Inputs {
		oi := make([]byte, 4)
		binary.LittleEndian.PutUint32(oi, in.SequenceNumber)
		buf = append(buf, oi...)
	}

	return crypto.Sha256d(buf)
}

// InsertInputUnlockingScript applies a script to the transaction at a specific index in
// unlocking script field.
func (tx *Transaction) InsertInputUnlockingScript(index uint32, s *script.Script) error {
	if tx.Inputs[index] != nil {
		tx.Inputs[index].UnlockingScript = s
		return nil
	}

	return fmt.Errorf("no input at index %d", index)
}

// FillInput is used to unlock the transaction at a specific input index.
// It takes an Unlocker interface as a parameter so that different
// unlocking implementations can be used to unlock the transaction -
// for example local or external unlocking (hardware wallet), or
// signature / non-signature based.
func (tx *Transaction) FillInput(ctx context.Context, unlocker Unlocker, params UnlockerParams) error {
	if unlocker == nil {
		return ErrNoUnlocker
	}

	if params.SigHashFlags == 0 {
		params.SigHashFlags = sighash.AllForkID
	}

	unlockingScript, err := unlocker.UnlockingScript(ctx, tx, params)
	if err != nil {
		return err
	}

	return tx.InsertInputUnlockingScript(params.InputIdx, unlockingScript)
}

// FillAllInputs is used to sign all inputs. It takes an UnlockerGetter interface
// as a parameter so that different unlocking implementations can
// be used to sign the transaction - for example local/external
// signing, or P2PKH/contract signing.
//
// Given this signs inputs and outputs, sighash `ALL|FORKID` is used.
func (tx *Transaction) FillAllInputs(ctx context.Context, ug UnlockerGetter) error {
	for i, in := range tx.Inputs {
		u, err := ug.Unlocker(ctx, in.PreviousTxScript)
		if err != nil {
			return err
		}

		if err = tx.FillInput(ctx, u, UnlockerParams{
			InputIdx:     uint32(i),
			SigHashFlags: sighash.AllForkID, // use SIGHASHALLFORFORKID to sign automatically
		}); err != nil {
			return err
		}
	}

	return nil
}
