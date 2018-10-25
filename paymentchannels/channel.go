package paymentchannels

import (
	"crypto/rand"
	"github.com/gcash/bchd/bchec"
	"github.com/gcash/bchd/chaincfg"
	"github.com/gcash/bchd/chaincfg/chainhash"
	"github.com/gcash/bchd/txscript"
	"github.com/gcash/bchd/wire"
	"github.com/gcash/bchutil"
	"github.com/gcash/bchwallet/waddrmgr"
	"github.com/gcash/bchwallet/wallet/txsizes"
	"github.com/go-errors/errors"
	"github.com/libp2p/go-libp2p-crypto"
	"github.com/libp2p/go-libp2p-peer"
)

// TODO: these should be config options
var (
	DefaultChannelDelay       = 6 * 24
	MaxAcceptibleChannelDelay = 6 * 24 * 7

	DefaultFeePerByte       = bchutil.Amount(5)
	MinAcceptibleFeePerByte = bchutil.Amount(1)

	DefaultDustLimit       = bchutil.Amount(1000)
	MaxAcceptibleDustLimit = bchutil.Amount(1000)
)

type ChannelState uint8

const (
	// ChannelStateOpening is the initial state of the channel until we connect
	// and exchange commitment transactions.
	ChannelStateOpening ChannelState = 0

	// ChannelStateOpen is the normal running state for a channel.
	ChannelStateOpen ChannelState = 1

	// ChannelStatePendingClosure is set when either party broadcasts a commitment
	// transaction. While the transaction is still unconfirmed it will be in this
	// state.
	ChannelStatePendingClosure ChannelState = 2

	// ChannelStateClosed represents a closed channel. This includes channels
	// closed by broadcasting a commitment transaction.
	ChannelStateClosed ChannelState = 3

	// ChannelStateError means we messed up somehow.
	ChannelStateError ChannelState = 4
)

// String is the a stringer for ChannelState
func (s ChannelState) String() string {
	switch s {
	case ChannelStateOpening:
		return "Opening"
	case ChannelStateOpen:
		return "Open"
	case ChannelStatePendingClosure:
		return "Pending Closure"
	case ChannelStateClosed:
		return "Closed"
	case ChannelStateError:
		return "Error"
	default:
		return "Unknown"
	}
}

// Channel holds all the data relevant to a payment channel
type Channel struct {
	// ChannelID is the ID of the channel
	// TODO: how do we want to create this? We would have the sender set it to a random ID,
	// but then the recipient needs to reject channel open requests if he has the same ID in the
	// db. Alternatively we could set it to the funding txid, but we don't know it initially.
	ChannelID chainhash.Hash

	// State allows us to quickly tell what state the channel is in.
	State ChannelState

	// Incoming specifies whether the channel was opened by us or them
	Incoming bool

	// AddressID is taken from the cashaddr. It can be used by software to map channels
	// to external actions (like an order on a website).
	AddressID []byte

	// RemotePeerID is their libp2p peerID which we will use for communications.
	RemotePeerID peer.ID

	// RemotePubkey is the other party's public key which will be used in two spots:
	// 1. As part of the 2 of 2 multisig P2SH address that holds the channel funds and,
	// 2. As part of the 2 of 2 multisig P2SH address on the breach remedy output of our
	// commitment transactions.
	RemotePubkey bchec.PublicKey

	// LocalPrivkey is used the same way as RemotePubkey except it's our key and we give the
	// corresponding pubkey to the other party.
	LocalPrivkey bchec.PrivateKey

	// RemoteRevocationPrivkeys is a store of private keys given to us by the other party.
	// Every time we update the channel state both parties not only sign new commitment
	// transactions, but they exchange their revocation private keys which the other party
	// can use to claim the funds if an old commitment transaction gets broadcasted.
	// We use a map so we can quickly grab the correct key.
	RemoteRevocationPrivkeys map[bchutil.Address]bchec.PrivateKey

	// RemoteRevocationPubkey represents the revocation public key used for our most
	// recent commitment transaction. The
	RemoteRevocationPubkey bchec.PublicKey

	// LocalRevocationPrivkey is our revocation key that we will share with the other party
	// after each transaction. This will be updated with a new key after the old key is
	// disclosed.
	LocalRevocationPrivkey bchec.PrivateKey

	// Delay is the negotiated timeout on commitment transactions. If a channel is
	// unilaterally closed the party which closed the channel will need to wait the delay.
	// This is represneted in blocks as will be used by CheckSequenceVerify
	Delay uint32

	// FeePerByte is the fee rate used when calculating the fee on the payout transaction.
	// The fee should be subtracted evenly from the payout amount of both parties.
	FeePerByte bchutil.Amount

	// DustLimit is the minimum value of a TxOut used for a payout transaction. If the value
	// of an output is less than the dust limit then the output should be omitted from the
	// payout transaction.
	DustLimit bchutil.Amount

	// RemotePayoutScript is is the destination where the other party wants the funds to go.
	RemotePayoutScript []byte

	// LocalPayoutScript is the same as RemotePayoutScript except it represents our payout script.
	LocalPayoutScript []byte

	// RemoteBalance is the balance of the remote peer. This will be paid out when the channel is closed.
	RemoteBalance bchutil.Amount

	// LocalBalance is our channel balance which will be paid to us when the channel is closed.
	LocalBalance bchutil.Amount

	// CommitmentTx is out current commitment transaction. This should have the other
	// party's signature on their input. We just need to sign and broadcast if we
	// want to force close.
	CommitmentTx wire.MsgTx

	// FundingTxid is the transaction ID of the transaction which funded the channel.
	FundingTxid chainhash.Hash

	// FundingOutpoint is the outpoint that paid the funding transaction and which will
	// serve as the input for the payout transaction
	FundingOutpoint wire.OutPoint

	// PayoutTxid is the transaction ID of the transaction which closed out the channel.
	PayoutTxid chainhash.Hash

	// TransactionCount is the total number of transactions (not including the initial funding)
	// that have been processed while the channel is open.
	TransactionCount uint64

	// ChannelAddress is the address that holds the channel funds.
	ChannelAddress string

	// RedeemScript is the payout redeem script for the ChannelAddress.
	RedeemScript []byte
}

// initNewOutgoingChannel will create a new channel that has been initialized with all the
// values that are known at channel opening. This includes generating new keys to be used
// in the multisig and breach remedy outputs.
func (n *PaymentChannelNode) initNewOutgoingChannel(addr *bchutil.AddressPaymentChannel, amount bchutil.Amount) (*Channel, error) {
	// Extract the peerID from the bitcoin address
	pub, err := crypto.UnmarshalEd25519PublicKey(addr.PeerID[:])
	if err != nil {
		return nil, err
	}
	peerID, err := peer.IDFromPublicKey(pub)
	if err != nil {
		return nil, err
	}

	// Make a new random channel ID
	cidBytes := make([]byte, 32)
	rand.Read(cidBytes)
	channelID, err := chainhash.NewHash(cidBytes)
	if err != nil {
		return nil, err
	}

	// Generate our private keys
	channelPrivateKey, err := bchec.NewPrivateKey(bchec.S256())
	if err != nil {
		return nil, err
	}
	firstRevocationKey, err := bchec.NewPrivateKey(bchec.S256())
	if err != nil {
		return nil, err
	}

	// Fetch a new payout address from our wallet
	payoutAddr, err := n.Wallet.NewAddress(0, waddrmgr.KeyScopeBIP0044)
	if err != nil {
		return nil, err
	}
	script, err := txscript.PayToAddrScript(payoutAddr)
	if err != nil {
		return nil, err
	}

	channel := &Channel{
		ChannelID:              *channelID,
		AddressID:              addr.AddressID[:],
		RemotePeerID:           peerID,
		Delay:                  uint32(DefaultChannelDelay),
		FeePerByte:             DefaultFeePerByte,
		DustLimit:              DefaultDustLimit,
		Incoming:               false,
		State:                  ChannelStateOpening,
		LocalPrivkey:           *channelPrivateKey,
		LocalRevocationPrivkey: *firstRevocationKey,
		LocalPayoutScript:      script,
		LocalBalance:           amount,
	}
	return channel, err
}

// initNewIncomingChannel will create a new channel that has been initialized with all the
// values that are known at channel opening. This includes generating new keys to be used
// in the multisig and breach remedy outputs.
func (n *PaymentChannelNode) initNewIncomingChannel(channelID *chainhash.Hash, addressID []byte,
	remoteChannelPubkey *bchec.PublicKey, remoteRevocationPubkey *bchec.PublicKey, dustLimt bchutil.Amount,
	feePerByte bchutil.Amount, delay uint32, remotePayoutScript []byte, remotePeerID peer.ID) (*Channel, error) {

	// Generate our private keys
	channelPrivateKey, err := bchec.NewPrivateKey(bchec.S256())
	if err != nil {
		return nil, err
	}
	firstRevocationKey, err := bchec.NewPrivateKey(bchec.S256())
	if err != nil {
		return nil, err
	}

	// Fetch a new payout address from our wallet
	payoutAddr, err := n.Wallet.NewAddress(0, waddrmgr.KeyScopeBIP0044)
	if err != nil {
		return nil, err
	}
	script, err := txscript.PayToAddrScript(payoutAddr)
	if err != nil {
		return nil, err
	}

	// The channel opener's public key always goes first
	channelAddr, redeemScript, err := buildP2SHAddress(remoteChannelPubkey, channelPrivateKey.PubKey(), n.Params)
	if err != nil {
		return nil, err
	}

	channel := &Channel{
		ChannelID:              *channelID,
		AddressID:              addressID,
		RemotePeerID:           remotePeerID,
		Delay:                  delay,
		FeePerByte:             feePerByte,
		DustLimit:              dustLimt,
		Incoming:               true,
		State:                  ChannelStateOpening,
		LocalPrivkey:           *channelPrivateKey,
		RemotePubkey:           *remoteChannelPubkey,
		LocalRevocationPrivkey: *firstRevocationKey,
		RemoteRevocationPubkey: *remoteRevocationPubkey,
		LocalPayoutScript:      script,
		RemotePayoutScript:     remotePayoutScript,
		ChannelAddress:         channelAddr.String(),
		RedeemScript:           redeemScript,
	}

	return channel, nil
}

// buildCommitmentTransaction will build a new commitment transaction using all the data from the channel.
// if forLocalNode is set we will build a commitment transaction for our local node otherwise it will be
// the commitment for the remote node.
func (c *Channel) buildCommitmentTransaction(forLocalNode bool, params *chaincfg.Params) (*wire.MsgTx, []byte, error) {
	// Start with a tx paying from the multisig input
	tx := &wire.MsgTx{
		TxIn: []*wire.TxIn{
			{
				PreviousOutPoint: c.FundingOutpoint,
				Sequence:         wire.MaxTxInSequenceNum,
			},
		},
		Version:  1,
		LockTime: 0,
	}

	// Here we select the values and output scripts based on the forLocalNode bool
	// If this is a commitment for the remote peer, the 'standard' output should be
	// paying the local node while the 'breach' output should pay the remote peer.
	//
	// If the commitment is for the local node, the 'standard' output should pay the
	// remote peer while 'breach' output pays us.
	// OP_IF
	//   2 <revocationPubkey> <aliceCommentPubkey> 2 OP_CHECKMULTISIG
	// OP_ELSE
	//   <delay> OP_CHECKSEQUENCEVERIFY OP_DROP
	//   <bobCommitmentPubkey> OP_CHECKSIG
	var revocationPubkey, commitmentPubkey, delayPubkey *bchec.PublicKey
	var standardScript []byte
	var standardValue, breachValue bchutil.Amount
	if forLocalNode {
		revocationPubkey = c.LocalRevocationPrivkey.PubKey()
		commitmentPubkey = &c.RemotePubkey
		delayPubkey = c.LocalPrivkey.PubKey()
		standardScript = c.RemotePayoutScript
		standardValue = c.RemoteBalance
		breachValue = c.LocalBalance
	} else {
		revocationPubkey = &c.RemoteRevocationPubkey
		commitmentPubkey = c.LocalPrivkey.PubKey()
		delayPubkey = &c.RemotePubkey
		standardScript = c.LocalPayoutScript
		standardValue = c.LocalBalance
		breachValue = c.RemoteBalance
	}

	breachAddr, _, err := buildBreachRemedyAddress(revocationPubkey, commitmentPubkey, delayPubkey, c.Delay, params)
	if err != nil {
		return nil, nil, err
	}
	breachScript, err := txscript.PayToAddrScript(breachAddr)
	if err != nil {
		return nil, nil, err
	}
	breachOutput := &wire.TxOut{
		PkScript: breachScript,
	}
	standardOutput := &wire.TxOut{
		PkScript: standardScript,
	}

	// Don't add any outputs below the dust limit
	if standardValue > c.DustLimit {
		standardOutput.Value = int64(standardValue)
		tx.TxOut = append(tx.TxOut, standardOutput)
	}
	if breachValue > c.DustLimit {
		breachOutput.Value = int64(breachValue)
		tx.TxOut = append(tx.TxOut, breachOutput)
	}

	// Sanity check. This shouldn't happen.
	if len(tx.TxOut) == 0 {
		return nil, nil, errors.New("both outputs below dust threshold")
	}

	// Calculate txfee and split it evenly between both outputs. If only one output is
	// being paid it will pay the entire fee.
	size := txsizes.EstimateSerializeSize(1, tx.TxOut, false)
	fee := int(c.FeePerByte) * size
	if len(tx.TxOut) == 1 {
		tx.TxOut[0].Value -= int64(fee)
	} else {
		for _, out := range tx.TxOut {
			out.Value -= int64(fee / 2)
		}
	}
	sig, err := txscript.RawTxInSignature(tx, 0, c.RedeemScript, txscript.SigHashAll, &c.LocalPrivkey, int64(c.LocalBalance+c.RemoteBalance))
	if err != nil {
		return nil, nil, err
	}
	return tx, sig, err
}

func buildP2SHAddress(alicePubkey, bobPubkey *bchec.PublicKey, params *chaincfg.Params) (bchutil.Address, []byte, error) {
	builder := txscript.NewScriptBuilder()
	builder.AddInt64(2)
	builder.AddData(alicePubkey.SerializeCompressed())
	builder.AddData(bobPubkey.SerializeCompressed())
	builder.AddInt64(2)
	builder.AddOp(txscript.OP_CHECKMULTISIG)
	redeemScript, err := builder.Script()
	if err != nil {
		return nil, nil, err
	}
	addr, err := bchutil.NewAddressScriptHash(redeemScript, params)
	if err != nil {
		return nil, nil, err
	}
	return addr, redeemScript, nil
}

func buildBreachRemedyAddress(revocationPubkey, commitmentPubkey, delayPubkey *bchec.PublicKey, delay uint32, params *chaincfg.Params) (bchutil.Address, []byte, error) {
	builder := txscript.NewScriptBuilder()
	builder.AddOp(txscript.OP_IF).
		AddInt64(2).
		AddData(revocationPubkey.SerializeCompressed()).
		AddData(commitmentPubkey.SerializeCompressed()).
		AddInt64(2).
		AddOp(txscript.OP_CHECKMULTISIG).
		AddOp(txscript.OP_ELSE).
		AddInt64(int64(delay)).
		AddOp(txscript.OP_CHECKSEQUENCEVERIFY).
		AddOp(txscript.OP_DROP).
		AddData(delayPubkey.SerializeCompressed()).
		AddOp(txscript.OP_CHECKSIG).
		AddOp(txscript.OP_ENDIF)

	redeemScript, err := builder.Script()
	if err != nil {
		return nil, nil, err
	}
	addr, err := bchutil.NewAddressScriptHash(redeemScript, params)
	if err != nil {
		return nil, nil, err
	}
	return addr, redeemScript, nil
}

func buildCommitmentScriptSig(sig1, sig2, redeemScript []byte) ([]byte, error) {
	builder := txscript.NewScriptBuilder()
	builder.AddOp(txscript.OP_0)
	builder.AddData(sig1)
	builder.AddData(sig2)
	builder.AddData(redeemScript)
	return builder.Script()
}

func (c *Channel) validateCommitmentSignature(tx *wire.MsgTx, params *chaincfg.Params) bool {
	sigHashes := txscript.NewTxSigHashes(tx)

	addr, err := bchutil.DecodeAddress(c.ChannelAddress, params)
	if err != nil {
		return false
	}
	scriptPubkey, err := txscript.PayToAddrScript(addr)
	if err != nil {
		return false
	}
	engine, err := txscript.NewEngine(scriptPubkey, tx, 0, txscript.StandardVerifyFlags, nil, sigHashes, int64(c.LocalBalance + c.RemoteBalance))
	if err != nil {
		return false
	}
	return engine.Execute() == nil
}
