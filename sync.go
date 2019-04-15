package dcrlibwallet

import (
	"context"
	"net"
	"strings"
	"sync"

	"github.com/decred/dcrd/addrmgr"
	"github.com/decred/dcrd/rpcclient"
	"github.com/decred/dcrwallet/chain"
	"github.com/decred/dcrwallet/errors"
	"github.com/decred/dcrwallet/p2p"
	"github.com/decred/dcrwallet/spv"
	"github.com/decred/dcrwallet/wallet"
	"github.com/raedahgroup/dcrlibwallet/utils"
	"github.com/raedahgroup/dcrlibwallet/blockchainsync"
)

type syncData struct {
	mu                    sync.Mutex
	rpcClient             *chain.RPCClient
	cancelSync            context.CancelFunc
	syncProgressListeners []blockchainsync.ProgressListener
	rescanning            bool
}

func (lw *LibWallet) AddSyncProgressListener(syncProgressListener blockchainsync.ProgressListener) {
	lw.syncProgressListeners = append(lw.syncProgressListeners, syncProgressListener)
}

func (lw *LibWallet) SpvSync(peerAddresses string) error {
	loadedWallet, err := lw.getLoadedWalletForSyncing()
	if err != nil {
		return err
	}

	addr := &net.TCPAddr{IP: net.ParseIP("::1"), Port: 0}
	addrManager := addrmgr.New(lw.walletDataDir, net.LookupIP) // TODO: be mindful of tor
	lp := p2p.NewLocalPeer(loadedWallet.ChainParams(), addr, addrManager)

	var validPeerAddresses []string
	if peerAddresses != "" {
		addresses := strings.Split(peerAddresses, ";")
		for _, address := range addresses {
			peerAddress, err := utils.NormalizeAddress(address, lw.activeNet.Params.DefaultPort)
			if err != nil {
				lw.notifySyncError(3, errors.E("SPV peer address invalid: %v", err))
			} else {
				validPeerAddresses = append(validPeerAddresses, peerAddress)
			}
		}

		if len(validPeerAddresses) == 0 {
			return errors.New(ErrInvalidPeers)
		}
	}

	syncer := spv.NewSyncer(loadedWallet, lp)
	syncer.SetNotifications(lw.spvSyncNotificationCallbacks(loadedWallet))
	if len(validPeerAddresses) > 0 {
		syncer.SetPersistantPeers(validPeerAddresses)
	}

	loadedWallet.SetNetworkBackend(syncer)
	lw.walletLoader.SetNetworkBackend(syncer)

	ctx, cancel := contextWithShutdownCancel(context.Background())
	lw.cancelSync = cancel

	// syncer.Run uses a wait group to block the thread until blockchainsync completes or an error occurs
	go func() {
		err := syncer.Run(ctx)
		if err != nil {
			if err == context.Canceled {
				lw.notifySyncError(1, errors.E("SPV synchronization canceled: %v", err))
			} else if err == context.DeadlineExceeded {
				lw.notifySyncError(2, errors.E("SPV synchronization deadline exceeded: %v", err))
			} else {
				lw.notifySyncError(-1, err)
			}
		}
	}()

	return nil
}

func (lw *LibWallet) RpcSync(networkAddress string, username string, password string, cert []byte) error {
	loadedWallet, err := lw.getLoadedWalletForSyncing()
	if err != nil {
		return err
	}

	ctx, cancel := contextWithShutdownCancel(context.Background())
	lw.cancelSync = cancel

	chainClient, err := lw.connectToRpcClient(ctx, networkAddress, username, password, cert)
	if err != nil {
		return err
	}

	syncer := chain.NewRPCSyncer(loadedWallet, chainClient)
	syncer.SetNotifications(lw.generalSyncNotificationCallbacks(loadedWallet))

	networkBackend := chain.BackendFromRPCClient(chainClient.Client)
	lw.walletLoader.SetNetworkBackend(networkBackend)
	loadedWallet.SetNetworkBackend(networkBackend)

	// notify blockchainsync progress listeners that connected peer count will not be reported because we're using rpc
	for _, syncProgressListener := range lw.syncProgressListeners {
		syncProgressListener.OnPeerDisconnected(-1)
	}

	// syncer.Run uses a wait group to block the thread until blockchainsync completes or an error occurs
	go func() {
		err := syncer.Run(ctx, true)
		if err != nil {
			if err == context.Canceled {
				lw.notifySyncError(1, errors.E("SPV synchronization canceled: %v", err))
			} else if err == context.DeadlineExceeded {
				lw.notifySyncError(2, errors.E("SPV synchronization deadline exceeded: %v", err))
			} else {
				lw.notifySyncError(-1, err)
			}
		}
	}()

	return nil
}

func (lw *LibWallet) connectToRpcClient(ctx context.Context, networkAddress string, username string, password string,
	cert []byte) (chainClient *chain.RPCClient, err error) {

	lw.mu.Lock()
	chainClient = lw.rpcClient
	lw.mu.Unlock()

	// If the rpcClient is already set, you can just use that instead of attempting a new connection.
	if chainClient != nil {
		return
	}

	// rpcClient is not already set, attempt a new connection.
	networkAddress, err = utils.NormalizeAddress(networkAddress, lw.activeNet.JSONRPCClientPort)
	if err != nil {
		return nil, errors.New(ErrInvalidAddress)
	}
	chainClient, err = chain.NewRPCClient(lw.activeNet.Params, networkAddress, username, password, cert, len(cert) == 0)
	if err != nil {
		return nil, translateError(err)
	}

	err = chainClient.Start(ctx, false)
	if err != nil {
		if err == rpcclient.ErrInvalidAuth {
			return nil, errors.New(ErrInvalid)
		}
		if errors.Match(errors.E(context.Canceled), err) {
			return nil, errors.New(ErrContextCanceled)
		}
		return nil, errors.New(ErrUnavailable)
	}

	// Set rpcClient so it can be used subsequently without re-connecting to the rpc server.
	lw.mu.Lock()
	lw.rpcClient = chainClient
	lw.mu.Unlock()

	return
}

func (lw *LibWallet) getLoadedWalletForSyncing() (*wallet.Wallet, error) {
	loadedWallet, walletLoaded := lw.walletLoader.LoadedWallet()
	if walletLoaded {
		// Error if the wallet is already syncing with the network.
		currentNetworkBackend, _ := loadedWallet.NetworkBackend()
		if currentNetworkBackend != nil {
			return nil, errors.New(ErrSyncAlreadyInProgress)
		}
	} else {
		return nil, errors.New(ErrWalletNotLoaded)
	}
	return loadedWallet, nil
}

func (lw *LibWallet) CancelSync() {
	if lw.cancelSync != nil {
		lw.cancelSync()
	}

	for _, syncResponse := range lw.syncProgressListeners {
		syncResponse.OnSynced(false)
	}
}

func (lw *LibWallet) RescanBlocks() error {
	netBackend, err := lw.wallet.NetworkBackend()
	if err != nil {
		return errors.E(ErrNotConnected)
	}

	if lw.rescanning {
		return errors.E(ErrInvalid)
	}

	go func() {
		defer func() {
			lw.rescanning = false
		}()
		lw.rescanning = true
		progress := make(chan wallet.RescanProgress, 1)
		ctx, _ := contextWithShutdownCancel(context.Background())

		var totalHeight int32
		go lw.wallet.RescanProgressFromHeight(ctx, netBackend, 0, progress)

		for p := range progress {
			if p.Err != nil {
				log.Error(p.Err)

				return
			}
			totalHeight += p.ScannedThrough
			for _, syncProgressListener := range lw.syncProgressListeners {
				syncProgressListener.OnRescan(p.ScannedThrough, blockchainsync.PROGRESS)
			}
		}

		select {
		case <-ctx.Done():
			for _, syncProgressListener := range lw.syncProgressListeners {
				syncProgressListener.OnRescan(totalHeight, blockchainsync.PROGRESS)
			}
		default:
			for _, syncProgressListener := range lw.syncProgressListeners {
				syncProgressListener.OnRescan(totalHeight, blockchainsync.FINISH)
			}
		}
	}()

	return nil
}

func (lw *LibWallet) GetBestBlock() int32 {
	_, height := lw.wallet.MainChainTip()
	return height
}

func (lw *LibWallet) GetBestBlockTimeStamp() int64 {
	_, height := lw.wallet.MainChainTip()
	identifier := wallet.NewBlockIdentifierFromHeight(height)
	info, err := lw.wallet.BlockInfo(identifier)
	if err != nil {
		log.Error(err)
		return 0
	}
	return info.Timestamp
}