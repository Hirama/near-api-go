package near

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"math/big"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/aurora-is-near/near-api-go/utils"
	"github.com/btcsuite/btcutil/base58"
	"github.com/near/borsh-go"
)

const ed25519Prefix = "ed25519:"

// Default number of retries with different nonce before giving up on a transaction.
const txNonceRetryNumber = 12

// Default wait until next retry in milli seconds.
const txNonceRetryWait = 500

// Exponential back off for waiting to retry.
const txNonceRetryWaitBackoff = 1.5

// Account defines access credentials for a NEAR account.
type Account struct {
	AccountID                 string `json:"account_id"`
	PublicKey                 string `json:"public_key"`
	PrivateKey                string `json:"private_key"`
	conn                      *Connection
	pubKey                    ed25519.PublicKey
	privKey                   ed25519.PrivateKey
	accessKeyByPublicKeyCache map[string]map[string]interface{}
}

// LoadAccount loads the credential for the receiverID account, to be used via
// connection c, and returns it.
func LoadAccount(c *Connection, cfg *Config, receiverID string) (*Account, error) {
	var a Account
	a.conn = c
	if err := a.locateAccessKey(cfg, receiverID); err != nil {
		return nil, err
	}
	a.accessKeyByPublicKeyCache = make(map[string]map[string]interface{})
	return &a, nil
}

func (a *Account) locateAccessKey(cfg *Config, receiverID string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	fn := filepath.Join(home, ".near-credentials", cfg.NetworkID, receiverID+".json")
	return a.readAccessKey(fn, receiverID)
}

func (a *Account) readAccessKey(filename, receiverID string) error {
	buf, err := ioutil.ReadFile(filename)
	if err != nil {
		return err
	}
	err = json.Unmarshal(buf, &a)
	if err != nil {
		return err
	}
	// account ID
	if a.AccountID != receiverID {
		return fmt.Errorf("near: parsed account_id '%s' does not match with receiverID '%s'",
			a.AccountID, receiverID)
	}
	// public key
	if !strings.HasPrefix(a.PublicKey, ed25519Prefix) {
		return fmt.Errorf("near: parsed public_key '%s' is not an Ed25519 key",
			a.PublicKey)
	}
	pubKey := base58.Decode(strings.TrimPrefix(a.PublicKey, ed25519Prefix))
	a.pubKey = ed25519.PublicKey(pubKey)
	// private key
	if !strings.HasPrefix(a.PrivateKey, ed25519Prefix) {
		return fmt.Errorf("near: parsed private_key '%s' is not an Ed25519 key",
			a.PrivateKey)
	}
	privateKey := base58.Decode(strings.TrimPrefix(a.PrivateKey, ed25519Prefix))
	a.privKey = ed25519.PrivateKey(privateKey)
	// make sure keys match
	if !bytes.Equal(pubKey, a.privKey.Public().(ed25519.PublicKey)) {
		return fmt.Errorf("near: public_key does not match private_key: %s", filename)
	}
	return nil
}

// SendMoney sends amount NEAR from account to receiverID.
func (a *Account) SendMoney(
	receiverID string,
	amount big.Int,
) (map[string]interface{}, error) {
	return a.SignAndSendTransaction(receiverID, []Action{{
		Enum: 3,
		Transfer: Transfer{
			Deposit: amount,
		},
	}})
}

// DeleteAccount deletes the account and sends the remaining Ⓝ balance to the
// account beneficiaryID.
func (a *Account) DeleteAccount(
	beneficiaryID string,
) (map[string]interface{}, error) {
	return a.SignAndSendTransaction(a.AccountID, []Action{{
		Enum: 7,
		DeleteAccount: DeleteAccount{
			BeneficiaryID: beneficiaryID,
		},
	}})
}

// SignAndSendTransaction signs the given actions and sends them as a transaction to receiverID.
func (a *Account) SignAndSendTransaction(
	receiverID string,
	actions []Action,
) (map[string]interface{}, error) {
	return utils.ExponentialBackoff(txNonceRetryWait, txNonceRetryNumber, txNonceRetryWaitBackoff,
		func() (map[string]interface{}, error) {
			_, signedTx, err := a.signTransaction(receiverID, actions)
			if err != nil {
				return nil, err
			}

			buf, err := borsh.Serialize(*signedTx)
			if err != nil {
				return nil, err
			}

			return a.conn.SendTransaction(buf)
		})
}

func (a *Account) signTransaction(
	receiverID string,
	actions []Action,
) (txHash []byte, signedTx *SignedTransaction, err error) {
	_, ak, err := a.findAccessKey()
	if err != nil {
		return nil, nil, err
	}

	// get current block hash
	block, err := a.conn.Block()
	if err != nil {
		return nil, nil, err
	}
	blockHash := block["header"].(map[string]interface{})["hash"].(string)

	// create next nonce
	var nonce int64
	jsonNonce, ok := ak["nonce"].(json.Number)
	if ok {
		nonce, err = jsonNonce.Int64()
		if err != nil {
			return nil, nil, err
		}
		nonce++
	}

	// save nonce
	ak["nonce"] = json.Number(strconv.FormatInt(nonce, 10))

	// sign transaction
	return signTransaction(receiverID, uint64(nonce), actions, base58.Decode(blockHash),
		a.pubKey, a.privKey, a.AccountID)

}

func (a *Account) findAccessKey() (publicKey ed25519.PublicKey, accessKey map[string]interface{}, err error) {
	// TODO: Find matching access key based on transaction
	// TODO: use accountId and networkId?
	pk := a.pubKey
	if ak := a.accessKeyByPublicKeyCache[string(publicKey)]; ak != nil {
		return pk, ak, nil
	}
	ak, err := a.conn.ViewAccessKey(a.AccountID, a.PublicKey)
	if err != nil {
		return nil, nil, err
	}
	a.accessKeyByPublicKeyCache[string(publicKey)] = ak
	return pk, ak, nil
}

// FunctionCall performs a NEAR function call.
func (a *Account) FunctionCall(
	contractID, methodName string,
	args []byte,
	gas uint64,
	amount big.Int,
) (map[string]interface{}, error) {
	return a.SignAndSendTransaction(contractID, []Action{{
		Enum: 2,
		FunctionCall: FunctionCall{
			MethodName: methodName,
			Args:       args,
			Gas:        gas,
			Deposit:    amount,
		},
	}})
}
