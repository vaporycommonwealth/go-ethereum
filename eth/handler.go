// Copyright 2015 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package eth

import (
	"errors"
	"fmt"
	"math"
	"math/big"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ethereumproject/go-ethereum/common"
	"github.com/ethereumproject/go-ethereum/core"
	"github.com/ethereumproject/go-ethereum/core/types"
	"github.com/ethereumproject/go-ethereum/eth/downloader"
	"github.com/ethereumproject/go-ethereum/eth/fetcher"
	"github.com/ethereumproject/go-ethereum/ethdb"
	"github.com/ethereumproject/go-ethereum/event"
	"github.com/ethereumproject/go-ethereum/logger"
	"github.com/ethereumproject/go-ethereum/logger/glog"
	"github.com/ethereumproject/go-ethereum/p2p"
	"github.com/ethereumproject/go-ethereum/p2p/discover"
	"github.com/ethereumproject/go-ethereum/pow"
	"github.com/ethereumproject/go-ethereum/rlp"
)

const (
	softResponseLimit = 2 * 1024 * 1024 // Target maximum size of returned blocks, headers or node data.
	estHeaderRlpSize  = 500             // Approximate size of an RLP encoded block header
)

// errIncompatibleConfig is returned if the requested protocols and configs are
// not compatible (low protocol version restrictions and high requirements).
var errIncompatibleConfig = errors.New("incompatible configuration")

func errResp(code errCode, format string, v ...interface{}) error {
	return fmt.Errorf("%v - %v", code, fmt.Sprintf(format, v...))
}

type ProtocolManager struct {
	networkId int

	fastSync uint32 // Flag whether fast sync is enabled (gets disabled if we already have blocks)
	synced   uint32 // Flag whether we're considered synchronised (enables transaction processing)

	txpool      txPool
	blockchain  *core.BlockChain
	chaindb     ethdb.Database
	chainConfig *core.ChainConfig

	downloader *downloader.Downloader
	fetcher    *fetcher.Fetcher
	peers      *peerSet

	SubProtocols []p2p.Protocol

	eventMux      *event.TypeMux
	txSub         event.Subscription
	minedBlockSub event.Subscription

	// channels for fetcher, syncer, txsyncLoop
	newPeerCh   chan *peer
	txsyncCh    chan *txsync
	quitSync    chan struct{}
	noMorePeers chan struct{}

	// wait group is used for graceful shutdowns during downloading
	// and processing
	wg sync.WaitGroup

	badBlockReportingEnabled bool
}

// NewProtocolManager returns a new ethereum sub protocol manager. The Ethereum sub protocol manages peers capable
// with the ethereum network.
func NewProtocolManager(config *core.ChainConfig, fastSync bool, networkId int, mux *event.TypeMux, txpool txPool, pow pow.PoW, blockchain *core.BlockChain, chaindb ethdb.Database) (*ProtocolManager, error) {
	// Create the protocol manager with the base fields
	manager := &ProtocolManager{
		networkId:   networkId,
		eventMux:    mux,
		txpool:      txpool,
		blockchain:  blockchain,
		chaindb:     chaindb,
		chainConfig: config,
		peers:       newPeerSet(),
		newPeerCh:   make(chan *peer),
		noMorePeers: make(chan struct{}),
		txsyncCh:    make(chan *txsync),
		quitSync:    make(chan struct{}),
	}
	// Figure out whether to allow fast sync or not
	if fastSync && blockchain.CurrentBlock().NumberU64() > 0 {
		glog.V(logger.Info).Infof("blockchain not empty, fast sync disabled")
		fastSync = false
	}
	if fastSync {
		manager.fastSync = uint32(1)
	}
	// Initiate a sub-protocol for every implemented version we can handle
	manager.SubProtocols = make([]p2p.Protocol, 0, len(ProtocolVersions))
	for i, version := range ProtocolVersions {
		// Skip protocol version if incompatible with the mode of operation
		if fastSync && version < eth63 {
			continue
		}
		// Compatible; initialise the sub-protocol
		version := version // Closure for the run
		manager.SubProtocols = append(manager.SubProtocols, p2p.Protocol{
			Name:    ProtocolName,
			Version: version,
			Length:  ProtocolLengths[i],
			Run: func(p *p2p.Peer, rw p2p.MsgReadWriter) error {
				peer := manager.newPeer(int(version), p, rw)
				select {
				case manager.newPeerCh <- peer:
					manager.wg.Add(1)
					defer manager.wg.Done()
					return manager.handle(peer)
				case <-manager.quitSync:
					return p2p.DiscQuitting
				}
			},
			NodeInfo: func() interface{} {
				return manager.NodeInfo()
			},
			PeerInfo: func(id discover.NodeID) interface{} {
				if p := manager.peers.Peer(fmt.Sprintf("%x", id[:8])); p != nil {
					return p.Info()
				}
				return nil
			},
		})
	}
	if len(manager.SubProtocols) == 0 {
		return nil, errIncompatibleConfig
	}
	// Construct the different synchronisation mechanisms
	manager.downloader = downloader.New(chaindb, manager.eventMux, blockchain.HasHeader, blockchain.HasBlockAndState, blockchain.GetHeader,
		blockchain.GetBlock, blockchain.CurrentHeader, blockchain.CurrentBlock, blockchain.CurrentFastBlock, blockchain.FastSyncCommitHead,
		blockchain.GetTd, blockchain.InsertHeaderChain, manager.insertChain, blockchain.InsertReceiptChain, blockchain.Rollback,
		manager.removePeer)

	validator := func(block *types.Block, parent *types.Block) error {
		return core.ValidateHeader(config, pow, block.Header(), parent.Header(), true, false)
	}
	heighter := func() uint64 {
		return blockchain.CurrentBlock().NumberU64()
	}
	inserter := func(blocks types.Blocks) (int, error) {
		atomic.StoreUint32(&manager.synced, 1) // Mark initial sync done on any fetcher import
		return manager.insertChain(blocks)
	}
	manager.fetcher = fetcher.New(blockchain.GetBlock, validator, manager.BroadcastBlock, heighter, inserter, manager.removePeer)

	if blockchain.Genesis().Hash().Hex() == defaultGenesisHash && networkId == 1 {
		manager.badBlockReportingEnabled = false
	}

	return manager, nil
}

func (pm *ProtocolManager) insertChain(blocks types.Blocks) (i int, err error) {
	i, err = pm.blockchain.InsertChain(blocks)
	if err != nil && pm.badBlockReportingEnabled && core.IsValidateError(err) {
		go sendBadBlockReport(blocks[i], err)
	}
	return i, err
}

func (pm *ProtocolManager) removePeer(id string) {
	// Short circuit if the peer was already removed
	peer := pm.peers.Peer(id)
	if peer == nil {
		return
	}
	glog.V(logger.Debug).Infoln("Removing peer", id)

	// Unregister the peer from the downloader and Ethereum peer set
	pm.downloader.UnregisterPeer(id)
	if err := pm.peers.Unregister(id); err != nil {
		glog.V(logger.Error).Infoln("Removal failed:", err)
	}
	// Hard disconnect at the networking layer
	if peer != nil {
		peer.Peer.Disconnect(p2p.DiscUselessPeer)
	}
}

func (pm *ProtocolManager) Start() {
	// broadcast transactions
	pm.txSub = pm.eventMux.Subscribe(core.TxPreEvent{})
	go pm.txBroadcastLoop()
	// broadcast mined blocks
	pm.minedBlockSub = pm.eventMux.Subscribe(core.NewMinedBlockEvent{})
	go pm.minedBroadcastLoop()

	// start sync handlers
	go pm.syncer()
	go pm.txsyncLoop()
}

func (pm *ProtocolManager) Stop() {
	glog.V(logger.Info).Infoln("Stopping ethereum protocol handler...")

	pm.txSub.Unsubscribe()         // quits txBroadcastLoop
	pm.minedBlockSub.Unsubscribe() // quits blockBroadcastLoop

	// Quit the sync loop.
	// After this send has completed, no new peers will be accepted.
	pm.noMorePeers <- struct{}{}

	// Quit fetcher, txsyncLoop.
	close(pm.quitSync)

	// Disconnect existing sessions.
	// This also closes the gate for any new registrations on the peer set.
	// sessions which are already established but not added to pm.peers yet
	// will exit when they try to register.
	pm.peers.Close()

	// Wait for all peer handler goroutines and the loops to come down.
	pm.wg.Wait()

	glog.V(logger.Info).Infoln("Ethereum protocol handler stopped")
}

func (pm *ProtocolManager) newPeer(pv int, p *p2p.Peer, rw p2p.MsgReadWriter) *peer {
	return newPeer(pv, p, newMeteredMsgWriter(rw))
}

// handle is the callback invoked to manage the life cycle of an eth peer. When
// this function terminates, the peer is disconnected.
func (pm *ProtocolManager) handle(p *peer) error {
	glog.V(logger.Debug).Infof("%v: peer connected [%s]", p, p.Name())

	// Execute the Ethereum handshake
	td, head, genesis := pm.blockchain.Status()
	if err := p.Handshake(pm.networkId, td, head, genesis); err != nil {
		glog.V(logger.Debug).Infof("%v: handshake failed: %v", p, err)
		return err
	}
	if rw, ok := p.rw.(*meteredMsgReadWriter); ok {
		rw.Init(p.version)
	}
	// Register the peer locally
	glog.V(logger.Detail).Infof("%v: adding peer", p)
	if err := pm.peers.Register(p); err != nil {
		glog.V(logger.Error).Infof("%v: addition failed: %v", p, err)
		return err
	}
	defer pm.removePeer(p.id)

	// Register the peer in the downloader. If the downloader considers it banned, we disconnect
	// TODO Causing error in tests
	if err := pm.downloader.RegisterPeer(p.id, p.version, p.Head,
		p.RequestHeadersByHash, p.RequestHeadersByNumber, p.RequestBodies,
		p.RequestReceipts, p.RequestNodeData); err != nil {
		return err
	}
	// Propagate existing transactions. new transactions appearing
	// after this will be sent via broadcasts.
	pm.syncTransactions(p)

	// Drop connections on opposite side of network split
	var fork *core.Fork
	for i := range pm.chainConfig.Forks {
		fork = pm.chainConfig.Forks[i]
		if _, height := p.Head(); height.Cmp(fork.Block) < 0 {
			break
		}
		if !fork.RequiredHash.IsEmpty() {
			// Request the peer's fork block header for extra-dat
			if err := p.RequestHeadersByNumber(fork.Block.Uint64(), 1, 0, false); err != nil {
				glog.V(logger.Warn).Infof("%v: error requesting headers by number ", p)
				return err
			}
			// Start a timer to disconnect if the peer doesn't reply in time
			p.timeout = time.AfterFunc((5 * time.Second), func() {
				glog.V(logger.Debug).Infof("%v: timed out fork-check, dropping", p)
				pm.removePeer(p.id)
			})
			// Make sure it's cleaned up if the peer dies off
			defer func() {
				if p.timeout != nil {
					p.timeout.Stop()
					p.timeout = nil
				}
			}()
		}
	}
	// main loop. handle incoming messages.
	for {
		if err := pm.handleMsg(p); err != nil {
			glog.V(logger.Debug).Infof("%v: message handling failed: %v", p, err)
			return err
		}
	}
}

// handleMsg is invoked whenever an inbound message is received from a remote
// peer. The remote connection is torn down upon returning any error.
func (pm *ProtocolManager) handleMsg(p *peer) error {
	// Read the next message from the remote peer, and ensure it's fully consumed
	msg, err := p.rw.ReadMsg()
	if err != nil {
		return err
	}
	if msg.Size > ProtocolMaxMsgSize {
		return errResp(ErrMsgTooLarge, "%v > %v", msg.Size, ProtocolMaxMsgSize)
	}
	defer msg.Discard()

	// Handle the message depending on its contents
	switch {
	case msg.Code == StatusMsg:
		// Status messages should never arrive after the handshake
		return errResp(ErrExtraStatusMsg, "uncontrolled status message")
	// Block header query, collect the requested headers and reply
	case p.version >= eth62 && msg.Code == GetBlockHeadersMsg:
		// Decode the complex header query
		var query getBlockHeadersData
		if err := msg.Decode(&query); err != nil {
			return errResp(ErrDecode, "%v: %v", msg, err)
		}
		hashMode := query.Origin.Hash != (common.Hash{})

		// Gather headers until the fetch or network limits is reached
		var (
			bytes   common.StorageSize
			headers []*types.Header
			unknown bool
		)
		for !unknown && len(headers) < int(query.Amount) && bytes < softResponseLimit && len(headers) < downloader.MaxHeaderFetch {
			// Retrieve the next header satisfying the query
			var origin *types.Header
			if hashMode {
				origin = pm.blockchain.GetHeader(query.Origin.Hash)
			} else {
				origin = pm.blockchain.GetHeaderByNumber(query.Origin.Number)
			}
			if origin == nil {
				break
			}
			headers = append(headers, origin)
			bytes += estHeaderRlpSize

			// Advance to the next header of the query
			switch {
			case query.Origin.Hash != (common.Hash{}) && query.Reverse:
				// Hash based traversal towards the genesis block
				for i := 0; i < int(query.Skip)+1; i++ {
					if header := pm.blockchain.GetHeader(query.Origin.Hash); header != nil {
						query.Origin.Hash = header.ParentHash
					} else {
						unknown = true
						break
					}
				}
			case query.Origin.Hash != (common.Hash{}) && !query.Reverse:
				// Hash based traversal towards the leaf block
				if header := pm.blockchain.GetHeaderByNumber(origin.Number.Uint64() + query.Skip + 1); header != nil {
					if pm.blockchain.GetBlockHashesFromHash(header.Hash(), query.Skip+1)[query.Skip] == query.Origin.Hash {
						query.Origin.Hash = header.Hash()
					} else {
						unknown = true
					}
				} else {
					unknown = true
				}
			case query.Reverse:
				// Number based traversal towards the genesis block
				if query.Origin.Number >= query.Skip+1 {
					query.Origin.Number -= (query.Skip + 1)
				} else {
					unknown = true
				}

			case !query.Reverse:
				// Number based traversal towards the leaf block
				query.Origin.Number += (query.Skip + 1)
			}
		}
		return p.SendBlockHeaders(headers)

	case p.version >= eth62 && msg.Code == BlockHeadersMsg:
		// A batch of headers arrived to one of our previous requests
		var headers []*types.Header
		if err := msg.Decode(&headers); err != nil {
			return errResp(ErrDecode, "msg %v: %v", msg, err)
		}
		// Good will assumption. Even if the peer is ahead of the fork check header but returns
		// empty header response, it might be that the peer is a light client which only keeps
		// the last 256 block headers. Besides it does not prevent network attacks. See #313 for
		// an explaination.
		if len(headers) == 0 && p.timeout != nil {
			// Disable the fork drop timeout
			p.timeout.Stop()
			p.timeout = nil
			return nil
		}
		// Filter out any explicitly requested headers, deliver the rest to the downloader
		filter := len(headers) == 1
		if filter {
			if p.timeout != nil {
				// Disable the fork drop timeout
				p.timeout.Stop()
				p.timeout = nil
			}
			if err := pm.chainConfig.HeaderCheck(headers[0]); err != nil {
				pm.removePeer(p.id)
				return err
			}
			// Irrelevant of the fork checks, send the header to the fetcher just in case
			headers = pm.fetcher.FilterHeaders(headers, time.Now())
		}
		if len(headers) > 0 || !filter {
			err := pm.downloader.DeliverHeaders(p.id, headers)
			if err != nil {
				glog.V(logger.Debug).Infoln(err)
			}
		}

	case p.version >= eth62 && msg.Code == GetBlockBodiesMsg:
		// Decode the retrieval message
		msgStream := rlp.NewStream(msg.Payload, uint64(msg.Size))
		if _, err := msgStream.List(); err != nil {
			return err
		}
		// Gather blocks until the fetch or network limits is reached
		var (
			hash   common.Hash
			bytes  int
			bodies []rlp.RawValue
		)
		for bytes < softResponseLimit && len(bodies) < downloader.MaxBlockFetch {
			// Retrieve the hash of the next block
			if err := msgStream.Decode(&hash); err == rlp.EOL {
				break
			} else if err != nil {
				return errResp(ErrDecode, "msg %v: %v", msg, err)
			}
			// Retrieve the requested block body, stopping if enough was found
			if data := pm.blockchain.GetBodyRLP(hash); len(data) != 0 {
				bodies = append(bodies, data)
				bytes += len(data)
			}
		}
		return p.SendBlockBodiesRLP(bodies)

	case p.version >= eth62 && msg.Code == BlockBodiesMsg:
		// A batch of block bodies arrived to one of our previous requests
		var request blockBodiesData
		if err := msg.Decode(&request); err != nil {
			return errResp(ErrDecode, "msg %v: %v", msg, err)
		}
		// Deliver them all to the downloader for queuing
		trasactions := make([][]*types.Transaction, len(request))
		uncles := make([][]*types.Header, len(request))

		for i, body := range request {
			trasactions[i] = body.Transactions
			uncles[i] = body.Uncles
		}
		// Filter out any explicitly requested bodies, deliver the rest to the downloader
		filter := len(trasactions) > 0 || len(uncles) > 0
		if filter {
			trasactions, uncles = pm.fetcher.FilterBodies(trasactions, uncles, time.Now())
		}
		if len(trasactions) > 0 || len(uncles) > 0 || !filter {
			err := pm.downloader.DeliverBodies(p.id, trasactions, uncles)
			if err != nil {
				glog.V(logger.Debug).Infoln(err)
			}
		}

	case p.version >= eth63 && msg.Code == GetNodeDataMsg:
		// Decode the retrieval message
		msgStream := rlp.NewStream(msg.Payload, uint64(msg.Size))
		if _, err := msgStream.List(); err != nil {
			return err
		}
		// Gather state data until the fetch or network limits is reached
		var (
			hash  common.Hash
			bytes int
			data  [][]byte
		)
		for bytes < softResponseLimit && len(data) < downloader.MaxStateFetch {
			// Retrieve the hash of the next state entry
			if err := msgStream.Decode(&hash); err == rlp.EOL {
				break
			} else if err != nil {
				return errResp(ErrDecode, "msg %v: %v", msg, err)
			}
			// Retrieve the requested state entry, stopping if enough was found
			if entry, err := pm.chaindb.Get(hash.Bytes()); err == nil {
				data = append(data, entry)
				bytes += len(entry)
			}
		}
		return p.SendNodeData(data)

	case p.version >= eth63 && msg.Code == NodeDataMsg:
		// A batch of node state data arrived to one of our previous requests
		var data [][]byte
		if err := msg.Decode(&data); err != nil {
			return errResp(ErrDecode, "msg %v: %v", msg, err)
		}
		// Deliver all to the downloader
		if err := pm.downloader.DeliverNodeData(p.id, data); err != nil {
			glog.V(logger.Debug).Infof("failed to deliver node state data: %v", err)
		}

	case p.version >= eth63 && msg.Code == GetReceiptsMsg:
		// Decode the retrieval message
		msgStream := rlp.NewStream(msg.Payload, uint64(msg.Size))
		if _, err := msgStream.List(); err != nil {
			return err
		}
		// Gather state data until the fetch or network limits is reached
		var (
			hash     common.Hash
			bytes    int
			receipts []rlp.RawValue
		)
		for bytes < softResponseLimit && len(receipts) < downloader.MaxReceiptFetch {
			// Retrieve the hash of the next block
			if err := msgStream.Decode(&hash); err == rlp.EOL {
				break
			} else if err != nil {
				return errResp(ErrDecode, "msg %v: %v", msg, err)
			}
			// Retrieve the requested block's receipts, skipping if unknown to us
			results := core.GetBlockReceipts(pm.chaindb, hash)
			if results == nil {
				if header := pm.blockchain.GetHeader(hash); header == nil || header.ReceiptHash != types.EmptyRootHash {
					continue
				}
			}
			// If known, encode and queue for response packet
			if encoded, err := rlp.EncodeToBytes(results); err != nil {
				glog.V(logger.Error).Infof("failed to encode receipt: %v", err)
			} else {
				receipts = append(receipts, encoded)
				bytes += len(encoded)
			}
		}
		return p.SendReceiptsRLP(receipts)

	case p.version >= eth63 && msg.Code == ReceiptsMsg:
		// A batch of receipts arrived to one of our previous requests
		var receipts [][]*types.Receipt
		if err := msg.Decode(&receipts); err != nil {
			return errResp(ErrDecode, "msg %v: %v", msg, err)
		}
		// Deliver all to the downloader
		if err := pm.downloader.DeliverReceipts(p.id, receipts); err != nil {
			glog.V(logger.Debug).Infof("failed to deliver receipts: %v", err)
		}

	case msg.Code == NewBlockHashesMsg:
		// Retrieve and deserialize the remote new block hashes notification
		type announce struct {
			Hash   common.Hash
			Number uint64
		}
		var announces = []announce{}

		if p.version < eth62 {
			// We're running the old protocol, make block number unknown (0)
			var hashes []common.Hash
			if err := msg.Decode(&hashes); err != nil {
				return errResp(ErrDecode, "%v: %v", msg, err)
			}
			for _, hash := range hashes {
				announces = append(announces, announce{hash, 0})
			}
		} else {
			// Otherwise extract both block hash and number
			var request newBlockHashesData
			if err := msg.Decode(&request); err != nil {
				return errResp(ErrDecode, "%v: %v", msg, err)
			}
			for _, block := range request {
				announces = append(announces, announce{block.Hash, block.Number})
			}
		}
		// Mark the hashes as present at the remote node
		for _, block := range announces {
			p.MarkBlock(block.Hash)
			p.SetHead(block.Hash, p.td)
		}
		// Schedule all the unknown hashes for retrieval
		unknown := make([]announce, 0, len(announces))
		for _, block := range announces {
			if !pm.blockchain.HasBlock(block.Hash) {
				unknown = append(unknown, block)
			}
		}
		for _, block := range unknown {
			// TODO Breaking /eth tests
			pm.fetcher.Notify(p.id, block.Hash, block.Number, time.Now(), p.RequestOneHeader, p.RequestBodies)
		}

	case msg.Code == NewBlockMsg:
		// Retrieve and decode the propagated block
		var request newBlockData
		if err := msg.Decode(&request); err != nil {
			return errResp(ErrDecode, "%v: %v", msg, err)
		}
		if err := request.Block.ValidateFields(); err != nil {
			return errResp(ErrDecode, "block validation %v: %v", msg, err)
		}
		request.Block.ReceivedAt = msg.ReceivedAt
		request.Block.ReceivedFrom = p

		// Mark the peer as owning the block and schedule it for import
		p.MarkBlock(request.Block.Hash())
		p.SetHead(request.Block.ParentHash(), request.TD)
		pm.fetcher.Enqueue(p.id, request.Block)

		// Assuming the block is importable by the peer, but possibly not yet done so,
		// calculate the head hash and TD that the peer truly must have.
		var (
			trueHead = request.Block.ParentHash()
			trueTD   = request.TD
		)
		// Update the peers total difficulty if better than the previous
		if _, td := p.Head(); trueTD.Cmp(td) > 0 {
			p.SetHead(trueHead, trueTD)

			// Schedule a sync if above ours. Note, this will not fire a sync for a gap of
			// a singe block (as the true TD is below the propagated block), however this
			// scenario should easily be covered by the fetcher.
			currentBlock := pm.blockchain.CurrentBlock()
			if trueTD.Cmp(pm.blockchain.GetTd(currentBlock.Hash())) > 0 {
				go pm.synchronise(p)
			}
		}

	case msg.Code == TxMsg:
		// Transactions arrived, make sure we have a valid and fresh chain to handle them
		if atomic.LoadUint32(&pm.synced) == 0 {
			break
		}
		// Transactions can be processed, parse all of them and deliver to the pool
		var txs []*types.Transaction
		if err := msg.Decode(&txs); err != nil {
			return errResp(ErrDecode, "msg %v: %v", msg, err)
		}
		for i, tx := range txs {
			// Validate and mark the remote transaction
			if tx == nil {
				return errResp(ErrDecode, "transaction %d is nil", i)
			}
			p.MarkTransaction(tx.Hash())
		}
		pm.txpool.AddTransactions(txs)

	default:
		return errResp(ErrInvalidMsgCode, "%v", msg.Code)
	}
	return nil
}

// BroadcastBlock will either propagate a block to a subset of it's peers, or
// will only announce it's availability (depending what's requested).
func (pm *ProtocolManager) BroadcastBlock(block *types.Block, propagate bool) {
	hash := block.Hash()
	peers := pm.peers.PeersWithoutBlock(hash)

	// If propagation is requested, send to a subset of the peer
	if propagate {
		// Calculate the TD of the block (it's not imported yet, so block.Td is not valid)
		var td *big.Int
		if parent := pm.blockchain.GetBlock(block.ParentHash()); parent != nil {
			td = new(big.Int).Add(block.Difficulty(), pm.blockchain.GetTd(block.ParentHash()))
		} else {
			glog.V(logger.Error).Infof("propagating dangling block #%d [%x]", block.NumberU64(), hash[:4])
			return
		}
		// Send the block to a subset of our peers
		transfer := peers[:int(math.Sqrt(float64(len(peers))))]
		for _, peer := range transfer {
			peer.SendNewBlock(block, td)
		}
		glog.V(logger.Detail).Infof("propagated block %x to %d peers in %v", hash[:4], len(transfer), time.Since(block.ReceivedAt))
	}
	// Otherwise if the block is indeed in our own chain, announce it
	if pm.blockchain.HasBlock(hash) {
		for _, peer := range peers {
			peer.SendNewBlockHashes([]common.Hash{hash}, []uint64{block.NumberU64()})
		}
		glog.V(logger.Detail).Infof("announced block %x to %d peers in %v", hash[:4], len(peers), time.Since(block.ReceivedAt))
	}
}

// BroadcastTx will propagate a transaction to all peers which are not known to
// already have the given transaction.
func (pm *ProtocolManager) BroadcastTx(hash common.Hash, tx *types.Transaction) {
	// Broadcast transaction to a batch of peers not knowing about it
	peers := pm.peers.PeersWithoutTx(hash)
	//FIXME include this again: peers = peers[:int(math.Sqrt(float64(len(peers))))]
	for _, peer := range peers {
		peer.SendTransactions(types.Transactions{tx})
	}
	glog.V(logger.Detail).Infoln("broadcast tx to", len(peers), "peers")
}

// Mined broadcast loop
func (self *ProtocolManager) minedBroadcastLoop() {
	// automatically stops if unsubscribe
	for obj := range self.minedBlockSub.Chan() {
		switch ev := obj.Data.(type) {
		case core.NewMinedBlockEvent:
			self.BroadcastBlock(ev.Block, true)  // First propagate block to peers
			self.BroadcastBlock(ev.Block, false) // Only then announce to the rest
		}
	}
}

func (self *ProtocolManager) txBroadcastLoop() {
	// automatically stops if unsubscribe
	for obj := range self.txSub.Chan() {
		event := obj.Data.(core.TxPreEvent)
		self.BroadcastTx(event.Tx.Hash(), event.Tx)
	}
}

// EthNodeInfo represents a short summary of the Ethereum sub-protocol metadata known
// about the host peer.
type EthNodeInfo struct {
	Network    int         `json:"network"`    // Ethereum network ID (1=Mainnet, 2=Morden)
	Difficulty *big.Int    `json:"difficulty"` // Total difficulty of the host's blockchain
	Genesis    common.Hash `json:"genesis"`    // SHA3 hash of the host's genesis block
	Head       common.Hash `json:"head"`       // SHA3 hash of the host's best owned block
}

// NodeInfo retrieves some protocol metadata about the running host node.
func (self *ProtocolManager) NodeInfo() *EthNodeInfo {
	return &EthNodeInfo{
		Network:    self.networkId,
		Difficulty: self.blockchain.GetTd(self.blockchain.CurrentBlock().Hash()),
		Genesis:    self.blockchain.Genesis().Hash(),
		Head:       self.blockchain.CurrentBlock().Hash(),
	}
}
