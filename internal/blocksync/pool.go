package blocksync

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tendermint/tendermint/internal/libs/flowrate"
	"github.com/tendermint/tendermint/libs/log"
	"github.com/tendermint/tendermint/libs/service"
	"github.com/tendermint/tendermint/types"
)

/*
eg, L = latency = 0.1s
	P = num peers = 10
	FN = num full nodes
	BS = 1kB block size
	CB = 1 Mbit/s = 128 kB/s
	CB/P = 12.8 kB
	B/S = CB/P/BS = 12.8 blocks/s

	12.8 * 0.1 = 1.28 blocks on conn
*/

const (
	requestIntervalMS         = 2
	maxTotalRequesters        = 600
	maxPeerErrBuffer          = 1000
	maxPendingRequests        = maxTotalRequesters
	maxPendingRequestsPerPeer = 20

	// Minimum recv rate to ensure we're receiving blocks from a peer fast
	// enough. If a peer is not sending us data at at least that rate, we
	// consider them to have timedout and we disconnect.
	//
	// Assuming a DSL connection (not a good choice) 128 Kbps (upload) ~ 15 KB/s,
	// sending data across atlantic ~ 7.5 KB/s.
	minRecvRate = 7680

	// Maximum difference between current and new block's height.
	maxDiffBetweenCurrentAndReceivedBlockHeight = 100
)

var peerTimeout = 15 * time.Second // not const so we can override with tests

/*
	Peers self report their heights when we join the block pool.
	Starting from our latest pool.height, we request blocks
	in sequence from peers that reported higher heights than ours.
	Every so often we ask peers what height they're on so we can keep going.

	Requests are continuously made for blocks of higher heights until
	the limit is reached. If most of the requests have no available peers, and we
	are not at peer limits, we can probably switch to consensus reactor
*/

// BlockRequest stores a block request identified by the block Height and the
// PeerID responsible for delivering the block.
type BlockRequest struct {
	Height int64
	PeerID types.NodeID
}

// request the header of a block at a certain height. Used to cross check
// the validated blocks with witnesses
type HeaderRequest struct {
	Height int64
	PeerID types.NodeID
}

// BlockPool keeps track of the block sync peers, block requests and block responses.
type BlockPool struct {
	service.BaseService
	logger log.Logger

	lastAdvance time.Time

	mtx sync.RWMutex
	// block requests
	requesters map[int64]*bpRequester
	// witness requesters
	//TODO we ideally want more than one witness per height
	witnessRequesters map[int64]*witnessRequester

	height int64 // the lowest key in requesters.
	// peers
	peers         map[types.NodeID]*bpPeer
	maxPeerHeight int64 // the biggest reported height

	// atomic
	numPending int32 // number of requests pending assignment or block response

	requestsCh chan<- BlockRequest
	//ToDO We essentially request a header here but reusing the same message
	// type for now
	witnessRequestsCh chan<- HeaderRequest
	errorsCh          chan<- peerError

	startHeight               int64
	lastHundredBlockTimeStamp time.Time
	lastSyncRate              float64
}

// NewBlockPool returns a new BlockPool with the height equal to start. Block
// requests and errors will be sent to requestsCh and errorsCh accordingly.
func NewBlockPool(
	logger log.Logger,
	start int64,
	requestsCh chan<- BlockRequest,
	errorsCh chan<- peerError,
	witnessRequestCh chan<- HeaderRequest,
) *BlockPool {

	bp := &BlockPool{
		logger:            logger,
		peers:             make(map[types.NodeID]*bpPeer),
		requesters:        make(map[int64]*bpRequester),
		witnessRequesters: make(map[int64]*witnessRequester),
		// verificationRequesters: make(map[int64]*bpRequester),
		height:            start,
		startHeight:       start,
		numPending:        0,
		requestsCh:        requestsCh,
		errorsCh:          errorsCh,
		witnessRequestsCh: witnessRequestCh,
		lastSyncRate:      0,
	}
	bp.BaseService = *service.NewBaseService(logger, "BlockPool", bp)
	return bp
}

// OnStart implements service.Service by spawning requesters routine and recording
// pool's start time.
func (pool *BlockPool) OnStart(ctx context.Context) error {
	pool.lastAdvance = time.Now()
	pool.lastHundredBlockTimeStamp = pool.lastAdvance
	go pool.makeRequestersRoutine(ctx)

	return nil
}

func (*BlockPool) OnStop() {}

// spawns requesters as needed
func (pool *BlockPool) makeRequestersRoutine(ctx context.Context) {
	for {
		if !pool.IsRunning() {
			break
		}

		_, numPending, lenRequesters := pool.GetStatus()
		switch {
		case numPending >= maxPendingRequests:
			// sleep for a bit.
			time.Sleep(requestIntervalMS * time.Millisecond)
			// check for timed out peers
			pool.removeTimedoutPeers()
		case lenRequesters >= maxTotalRequesters:
			// sleep for a bit.
			time.Sleep(requestIntervalMS * time.Millisecond)
			// check for timed out peers
			pool.removeTimedoutPeers()
		default:
			// request for more blocks.
			pool.makeNextRequester(ctx)
		}
	}
}

func (pool *BlockPool) removeTimedoutPeers() {
	pool.mtx.Lock()
	defer pool.mtx.Unlock()

	for _, peer := range pool.peers {
		// check if peer timed out
		if !peer.didTimeout && peer.numPending > 0 {
			curRate := peer.recvMonitor.CurrentTransferRate()
			// curRate can be 0 on start
			if curRate != 0 && curRate < minRecvRate {
				err := errors.New("peer is not sending us data fast enough")
				pool.sendError(err, peer.id)
				pool.logger.Error("SendTimeout", "peer", peer.id,
					"reason", err,
					"curRate", fmt.Sprintf("%d KB/s", curRate/1024),
					"minRate", fmt.Sprintf("%d KB/s", minRecvRate/1024))
				peer.didTimeout = true
			}
		}

		if peer.didTimeout {
			pool.removePeer(peer.id)
		}
	}
}

// GetStatus returns pool's height, numPending requests and the number of
// requesters.
func (pool *BlockPool) GetStatus() (height int64, numPending int32, lenRequesters int) {
	pool.mtx.RLock()
	defer pool.mtx.RUnlock()

	return pool.height, atomic.LoadInt32(&pool.numPending), len(pool.requesters)
}

// IsCaughtUp returns true if this node is caught up, false - otherwise.
func (pool *BlockPool) IsCaughtUp() bool {
	pool.mtx.RLock()
	defer pool.mtx.RUnlock()

	// Need at least 1 peer to be considered caught up.
	if len(pool.peers) == 0 {
		return false
	}

	// NOTE: we use maxPeerHeight - 1 because to sync block H requires block H+1
	// to verify the LastCommit.
	return pool.height >= (pool.maxPeerHeight - 1)
}

func (pool *BlockPool) PeekBlock() (first *types.Block) {
	pool.mtx.RLock()
	defer pool.mtx.RUnlock()

	if r := pool.requesters[pool.height]; r != nil {
		first = r.getBlock()
	}
	return
}

// PeekTwoBlocks returns blocks at pool.height and pool.height+1.
// We need to see the second block's Commit to validate the first block.
// So we peek two blocks at a time.
// The caller will verify the commit.
func (pool *BlockPool) PeekTwoBlocks() (first *types.Block, second *types.Block) {
	pool.mtx.RLock()
	defer pool.mtx.RUnlock()

	if r := pool.requesters[pool.height]; r != nil {
		first = r.getBlock()
	}
	if r := pool.requesters[pool.height+1]; r != nil {
		second = r.getBlock()
	}
	return
}

// PopRequest pops the first block at pool.height.
func (pool *BlockPool) PopRequest() {
	pool.mtx.Lock()
	defer pool.mtx.Unlock()

	if r := pool.requesters[pool.height]; r != nil {
		r.Stop()
		delete(pool.requesters, pool.height)
		delete(pool.witnessRequesters, pool.height)
		pool.height++
		pool.lastAdvance = time.Now()
		// the lastSyncRate will be updated every 100 blocks, it uses the adaptive filter
		// to smooth the block sync rate and the unit represents the number of blocks per second.
		// -1 because the start height is assumed to be 1  @jmalicevic ToDo, verify it is still OK when
		// starting height is not 1
		if (pool.height-pool.startHeight-1)%100 == 0 {
			newSyncRate := 100 / time.Since(pool.lastHundredBlockTimeStamp).Seconds()
			if pool.lastSyncRate == 0 {
				pool.lastSyncRate = newSyncRate
			} else {
				pool.lastSyncRate = 0.9*pool.lastSyncRate + 0.1*newSyncRate
			}
			pool.lastHundredBlockTimeStamp = time.Now()
		}

	} else {
		panic(fmt.Sprintf("Expected requester to pop, got nothing at height %v", pool.height))
	}
	// if r := pool.verificationRequesters[pool.height]; r != nil {
	// 	r.Stop()
	// 	delete(pool.verificationRequesters, pool.height)
	// }
}

// RedoRequest invalidates the block at pool.height,
// Remove the peer and redo request from others.
// Returns the ID of the removed peer.
func (pool *BlockPool) RedoRequest(height int64) types.NodeID {
	pool.mtx.Lock()
	defer pool.mtx.Unlock()

	request := pool.requesters[height]
	peerID := request.getPeerID()
	if peerID != types.NodeID("") {
		// RemovePeer will redo all requesters associated with this peer.
		pool.removePeer(peerID)
	}
	return peerID
}

func (pool *BlockPool) AddWitnessHeader(header *types.Header) {
	pool.mtx.Lock()
	defer pool.mtx.Unlock()
	requester := pool.witnessRequesters[header.Height]

	if requester == nil {
		pool.logger.Error("peer sent us a block we didn't expect")
		return
	}
	requester.SetBlock(header)
	peer := pool.peers[requester.peerID]
	if peer != nil {
		peer.decrPending(header.ToProto().Size())
	}

}

// AddBlock validates that the block comes from the peer it was expected from and calls the requester to store it.
// TODO: ensure that blocks come in order for each peer.
func (pool *BlockPool) AddBlock(peerID types.NodeID, block *types.Block, blockSize int) {
	pool.mtx.Lock()
	defer pool.mtx.Unlock()

	requester := pool.requesters[block.Height]
	if requester == nil {
		pool.logger.Error("peer sent us a block we didn't expect",
			"peer", peerID, "curHeight", pool.height, "blockHeight", block.Height)
		diff := pool.height - block.Height
		if diff < 0 {
			diff *= -1
		}
		if diff > maxDiffBetweenCurrentAndReceivedBlockHeight {
			pool.sendError(errors.New("peer sent us a block we didn't expect with a height too far ahead/behind"), peerID)
		}
		return
	}

	if requester.setBlock(block, peerID) {
		atomic.AddInt32(&pool.numPending, -1)
		peer := pool.peers[peerID]
		if peer != nil {
			peer.decrPending(blockSize)
		}
	} else {

		err := errors.New("requester is different or block already exists")
		pool.logger.Error(err.Error(), "peer", peerID, "requester", requester.getPeerID(), "blockHeight", block.Height)
		pool.sendError(err, peerID)

	}
}

// MaxPeerHeight returns the highest reported height.
func (pool *BlockPool) MaxPeerHeight() int64 {
	pool.mtx.RLock()
	defer pool.mtx.RUnlock()
	return pool.maxPeerHeight
}

// LastAdvance returns the time when the last block was processed (or start
// time if no blocks were processed).
func (pool *BlockPool) LastAdvance() time.Time {
	pool.mtx.RLock()
	defer pool.mtx.RUnlock()
	return pool.lastAdvance
}

// SetPeerRange sets the peer's alleged blockchain base and height.
func (pool *BlockPool) SetPeerRange(peerID types.NodeID, base int64, height int64) {
	pool.mtx.Lock()
	defer pool.mtx.Unlock()

	peer := pool.peers[peerID]
	if peer != nil {
		peer.base = base
		peer.height = height
	} else {
		peer = &bpPeer{
			pool:       pool,
			id:         peerID,
			base:       base,
			height:     height,
			numPending: 0,
			logger:     pool.logger.With("peer", peerID),
			startAt:    time.Now(),
		}

		pool.peers[peerID] = peer
	}

	if height > pool.maxPeerHeight {
		pool.maxPeerHeight = height
	}
}

// RemovePeer removes the peer with peerID from the pool. If there's no peer
// with peerID, function is a no-op.
func (pool *BlockPool) RemovePeer(peerID types.NodeID) {
	pool.mtx.Lock()
	defer pool.mtx.Unlock()

	pool.removePeer(peerID)
}

func (pool *BlockPool) removePeer(peerID types.NodeID) {
	for _, requester := range pool.requesters {
		if requester.getPeerID() == peerID {
			requester.redo(peerID)
		}

	}

	for _, requester := range pool.witnessRequesters {
		if requester.getPeerID() == peerID {
			requester.redo(peerID)
		}
	}
	peer, ok := pool.peers[peerID]
	if ok {
		if peer.timeout != nil {
			peer.timeout.Stop()
		}

		delete(pool.peers, peerID)

		// Find a new peer with the biggest height and update maxPeerHeight if the
		// peer's height was the biggest.
		if peer.height == pool.maxPeerHeight {
			pool.updateMaxPeerHeight()
		}
	}
}

// If no peers are left, maxPeerHeight is set to 0.
func (pool *BlockPool) updateMaxPeerHeight() {
	var max int64
	for _, peer := range pool.peers {
		if peer.height > max {
			max = peer.height
		}
	}
	pool.maxPeerHeight = max
}

func (pool *BlockPool) pickIncrAvailableWitness(height int64) *bpPeer {
	pool.mtx.Lock()
	defer pool.mtx.Unlock()

	for _, peer := range pool.peers {
		if peer.didTimeout {
			pool.removePeer(peer.id)
			continue
		}
		if peer.numPending >= maxPendingRequestsPerPeer {
			continue
		}
		if height < peer.base || height > peer.height || peer.id == pool.witnessRequesters[height].peerID {
			continue
		}
		peer.incrPending()

		return peer
	}
	return nil
}

// Pick an available peer with the given height available.
// If no peers are available, returns nil.
func (pool *BlockPool) pickIncrAvailablePeer(height int64) *bpPeer {
	pool.mtx.Lock()
	defer pool.mtx.Unlock()

	for _, peer := range pool.peers {
		if peer.didTimeout {
			pool.removePeer(peer.id)
			continue
		}
		if peer.numPending >= maxPendingRequestsPerPeer {
			continue
		}
		if height < peer.base || height > peer.height {
			continue
		}
		peer.incrPending()

		return peer
	}
	return nil
}

func (pool *BlockPool) makeNextRequester(ctx context.Context) {
	pool.mtx.Lock()
	defer pool.mtx.Unlock()

	nextHeight := pool.height + pool.requestersLen()
	if nextHeight > pool.maxPeerHeight {
		return
	}

	request := newBPRequester(pool.logger, pool, nextHeight)
	witnessRequester := newWitnessRequester(pool.logger, pool, nextHeight)
	witnessRequester.excludePeerID = request.peerID
	pool.requesters[nextHeight] = request
	pool.witnessRequesters[nextHeight] = witnessRequester
	atomic.AddInt32(&pool.numPending, 1)

	err := request.Start(ctx)
	if err != nil {
		request.logger.Error("error starting request", "err", err)
	}
	err = witnessRequester.Start(ctx)
	if err != nil {
		witnessRequester.logger.Error("error starting witness request", "err", err)
	}
}

func (pool *BlockPool) requestersLen() int64 {
	return int64(len(pool.requesters))
}

func (pool *BlockPool) sendRequest(height int64, peerID types.NodeID) {
	if !pool.IsRunning() {
		return
	}
	pool.requestsCh <- BlockRequest{height, peerID}
}

func (pool *BlockPool) sendWitnessRequest(height int64, peerID types.NodeID) {
	if !pool.IsRunning() {
		return
	}
	pool.witnessRequestsCh <- HeaderRequest{height, peerID}
}

func (pool *BlockPool) sendError(err error, peerID types.NodeID) {
	if !pool.IsRunning() {
		return
	}
	pool.errorsCh <- peerError{err, peerID}
}

// for debugging purposes
//nolint:unused
func (pool *BlockPool) debug() string {
	pool.mtx.Lock()
	defer pool.mtx.Unlock()

	str := ""
	nextHeight := pool.height + pool.requestersLen()
	for h := pool.height; h < nextHeight; h++ {
		if pool.requesters[h] == nil {
			str += fmt.Sprintf("H(%v):X ", h)
		} else {
			str += fmt.Sprintf("H(%v):", h)
			str += fmt.Sprintf("B?(%v) ", pool.requesters[h].block != nil)
		}
	}
	return str
}

func (pool *BlockPool) targetSyncBlocks() int64 {
	pool.mtx.RLock()
	defer pool.mtx.RUnlock()

	return pool.maxPeerHeight - pool.startHeight + 1
}

func (pool *BlockPool) getLastSyncRate() float64 {
	pool.mtx.RLock()
	defer pool.mtx.RUnlock()

	return pool.lastSyncRate
}

//-------------------------------------

type bpPeer struct {
	didTimeout  bool
	numPending  int32
	height      int64
	base        int64
	pool        *BlockPool
	id          types.NodeID
	recvMonitor *flowrate.Monitor

	timeout *time.Timer
	startAt time.Time

	logger log.Logger
}

func (peer *bpPeer) resetMonitor() {
	peer.recvMonitor = flowrate.New(peer.startAt, time.Second, time.Second*40)
	initialValue := float64(minRecvRate) * math.E
	peer.recvMonitor.SetREMA(initialValue)
}

func (peer *bpPeer) resetTimeout() {
	if peer.timeout == nil {
		peer.timeout = time.AfterFunc(peerTimeout, peer.onTimeout)
	} else {
		peer.timeout.Reset(peerTimeout)
	}
}

func (peer *bpPeer) incrPending() {
	if peer.numPending == 0 {
		peer.resetMonitor()
		peer.resetTimeout()
	}
	peer.numPending++
}

func (peer *bpPeer) decrPending(recvSize int) {
	peer.numPending--
	if peer.numPending == 0 {
		peer.timeout.Stop()
	} else {
		peer.recvMonitor.Update(recvSize)
		peer.resetTimeout()
	}
}

func (peer *bpPeer) onTimeout() {
	peer.pool.mtx.Lock()
	defer peer.pool.mtx.Unlock()

	err := errors.New("peer did not send us anything")
	peer.pool.sendError(err, peer.id)
	peer.logger.Error("SendTimeout", "reason", err, "timeout", peerTimeout)
	peer.didTimeout = true
}

//-------------------------------------

type BlockResponse struct {
	block  *types.Block
	commit *types.Commit
}

//-------------------------------------

type witnessRequester struct {
	service.BaseService
	peerID      types.NodeID
	header      *types.Header
	height      int64
	getHeaderCh chan struct{}
	redoCh      chan types.NodeID
	mtx         sync.Mutex
	// ID of peer we have already received this block from
	excludePeerID types.NodeID
	pool          *BlockPool
	logger        log.Logger
}

func newWitnessRequester(logger log.Logger, pool *BlockPool, height int64) *witnessRequester {
	wreq := &witnessRequester{
		logger:      pool.logger,
		pool:        pool,
		height:      height,
		getHeaderCh: make(chan struct{}, 1),
		redoCh:      make(chan types.NodeID),
		peerID:      "",
		header:      nil,
	}
	wreq.BaseService = *service.NewBaseService(logger, "witnessReqester", wreq)
	return wreq
}

func (wreq *witnessRequester) SetBlock(header *types.Header) bool {
	wreq.mtx.Lock()
	if wreq.header != nil { //|| wreq.peerID != peerID {
		wreq.mtx.Unlock()
		return false
	}
	wreq.header = header
	wreq.mtx.Unlock()

	select {
	case wreq.getHeaderCh <- struct{}{}:
	default:
	}
	return true
}

func (wreq *witnessRequester) OnStart(ctx context.Context) error {
	go wreq.requestRoutine(ctx)
	return nil
}
func (*witnessRequester) OnStop() {}

func (wreq *witnessRequester) getPeerID() types.NodeID {
	wreq.mtx.Lock()
	defer wreq.mtx.Unlock()
	return wreq.peerID
}

func (wreq *witnessRequester) requestRoutine(ctx context.Context) {
OUTER_LOOP:
	for {
		// Pick a peer to send request to.
		var peer *bpPeer
	PICK_PEER_LOOP:
		for {
			if !wreq.IsRunning() || !wreq.pool.IsRunning() {
				return
			}
			peer = wreq.pool.pickIncrAvailableWitness(wreq.height)
			if peer == nil {
				time.Sleep(requestIntervalMS * time.Millisecond)
				continue PICK_PEER_LOOP
			}
			break PICK_PEER_LOOP
		}
		wreq.mtx.Lock()
		wreq.peerID = peer.id
		wreq.mtx.Unlock()

		// Send request and wait.
		wreq.pool.sendWitnessRequest(wreq.height, peer.id)
	WAIT_LOOP:
		for {
			select {
			case <-ctx.Done():
				return
			case peerID := <-wreq.redoCh:
				if peerID == wreq.peerID {
					wreq.reset()
					continue OUTER_LOOP
				} else {
					continue WAIT_LOOP
				}
			case <-wreq.getHeaderCh:
				// We got a block!
				// Continue the for-loop and wait til Quit.
				continue WAIT_LOOP
			}
		}
	}
}

func (wreq *witnessRequester) redo(peerID types.NodeID) {
	select {
	case wreq.redoCh <- peerID:
	default:
	}
}

func (wreq *witnessRequester) reset() {
	wreq.mtx.Lock()
	defer wreq.mtx.Unlock()
	wreq.peerID = ""
	wreq.header = nil
}

//-------------------------------------

type bpRequester struct {
	service.BaseService
	logger     log.Logger
	pool       *BlockPool
	height     int64
	gotBlockCh chan struct{}
	redoCh     chan types.NodeID // redo may send multitime, add peerId to identify repeat

	mtx    sync.Mutex
	peerID types.NodeID
	block  *types.Block
}

func newBPRequester(logger log.Logger, pool *BlockPool, height int64) *bpRequester {
	bpr := &bpRequester{
		logger:     pool.logger,
		pool:       pool,
		height:     height,
		gotBlockCh: make(chan struct{}, 1),
		redoCh:     make(chan types.NodeID, 1),

		peerID: "",
		block:  nil,
	}
	bpr.BaseService = *service.NewBaseService(logger, "bpRequester", bpr)
	return bpr
}

func (bpr *bpRequester) OnStart(ctx context.Context) error {
	go bpr.requestRoutine(ctx)
	return nil
}

func (*bpRequester) OnStop() {}

// Returns true if the peer matches and block doesn't already exist.
func (bpr *bpRequester) setBlock(block *types.Block, peerID types.NodeID) bool {
	bpr.mtx.Lock()
	if bpr.block != nil || bpr.peerID != peerID {
		bpr.mtx.Unlock()
		return false
	}
	bpr.block = block
	bpr.mtx.Unlock()

	select {
	case bpr.gotBlockCh <- struct{}{}:
	default:
	}
	return true
}

func (bpr *bpRequester) getBlock() *types.Block {
	bpr.mtx.Lock()
	defer bpr.mtx.Unlock()
	return bpr.block
}

func (bpr *bpRequester) getPeerID() types.NodeID {
	bpr.mtx.Lock()
	defer bpr.mtx.Unlock()
	return bpr.peerID
}

// This is called from the requestRoutine, upon redo().
func (bpr *bpRequester) reset() {
	bpr.mtx.Lock()
	defer bpr.mtx.Unlock()

	if bpr.block != nil {
		atomic.AddInt32(&bpr.pool.numPending, 1)
	}

	bpr.peerID = ""
	bpr.block = nil
}

// Tells bpRequester to pick another peer and try again.
// NOTE: Nonblocking, and does nothing if another redo
// was already requested.
func (bpr *bpRequester) redo(peerID types.NodeID) {
	select {
	case bpr.redoCh <- peerID:
	default:
	}
}

// Responsible for making more requests as necessary
// Returns only when a block is found (e.g. AddBlock() is called)
func (bpr *bpRequester) requestRoutine(ctx context.Context) {
OUTER_LOOP:
	for {
		// Pick a peer to send request to.
		var peer *bpPeer
	PICK_PEER_LOOP:
		for {
			if !bpr.IsRunning() || !bpr.pool.IsRunning() {
				return
			}
			peer = bpr.pool.pickIncrAvailablePeer(bpr.height)
			if peer == nil {
				time.Sleep(requestIntervalMS * time.Millisecond)
				continue PICK_PEER_LOOP
			}
			break PICK_PEER_LOOP
		}
		bpr.mtx.Lock()
		bpr.peerID = peer.id
		bpr.mtx.Unlock()

		// Send request and wait.
		bpr.pool.sendRequest(bpr.height, peer.id)
	WAIT_LOOP:
		for {
			select {
			case <-ctx.Done():
				return
			case peerID := <-bpr.redoCh:
				if peerID == bpr.peerID {
					bpr.reset()
					continue OUTER_LOOP
				} else {
					continue WAIT_LOOP
				}
			case <-bpr.gotBlockCh:
				// We got a block!
				// Continue the for-loop and wait til Quit.
				continue WAIT_LOOP
			}
		}
	}
}
