## Communication between peers and components within the blocksync reactor

Each peer has an open p2p channel. The number of total requests in flight is limited (`maxPendingRequests` initially set to `maxTotalRequesters`). Additionally, there is an upper limit on requests **per** peer (20). 

Once a node receives messages via the p2p channel, they are propagated further via the reactor's go channels. This section contains details on each of the open communication channels, their capacity and when they get activated. 

On startup, the reactor fires up four go routines:
1. Process requests
2. Pool routine
3. Handle block sync channel messages
4. Process peer updates

The pool routine picks out blocks form the block pool and processes them. It also checks whether we should switch to consensus if we are caught up. ToDo - change wording (Remove we, discuss whether the pool routing should be the one checking the condition for consensus). 


### Communication channels

`BlockSyncCh`: a p2p channel for sending/receiving requests to/from peers.  
- Channel id: 0x40
- Size of receive buffer: 1024 messages
- Size of send queue: 1000 messages
- Message size: maximum size of a block + size of proto block messages  (response message prefix and key size)

Messages processed via the channel: 

    - `BlockRequest {height int64, peerID types.NodeID}` : request block at height `height` from peer `peerID`.
    - `BlockResponse {block types.Block} `: Send `block` to peer that requested it.
    - `NoBlockResponse{height int64} `: Indicates that a peer does not have a block at `height`.
    - `StatusRequest {} `: Sent to a peer to request its status.
    - `StatusResponse {height int64, base int64} `: Send to a peer the lowest and heights height of blocks within it's store (`store.Height()`, `store.Base()`).

### Reactor channels

This section describes, per reactor component, the open channels and information they process. 

#### `BlockPool`
`requestsCh chan<- BlockRequest` The number of requests is capped by a fixed parameter `maxPendingRequestsPerPeer` (initially set to 20). 
errorsCh   chan<- peerError ; Channel buffer size is limited. 

The reactor sends a p2p block request once it receives a signal via this channel from the block pool. The block pool will first pick a peer (in round robin) and assign it this particular height. Once this is done, the reactor can request a block.

#### `bpRequester`
`gotBlock chan struct{}`; capped at 1; Here we simply register a received block and keep waiting for the reactor  to terminate or a redo request. 

**Note**. It is not clear why we need this. 

`redoCh   chan types.NodeID`; capped at 1 ; Signals the requester to redo a request for aparticular block after replacing the peer for this height

When a block is received by a requester, the requester does a number of checks on the received block. Before marking the block as available, the requester verifies the following:
- that we expected a block at the particular height.
- the block came from the peer we assigned to it. 
- the block is not nil

In the code there is the following ToDo listed: 
` // TODO: ensure that blocks come in order for each peer.` This needs further specification. 

If the checks pass, the `block` field of the requester is populated with the new block and i sthus made available to the blocksync reactor.

#### `Reactor`
`requestsCh chan BlockRequest` :size `maxTotalRequesters`
`errorsCh   chan peerError` : size `maxPeerErrBuffer`

`didProcessCh chan struct{}` : size `1`.
The channel is created within the pool routine of the reactor and is used to signal that the reactor should check the block pool for new blocks. A message is sent to the channel after a fixed timeout (`trySyncTicker`). As we need two blocks to verify one of them (this is more clearly defined in [verification](#./verification.md), if we miss only on of them, we will not wait for the sync timer to time out, but rather try quickly again until we fetch both. 

`switchToConsensusTicker`. In addition to the sync timeout, in the same routine, the reactor checks periodically whether the conditions to switch to consensus are fullfilled. 