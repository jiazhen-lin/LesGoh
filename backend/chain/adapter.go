package chain

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"time"

	suiAcc "github.com/coming-chat/go-sui/account"
	suiCli "github.com/coming-chat/go-sui/client"
	suiTypes "github.com/coming-chat/go-sui/types"
	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"
	aptosCli "github.com/portto/aptos-go-sdk/client"
	uuid "github.com/satori/go.uuid"
	"github.com/sirupsen/logrus"
	"gorm.io/gorm"

	"github.com/x24870/LesGoh/backend/contracts/accountrouter"
	"github.com/x24870/LesGoh/backend/contracts/mailbox"
	"github.com/x24870/LesGoh/backend/contracts/nft"
	"github.com/x24870/LesGoh/backend/contracts/paymaster"
)

type TransactionStatus int

const (
	TransactionStatusPending TransactionStatus = iota
	TransactionStatusSuccess
	TransactionStatusFailed
)

const (
	targetEventType = "0xec42a352cc65eca17a9fa85d0fc602295897ed6b8b8af6a6c79ef490eb8f9eba::amm_swap::SwapEvent"

	aptosRpcUrl = "https://fullnode.testnet.aptoslabs.com"
	// aptosRpcUrl = "https://fullnode.mainnet.aptoslabs.com"
	suiRpcUrl = "https://fullnode.devnet.sui.io:443"

	endpoint = "https://ethereum-goerli.publicnode.com" // goerli

	pk              = "3f65e43afa4731c92c9570728ef74836ab2fb40ce03adb69ad3e214498feb7a0"            // for test only
	mnemonic        = "tag couch antenna aerobic dignity boost equal unfold peanut humble seek ten" // for test only
	nftCreator      = "0xda0cd74d3815ed8e319a97c1e8ff9e162aac4ac6d7a1bbc6ef614f7892d11ede"
	nftAddress      = ""
	nftHandleStruct = "0xda0cd74d3815ed8e319a97c1e8ff9e162aac4ac6d7a1bbc6ef614f7892d11ede::urn::UrnMinter"
	nftFieldName    = "burn_event"

	// pkgId    = "0x37a7b60b7e8c4ae9799b0b1e748f98aa03a818bb"
	pkgId  = "0x8aa7000f11ef9b7ffec4c20e68fcdad4a9882226"
	gasId  = "0x19d4f97e22da92811478b8c9a69fc920a190aa8b"
	maxGas = 30000
	seed   = "AH+Jiv+xw/G5t/8wmBsAv+bbTIez6qeqqA/0xETqQsGz55Uow52GZi58ExLkhgYJnt7vZKZ45Qf5brj7HmRlqjQ="
)

var (
	ethClient *ethclient.Client
	chainID   *big.Int

	goerliAccountRouter = common.HexToAddress("0x55486284a85d7b51a7bBfd343702414D65276fa6")
	goerliPaymaster     = common.HexToAddress("0xF90cB82a76492614D07B82a7658917f3aC811Ac1")
	goerliMailBox       = common.HexToAddress("0xCC737a94FecaeC165AbCf12dED095BB13F037685")

	accountRouterABI  abi.ABI
	mailboxABI        abi.ABI
	nftABI            abi.ABI
	dispatchIdEventID common.Hash
	gasPaymaster      *paymaster.Paymaster

	mumbaiDest     uint32 = 80001
	mumbaiReceiver        = common.HexToAddress("0x36FdA966CfffF8a9Cdc814f546db0e6378bFef35")
	mumbaiNFT             = common.HexToAddress("0xdf67E4A6B7953782987f5684635e93f24a211D74")
)

type TransactionRow struct {
	ID                 uuid.UUID         `gorm:"column:id;primary_key;type:uuid;default:uuid_generate_v4()"`
	TransactionHash    string            `gorm:"uniqueIndex;column:transaction_hash;type:varchar(200)"`
	Status             TransactionStatus `gorm:"column:status"`
	Address            string            `gorm:"column:address;type:varchar(100)"`
	RecTransactionHash string            `gorm:"column:rec_transaction_hash;type:varchar(100)"`
	CreatedAt          time.Time         `gorm:"column:created_at"`
	UpdatedAt          time.Time         `gorm:"column:updated_at"`
}

func NewAdapter() *Adapter {
	db, err := NewPostgresSQL()
	if err != nil {
		panic(err)
	}

	if err := migrateUp(db); err != nil {
		panic(err)
	}

	ac := aptosCli.NewAptosClient(aptosRpcUrl)
	sc, err := suiCli.Dial(suiRpcUrl)
	if err != nil {
		panic(err)
	}

	ethClient, err = ethclient.Dial(endpoint)
	if err != nil {
		panic(err)
	}
	chainID, err = ethClient.NetworkID(context.Background())
	if err != nil {
		panic(err)
	}
	fmt.Println("chain id: ", chainID)

	accountRouterABI, err = abi.JSON(bytes.NewReader([]byte(accountrouter.AccountrouterABI)))
	if err != nil {
		panic(err)
	}
	mailboxABI, err = abi.JSON(bytes.NewReader([]byte(mailbox.MailboxABI)))
	if err != nil {
		panic(err)
	}
	nftABI, err = abi.JSON(bytes.NewReader([]byte(nft.NftABI)))
	if err != nil {
		panic(err)
	}
	dispatchIdEvent, ok := mailboxABI.Events["DispatchId"]
	if !ok {
		panic(errors.New("event DispatchId not found"))
	}
	dispatchIdEventID = dispatchIdEvent.ID
	gasPaymaster, err = paymaster.NewPaymaster(
		goerliPaymaster,
		ethClient,
	)
	if err != nil {
		panic(err)
	}

	a := Adapter{
		db:        db,
		aptClient: ac,
		suiClient: sc,
	}
	a.sync()

	return &a
}

type Adapter struct {
	db        *gorm.DB
	aptClient aptosCli.AptosClient
	suiClient *suiCli.Client
}

func (a *Adapter) sync() {
	go func() {
		ticker := time.NewTicker(15 * time.Second)
		for {
			select {
			case _ = <-ticker.C:
				// logrus.WithField("tick", t).Info("ticker")
				// _ = a.checkTx()
				_ = a.findTx()
			}
		}
	}()
}

func (a *Adapter) findTx() error {
	ctx := context.Background()
	events, err := a.aptClient.GetEventsByEventHandle(
		ctx,
		nftCreator,
		nftHandleStruct,
		nftFieldName,
		0, 10000,
	)
	if err != nil {
		return err
	}
	for _, event := range events {
		version := "version_" + strconv.FormatUint(uint64(event.Version), 10)
		destAddr := event.Data["des_addr"].(string)
		logrus.WithField("version", version).Info("find event")

		if err := a.mintByHyperlane(ctx, version, destAddr); err != nil {
			logrus.WithField("err", err).Error("failed to mint")
		}
	}
	return nil
}

func getAddress(ctx context.Context) (common.Address, error) {
	privateKey, err := crypto.HexToECDSA(pk)
	if err != nil {
		return common.Address{}, err
	}
	publicKey := privateKey.Public()
	publicKeyECDSA, ok := publicKey.(*ecdsa.PublicKey)
	if !ok {
		return common.Address{}, fmt.Errorf("failed to get public key")
	}

	address := crypto.PubkeyToAddress(*publicKeyECDSA)
	return address, nil
}

func mintNTF(ctx context.Context, user common.Address, target string) (common.Hash, error) {
	mintData, err := nftABI.Pack(
		"mint",
		common.HexToAddress(target),
	)
	if err != nil {
		return common.Hash{}, err
	}

	callData, err := accountRouterABI.Pack(
		"callRemote",
		mumbaiDest,
		mumbaiNFT,
		big.NewInt(0),
		mintData,
	)
	if err != nil {
		return common.Hash{}, err
	}
	estGas, err := ethClient.EstimateGas(ctx, ethereum.CallMsg{
		From:  user,
		To:    &goerliAccountRouter,
		Value: nil,
		Data:  callData,
	})
	if err != nil {
		rpcErr, ok := err.(rpc.Error)
		if !ok {
			fmt.Printf("unexpected estimate error: %+v\n", err)
			return common.Hash{}, err
		}
		// error message: err: insufficient funds for gas * price + value: address 0x0b43BCe5Cfc9Fcc9B25E70e5d7022c2f1D5468ad have 189978999999664000 want 1000000000000000000 (supplied gas 10020264)
		// error code: -32000
		fmt.Printf("estimate error: %+v\n", err)
		fmt.Printf("error message: %s\n", rpcErr.Error())
		fmt.Printf("error code: %d\n", rpcErr.ErrorCode())
		fmt.Printf("error code: %d\n", rpcErr.ErrorCode())

		rpcDataErr, ok := err.(rpc.DataError)
		if !ok {
			fmt.Println("not support data")
			return common.Hash{}, err
		}
		// error message: err: insufficient funds for gas * price + value: address 0x0b43BCe5Cfc9Fcc9B25E70e5d7022c2f1D5468ad have 189978999999664000 want 1000000000000000000 (supplied gas 10020264)
		// error code: -32000
		fmt.Printf("data error: %+v\n", rpcDataErr)
		fmt.Printf("error message: %s\n", rpcDataErr.Error())
		fmt.Printf("error data: %+v\n", rpcDataErr.ErrorData())
		return common.Hash{}, fmt.Errorf("estimate gas error: %v", err)
	}

	nonce, err := ethClient.PendingNonceAt(ctx, user)
	if err != nil {
		return common.Hash{}, err
	}
	price, err := ethClient.SuggestGasPrice(ctx)
	if err != nil {
		return common.Hash{}, err
	}
	tip, err := ethClient.SuggestGasTipCap(ctx)
	if err != nil {
		return common.Hash{}, err
	}
	fmt.Println("nonce: ", nonce)
	fmt.Println("estRes: ", estGas)
	fmt.Println("price: ", price)
	fmt.Println("tip: ", tip)

	tx := types.NewTx(&types.DynamicFeeTx{
		ChainID:   chainID,
		Nonce:     nonce,
		GasFeeCap: price,
		GasTipCap: tip,
		Gas:       estGas,
		To:        &goerliAccountRouter,
		Value:     nil,
		Data:      callData,
	})
	privateKey, err := crypto.HexToECDSA(pk)
	if err != nil {
		return common.Hash{}, err
	}
	tx, err = types.SignTx(tx, types.LatestSignerForChainID(chainID), privateKey)
	if err != nil {
		return common.Hash{}, err
	}
	txHash := tx.Hash()
	err = ethClient.SendTransaction(ctx, tx)
	if err != nil {
		rpcErr, ok := err.(rpc.Error)
		if !ok {
			fmt.Printf("unexpected send error: %+v\n", err)
			return common.Hash{}, err
		}
		// error message: err: insufficient funds for gas * price + value: address 0x0b43BCe5Cfc9Fcc9B25E70e5d7022c2f1D5468ad have 189978999999664000 want 1000000000000000000 (supplied gas 10020264)
		// error code: -32000
		fmt.Printf("send error: %+v\n", err)
		fmt.Printf("error message: %s\n", rpcErr.Error())
		fmt.Printf("error code: %d\n", rpcErr.ErrorCode())
		fmt.Printf("error code: %d\n", rpcErr.ErrorCode())
		fmt.Printf("compare error: %v\n", err == core.ErrNonceTooLow)
		fmt.Printf("compare error: %v\n", errors.Is(err, core.ErrNonceTooLow))
		fmt.Printf("compare error: %v\n", errors.Is(rpcErr, core.ErrNonceTooLow))

		rpcDataErr, ok := err.(rpc.DataError)
		if !ok {
			fmt.Println("not support data")
			return common.Hash{}, err
		}
		// error message: err: insufficient funds for gas * price + value: address 0x0b43BCe5Cfc9Fcc9B25E70e5d7022c2f1D5468ad have 189978999999664000 want 1000000000000000000 (supplied gas 10020264)
		// error code: -32000
		fmt.Printf("data error: %+v\n", rpcDataErr)
		fmt.Printf("error message: %s\n", rpcDataErr.Error())
		fmt.Printf("error data: %+v\n", rpcDataErr.ErrorData())

		return common.Hash{}, err
	}

	return txHash, nil
}

func (a *Adapter) mintByHyperlane(ctx context.Context, version, destAddr string) error {
	var tx TransactionRow
	if err := a.db.Where("transaction_hash = ?", version).First(&tx).Error; err != nil {
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return fmt.Errorf("failed to get tx: %w", err)
		}
	} else {
		// row exists
		return nil
	}

	// write record
	if err := a.db.Create(&TransactionRow{
		TransactionHash: version,
		Status:          TransactionStatusPending,
		Address:         destAddr,
		CreatedAt:       time.Now(),
	}).Error; err != nil {
		return fmt.Errorf("failed to create tx: %w", err)
	}

	// mint
	address, err := getAddress(ctx)
	if err != nil {
		return err
	}
	txHash, err := mintNTF(ctx, address, destAddr)
	if err != nil {
		return err
	}

	var record TransactionRow
	if err := a.db.Where("transaction_hash = ?", version).First(&record).Error; err != nil {
		return fmt.Errorf("failed to get tx: %w", err)
	}
	updateStatus := TransactionStatusSuccess
	logrus.WithFields(logrus.Fields{
		"id":       record.ID,
		"version":  version,
		"destAddr": destAddr,
		"txHash":   txHash.Hex(),
	}).Info("mint NFT for user")

	if err := a.updateTransactionRow(record, updateStatus, txHash.Hex()); err != nil {
		logrus.WithFields(logrus.Fields{
			"err":     err,
			"id":      record.ID,
			"version": version,
			"txHash":  txHash.Hex(),
		}).Error("failed to update tx")
	}

	logrus.WithField("version", version).Info("receive a tx hash")

	return nil
}

func (a *Adapter) checkTx() error {
	var txs []TransactionRow
	if err := a.db.Where("status = ?", TransactionStatusPending).Find(&txs).Error; err != nil {
		logrus.WithField("err", err).Error("failed to get rows")
		return err
	}

	for _, tx := range txs {
		hash := tx.TransactionHash
		suiAddress := tx.Address

		logrus.WithFields(logrus.Fields{
			"hash":       hash,
			"suiAddress": suiAddress,
		}).Info("find a pending tx")

		ctx := context.Background()
		_, err := a.aptClient.GetTransactionByHash(ctx, hash)
		if err != nil {
			logrus.WithFields(logrus.Fields{
				"err":  err,
				"hash": hash,
			}).Error("GetTransactionByHash error")

			// mark as failed
			// if err := a.updateTransactionRow(tx, TransactionStatusFailed, ""); err != nil {
			// 	logrus.WithFields(logrus.Fields{
			// 		"err":  err,
			// 		"id":   tx.ID,
			// 		"hash": hash,
			// 	}).Error("failed to update tx")
			// }
			continue
		}

		// var find bool
		// var suiAddress string
		// for _, event := range resp.Events {
		// 	logrus.WithFields(logrus.Fields{
		// 		"hash": hash,
		// 		"type": event.Type,
		// 	}).Debug("pring event type")

		// 	if event.Type == targetEventType {
		// 		find = true
		// 		// got suiAddress by event data
		// 		break
		// 	}
		// }

		// if !find {
		// 	// mark as failed
		// 	logrus.WithFields(logrus.Fields{
		// 		"id":   tx.ID,
		// 		"hash": hash,
		// 	}).Info("update event to failed")

		// 	if err := a.updateTransactionRow(tx, TransactionStatusFailed, ""); err != nil {
		// 		logrus.WithFields(logrus.Fields{
		// 			"err":  err,
		// 			"id":   tx.ID,
		// 			"hash": hash,
		// 		}).Error("failed to update tx")
		// 	}
		// 	continue
		// }

		// if find -> call sui
		// sui client call --function mint --module devnet_nft --package 0x2 --args "test nft" "test net desc" "ipfs://QmYsPomv1uqCdPMN95eJdVbJm58mwpLewaroHoeqhVBxSV" --gas-budget 2000
		updateStatus := TransactionStatusSuccess
		suiHash, err := a.mint(ctx, hash, suiAddress)
		if err != nil {
			updateStatus = TransactionStatusFailed
		}

		logrus.WithFields(logrus.Fields{
			"id":         tx.ID,
			"hash":       hash,
			"suiAddress": suiAddress,
			"suiHash":    suiHash,
			"err":        err,
		}).Info("mint sui NFT for user")

		if err := a.updateTransactionRow(tx, updateStatus, suiHash); err != nil {
			logrus.WithFields(logrus.Fields{
				"err":  err,
				"id":   tx.ID,
				"hash": hash,
			}).Error("failed to update tx")
		}
	}

	return nil
}

func (a *Adapter) updateTransactionRow(tx TransactionRow, newStatus TransactionStatus, hash string) error {
	result := a.db.Model(TransactionRow{}).
		Where("id = ? AND status =?", tx.ID, TransactionStatusPending).
		Updates(map[string]interface{}{
			"status":               newStatus,
			"rec_transaction_hash": hash,
			"updated_at":           time.Now(),
		})
	if err := result.Error; err != nil {
		logrus.WithFields(logrus.Fields{
			"err":  err,
			"id":   tx.ID,
			"hash": tx.TransactionHash,
		}).Error("failed to update tx")
	}
	if result.RowsAffected == 0 {
		logrus.WithFields(logrus.Fields{
			"id":   tx.ID,
			"hash": tx.TransactionHash,
		}).Error("result.RowsAffected == 0")
	}
	return nil
}

func (a *Adapter) mint(ctx context.Context, hash string, suiAddress string) (suiHash string, err error) {
	acc, err := suiAcc.NewAccountWithMnemonic(mnemonic)
	if err != nil {
		logrus.WithField("err", err).Error("account.NewAccountWithMnemonic error")
		return "", err
	}
	signer, _ := suiTypes.NewAddressFromHex(acc.Address)

	pkgId, err := suiTypes.NewHexData(pkgId)
	if err != nil {
		logrus.WithField("err", err).Error("types.NewHexData err")
		return "", err
	}
	gasObjId, err := suiTypes.NewHexData(gasId)
	if err != nil {
		logrus.WithField("err", err).Error("types.NewHexData err")
		return "", err
	}

	args := []interface{}{
		// "0x9817db7e507a682b29354f1df62b3d1289ee0da6", 42, 7, suiAddress,
		"CryptoGranz", "Your granz reborn", "ipfs://QmbtYzcCi12b9Kdwb59hwtsNLEPQ6XZqVVpFZqFcReSkjB", suiAddress,
	}

	txnBytes, err := a.suiClient.MoveCall(ctx, *signer, *pkgId, "rebirth", "mint", []string{}, args, gasObjId, uint64(maxGas))
	if err != nil {
		logrus.WithField("err", err).Error("MoveCall err")
		return "", err
	}
	logrus.WithField("resp", *txnBytes).Debug("MoveCall resp")

	// sign
	signedTxn := txnBytes.SignWith(acc.PrivateKey)
	txnResponse, err := a.suiClient.ExecuteTransaction(ctx, *signedTxn, suiTypes.TxnRequestTypeWaitForLocalExecution)
	if err != nil {
		logrus.WithField("err", err).Error("ExecuteTransaction err")
		return "", err
	}

	logrus.Debug("transaction resp = ", *txnResponse)

	return txnResponse.TransactionDigest().String(), nil
}

func (a *Adapter) Save(hash, suiAddress string) error {
	if err := a.db.Create(&TransactionRow{
		TransactionHash: hash,
		Status:          TransactionStatusPending,
		Address:         suiAddress,
		CreatedAt:       time.Now(),
	}).Error; err != nil {
		return err
	}

	logrus.WithField("hash", hash).Info("receive a tx hash")

	return nil
}

func (a *Adapter) GetPercentage(address string) (int, error) {
	// logrus.WithField("hash", hash).Info("receive a tx hash")
	return 0, nil
}

func migrateUp(db *gorm.DB) error {
	if err := db.Exec(`CREATE EXTENSION IF NOT EXISTS "uuid-ossp" ;`).Error; err != nil {
		return err
	}
	m := &TransactionRow{}
	if err := db.AutoMigrate(m); err != nil {
		return err
	}
	return nil
}
