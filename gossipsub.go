package pubsub

import (
	"context"
	"fmt"
	"math/rand"
	"time"

	pb "github.com/libp2p/go-libp2p-pubsub/pb"

	"github.com/libp2p/go-libp2p-core/host"
	"github.com/libp2p/go-libp2p-core/network"
	"github.com/libp2p/go-libp2p-core/peer"
	"github.com/libp2p/go-libp2p-core/peerstore"
	"github.com/libp2p/go-libp2p-core/protocol"
	"github.com/libp2p/go-libp2p-core/routing"
)

const (
	GossipSubID_v10 = protocol.ID("/meshsub/1.0.0")
	GossipSubID_v11 = protocol.ID("/meshsub/1.1.0")
)

var (
	// overlay parameters
	GossipSubD   = 6
	GossipSubDlo = 4
	GossipSubDhi = 12

	// gossip parameters
	GossipSubHistoryLength = 5
	GossipSubHistoryGossip = 3

	// heartbeat interval
	GossipSubHeartbeatInitialDelay = 100 * time.Millisecond
	GossipSubHeartbeatInterval     = 1 * time.Second

	// fanout ttl
	GossipSubFanoutTTL = 60 * time.Second

	// number of peers to include in prune Peer eXchange
	GossipSubPrunePeers = 16

	// backoff time for pruned peers
	GossipSubPruneBackoff = time.Minute

	// number of active connection attempts for peers obtained through px
	GossipSubConnectors = 16

	// maximum number of pending connections for peers attempted through px
	GossipSubMaxPendingConnections = 1024

	// timeout for connection attempts
	GossipSubConnectionTimeout = 30 * time.Second
)

// NewGossipSub returns a new PubSub object using GossipSubRouter as the router.
func NewGossipSub(ctx context.Context, h host.Host, opts ...Option) (*PubSub, error) {
	rt := &GossipSubRouter{
		peers:   make(map[peer.ID]protocol.ID),
		mesh:    make(map[string]map[peer.ID]struct{}),
		fanout:  make(map[string]map[peer.ID]struct{}),
		lastpub: make(map[string]int64),
		gossip:  make(map[peer.ID][]*pb.ControlIHave),
		control: make(map[peer.ID]*pb.ControlMessage),
		backoff: make(map[string]map[peer.ID]time.Time),
		connect: make(chan connectInfo, GossipSubMaxPendingConnections),
		mcache:  NewMessageCache(GossipSubHistoryGossip, GossipSubHistoryLength),
	}
	return NewPubSub(ctx, h, rt, opts...)
}

// GossipSubRouter is a router that implements the gossipsub protocol.
// For each topic we have joined, we maintain an overlay through which
// messages flow; this is the mesh map.
// For each topic we publish to without joining, we maintain a list of peers
// to use for injecting our messages in the overlay with stable routes; this
// is the fanout map. Fanout peer lists are expired if we don't publish any
// messages to their topic for GossipSubFanoutTTL.
type GossipSubRouter struct {
	p       *PubSub
	peers   map[peer.ID]protocol.ID          // peer protocols
	mesh    map[string]map[peer.ID]struct{}  // topic meshes
	fanout  map[string]map[peer.ID]struct{}  // topic fanout
	lastpub map[string]int64                 // last publish time for fanout topics
	gossip  map[peer.ID][]*pb.ControlIHave   // pending gossip
	control map[peer.ID]*pb.ControlMessage   // pending control messages
	backoff map[string]map[peer.ID]time.Time // prune backoff
	connect chan connectInfo                 // px connection requests
	mcache  *MessageCache
	tracer  *pubsubTracer
}

type connectInfo struct {
	p   peer.ID
	srr *routing.SignedRoutingState
}

func (gs *GossipSubRouter) Protocols() []protocol.ID {
	return []protocol.ID{GossipSubID_v11, GossipSubID_v10, FloodSubID}
}

func (gs *GossipSubRouter) Attach(p *PubSub) {
	gs.p = p
	gs.tracer = p.tracer
	// start using the same msg ID function as PubSub for caching messages.
	gs.mcache.SetMsgIdFn(p.msgID)
	go gs.heartbeatTimer()
	for i := 0; i < GossipSubConnectors; i++ {
		go gs.connector()
	}
}

func (gs *GossipSubRouter) AddPeer(p peer.ID, proto protocol.ID) {
	log.Debugf("PEERUP: Add new peer %s using %s", p, proto)
	gs.tracer.AddPeer(p, proto)
	gs.peers[p] = proto
}

func (gs *GossipSubRouter) RemovePeer(p peer.ID) {
	log.Debugf("PEERDOWN: Remove disconnected peer %s", p)
	gs.tracer.RemovePeer(p)
	delete(gs.peers, p)
	for _, peers := range gs.mesh {
		delete(peers, p)
	}
	for _, peers := range gs.fanout {
		delete(peers, p)
	}
	delete(gs.gossip, p)
	delete(gs.control, p)
}

func (gs *GossipSubRouter) EnoughPeers(topic string, suggested int) bool {
	// check all peers in the topic
	tmap, ok := gs.p.topics[topic]
	if !ok {
		return false
	}

	fsPeers, gsPeers := 0, 0
	// floodsub peers
	for p := range tmap {
		if gs.peers[p] == FloodSubID {
			fsPeers++
		}
	}

	// gossipsub peers
	gsPeers = len(gs.mesh[topic])

	if suggested == 0 {
		suggested = GossipSubDlo
	}

	if fsPeers+gsPeers >= suggested || gsPeers >= GossipSubDhi {
		return true
	}

	return false
}

func (gs *GossipSubRouter) HandleRPC(rpc *RPC) {
	ctl := rpc.GetControl()
	if ctl == nil {
		return
	}

	iwant := gs.handleIHave(rpc.from, ctl)
	ihave := gs.handleIWant(rpc.from, ctl)
	prune := gs.handleGraft(rpc.from, ctl)
	gs.handlePrune(rpc.from, ctl)

	if len(iwant) == 0 && len(ihave) == 0 && len(prune) == 0 {
		return
	}

	out := rpcWithControl(ihave, nil, iwant, nil, prune)
	gs.sendRPC(rpc.from, out)
}

func (gs *GossipSubRouter) handleIHave(p peer.ID, ctl *pb.ControlMessage) []*pb.ControlIWant {
	iwant := make(map[string]struct{})

	for _, ihave := range ctl.GetIhave() {
		topic := ihave.GetTopicID()
		_, ok := gs.mesh[topic]
		if !ok {
			continue
		}

		for _, mid := range ihave.GetMessageIDs() {
			if gs.p.seenMessage(mid) {
				continue
			}
			iwant[mid] = struct{}{}
		}
	}

	if len(iwant) == 0 {
		return nil
	}

	log.Debugf("IHAVE: Asking for %d messages from %s", len(iwant), p)

	iwantlst := make([]string, 0, len(iwant))
	for mid := range iwant {
		iwantlst = append(iwantlst, mid)
	}

	return []*pb.ControlIWant{&pb.ControlIWant{MessageIDs: iwantlst}}
}

func (gs *GossipSubRouter) handleIWant(p peer.ID, ctl *pb.ControlMessage) []*pb.Message {
	ihave := make(map[string]*pb.Message)
	for _, iwant := range ctl.GetIwant() {
		for _, mid := range iwant.GetMessageIDs() {
			msg, ok := gs.mcache.Get(mid)
			if ok {
				ihave[mid] = msg
			}
		}
	}

	if len(ihave) == 0 {
		return nil
	}

	log.Debugf("IWANT: Sending %d messages to %s", len(ihave), p)

	msgs := make([]*pb.Message, 0, len(ihave))
	for _, msg := range ihave {
		msgs = append(msgs, msg)
	}

	return msgs
}

func (gs *GossipSubRouter) handleGraft(p peer.ID, ctl *pb.ControlMessage) []*pb.ControlPrune {
	var prune []string
	for _, graft := range ctl.GetGraft() {
		topic := graft.GetTopicID()
		peers, ok := gs.mesh[topic]
		if !ok {
			prune = append(prune, topic)
		} else {
			log.Debugf("GRAFT: Add mesh link from %s in %s", p, topic)
			gs.tracer.Graft(p, topic)
			peers[p] = struct{}{}
			gs.tagPeer(p, topic)
		}
	}

	if len(prune) == 0 {
		return nil
	}

	cprune := make([]*pb.ControlPrune, 0, len(prune))
	for _, topic := range prune {
		cprune = append(cprune, gs.makePrune(p, topic))
	}

	return cprune
}

func (gs *GossipSubRouter) handlePrune(p peer.ID, ctl *pb.ControlMessage) {
	for _, prune := range ctl.GetPrune() {
		topic := prune.GetTopicID()
		peers, ok := gs.mesh[topic]
		if ok {
			log.Debugf("PRUNE: Remove mesh link to %s in %s", p, topic)
			gs.tracer.Prune(p, topic)
			delete(peers, p)
			gs.untagPeer(p, topic)
			gs.addBackoff(p, topic)
			px := prune.GetPeers()
			if len(px) > 0 {
				gs.pxConnect(px)
			}
		}
	}
}

func (gs *GossipSubRouter) addBackoff(p peer.ID, topic string) {
	backoff, ok := gs.backoff[topic]
	if !ok {
		backoff = make(map[peer.ID]time.Time)
		gs.backoff[topic] = backoff
	}
	backoff[p] = time.Now().Add(GossipSubPruneBackoff)
}

func (gs *GossipSubRouter) pxConnect(peers []*pb.PeerInfo) {
	if len(peers) > GossipSubPrunePeers {
		shufflePeerInfo(peers)
		peers = peers[:GossipSubPrunePeers]
	}

	toconnect := make([]connectInfo, 0, len(peers))

	for _, pi := range peers {
		p := peer.ID(pi.PeerID)

		_, connected := gs.peers[p]
		if connected {
			continue
		}

		var srr *routing.SignedRoutingState
		var err error
		if pi.SignedAddrs != nil {
			// the peer sent us a signed record; ensure that it is valid
			srr, err = routing.UnmarshalSignedRoutingState(pi.SignedAddrs)
			if err != nil {
				log.Warningf("error unmarshalling routing record obtained through px: %s", err)
				continue
			}
			if srr.PeerID != p {
				log.Warningf("bogus routing record obtained through px: peer ID %s doesn't match expected peer %s", srr.PeerID, p)
				continue
			}
		}

		toconnect = append(toconnect, connectInfo{p, srr})
	}

	if len(toconnect) == 0 {
		return
	}

	for _, ci := range toconnect {
		select {
		case gs.connect <- ci:
		default:
			log.Debugf("ignoring peer connection attempt; too many pending connections")
			break
		}
	}
}

func (gs *GossipSubRouter) connector() {
	for {
		select {
		case ci := <-gs.connect:
			if gs.p.host.Network().Connectedness(ci.p) == network.Connected {
				continue
			}

			log.Debugf("connecting to %s", ci.p)
			if ci.srr != nil {
				gs.p.host.Peerstore().AddCertifiedAddrs(ci.srr, peerstore.TempAddrTTL)
			}

			ctx, cancel := context.WithTimeout(gs.p.ctx, GossipSubConnectionTimeout)
			err := gs.p.host.Connect(ctx, peer.AddrInfo{ID: ci.p})
			cancel()
			if err != nil {
				log.Debugf("error connecting to %s: %s", ci.p, err)
			}

		case <-gs.p.ctx.Done():
			return
		}
	}
}

func (gs *GossipSubRouter) Publish(from peer.ID, msg *pb.Message) {
	gs.mcache.Put(msg)

	tosend := make(map[peer.ID]struct{})
	for _, topic := range msg.GetTopicIDs() {
		// any peers in the topic?
		tmap, ok := gs.p.topics[topic]
		if !ok {
			continue
		}

		// floodsub peers
		for p := range tmap {
			if gs.peers[p] == FloodSubID {
				tosend[p] = struct{}{}
			}
		}

		// gossipsub peers
		gmap, ok := gs.mesh[topic]
		if !ok {
			// we are not in the mesh for topic, use fanout peers
			gmap, ok = gs.fanout[topic]
			if !ok || len(gmap) == 0 {
				// we don't have any, pick some
				peers := gs.getPeers(topic, GossipSubD, func(peer.ID) bool { return true })

				if len(peers) > 0 {
					gmap = peerListToMap(peers)
					gs.fanout[topic] = gmap
				}
			}
			gs.lastpub[topic] = time.Now().UnixNano()
		}

		for p := range gmap {
			tosend[p] = struct{}{}
		}
	}

	out := rpcWithMessages(msg)
	for pid := range tosend {
		if pid == from || pid == peer.ID(msg.GetFrom()) {
			continue
		}

		gs.sendRPC(pid, out)
	}
}

func (gs *GossipSubRouter) Join(topic string) {
	gmap, ok := gs.mesh[topic]
	if ok {
		return
	}

	log.Debugf("JOIN %s", topic)
	gs.tracer.Join(topic)

	gmap, ok = gs.fanout[topic]
	if ok {
		if len(gmap) < GossipSubD {
			// we need more peers; eager, as this would get fixed in the next heartbeat
			more := gs.getPeers(topic, GossipSubD-len(gmap), func(p peer.ID) bool {
				// filter our current peers
				_, ok := gmap[p]
				return !ok
			})
			for _, p := range more {
				gmap[p] = struct{}{}
			}
		}
		gs.mesh[topic] = gmap
		delete(gs.fanout, topic)
		delete(gs.lastpub, topic)
	} else {
		peers := gs.getPeers(topic, GossipSubD, func(peer.ID) bool { return true })
		gmap = peerListToMap(peers)
		gs.mesh[topic] = gmap
	}

	for p := range gmap {
		log.Debugf("JOIN: Add mesh link to %s in %s", p, topic)
		gs.tracer.Graft(p, topic)
		gs.sendGraft(p, topic)
		gs.tagPeer(p, topic)
	}
}

func (gs *GossipSubRouter) Leave(topic string) {
	gmap, ok := gs.mesh[topic]
	if !ok {
		return
	}

	log.Debugf("LEAVE %s", topic)
	gs.tracer.Leave(topic)

	delete(gs.mesh, topic)

	for p := range gmap {
		log.Debugf("LEAVE: Remove mesh link to %s in %s", p, topic)
		gs.tracer.Prune(p, topic)
		gs.sendPrune(p, topic)
		gs.untagPeer(p, topic)
	}
}

func (gs *GossipSubRouter) sendGraft(p peer.ID, topic string) {
	graft := []*pb.ControlGraft{&pb.ControlGraft{TopicID: &topic}}
	out := rpcWithControl(nil, nil, nil, graft, nil)
	gs.sendRPC(p, out)
}

func (gs *GossipSubRouter) sendPrune(p peer.ID, topic string) {
	prune := []*pb.ControlPrune{gs.makePrune(p, topic)}
	out := rpcWithControl(nil, nil, nil, nil, prune)
	gs.sendRPC(p, out)
}

func (gs *GossipSubRouter) sendRPC(p peer.ID, out *RPC) {
	// do we own the RPC?
	own := false

	// piggyback control message retries
	ctl, ok := gs.control[p]
	if ok {
		out = copyRPC(out)
		own = true
		gs.piggybackControl(p, out, ctl)
		delete(gs.control, p)
	}

	// piggyback gossip
	ihave, ok := gs.gossip[p]
	if ok {
		if !own {
			out = copyRPC(out)
			own = true
		}
		gs.piggybackGossip(p, out, ihave)
		delete(gs.gossip, p)
	}

	mch, ok := gs.p.peers[p]
	if !ok {
		return
	}

	select {
	case mch <- out:
		gs.tracer.SendRPC(out, p)
	default:
		log.Infof("dropping message to peer %s: queue full", p)
		gs.tracer.DropRPC(out, p)
		// push control messages that need to be retried
		ctl := out.GetControl()
		if ctl != nil {
			gs.pushControl(p, ctl)
		}
	}
}

func (gs *GossipSubRouter) heartbeatTimer() {
	time.Sleep(GossipSubHeartbeatInitialDelay)
	select {
	case gs.p.eval <- gs.heartbeat:
	case <-gs.p.ctx.Done():
		return
	}

	ticker := time.NewTicker(GossipSubHeartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			select {
			case gs.p.eval <- gs.heartbeat:
			case <-gs.p.ctx.Done():
				return
			}
		case <-gs.p.ctx.Done():
			return
		}
	}
}

func (gs *GossipSubRouter) heartbeat() {
	defer log.EventBegin(gs.p.ctx, "heartbeat").Done()

	// flush pending control message from retries and gossip
	// that hasn't been piggybacked since the last heartbeat
	gs.flush()

	tograft := make(map[peer.ID][]string)
	toprune := make(map[peer.ID][]string)

	// clean up expired backoffs
	gs.clearBackoff()

	// maintain the mesh for topics we have joined
	for topic, peers := range gs.mesh {

		// do we have enough peers?
		if len(peers) < GossipSubDlo {
			backoff := gs.backoff[topic]
			ineed := GossipSubD - len(peers)
			plst := gs.getPeers(topic, ineed, func(p peer.ID) bool {
				// filter our current peers and peers we are backing off
				_, inMesh := peers[p]
				_, doBackoff := backoff[p]
				return !inMesh && !doBackoff
			})

			for _, p := range plst {
				log.Debugf("HEARTBEAT: Add mesh link to %s in %s", p, topic)
				gs.tracer.Graft(p, topic)
				peers[p] = struct{}{}
				gs.tagPeer(p, topic)
				topics := tograft[p]
				tograft[p] = append(topics, topic)
			}
		}

		// do we have too many peers?
		if len(peers) > GossipSubDhi {
			idontneed := len(peers) - GossipSubD
			plst := peerMapToList(peers)
			shufflePeers(plst)

			for _, p := range plst[:idontneed] {
				log.Debugf("HEARTBEAT: Remove mesh link to %s in %s", p, topic)
				gs.tracer.Prune(p, topic)
				delete(peers, p)
				gs.untagPeer(p, topic)
				topics := toprune[p]
				toprune[p] = append(topics, topic)
			}
		}

		// 2nd arg are mesh peers excluded from gossip. We already push
		// messages to them, so its redundant to gossip IHAVEs.
		gs.emitGossip(topic, peers)
	}

	// expire fanout for topics we haven't published to in a while
	now := time.Now().UnixNano()
	for topic, lastpub := range gs.lastpub {
		if lastpub+int64(GossipSubFanoutTTL) < now {
			delete(gs.fanout, topic)
			delete(gs.lastpub, topic)
		}
	}

	// maintain our fanout for topics we are publishing but we have not joined
	for topic, peers := range gs.fanout {
		// check whether our peers are still in the topic
		for p := range peers {
			_, ok := gs.p.topics[topic][p]
			if !ok {
				delete(peers, p)
			}
		}

		// do we need more peers?
		if len(peers) < GossipSubD {
			ineed := GossipSubD - len(peers)
			plst := gs.getPeers(topic, ineed, func(p peer.ID) bool {
				// filter our current peers
				_, ok := peers[p]
				return !ok
			})

			for _, p := range plst {
				peers[p] = struct{}{}
			}
		}

		// 2nd arg are fanout peers excluded from gossip. We already push
		// messages to them, so its redundant to gossip IHAVEs.
		gs.emitGossip(topic, peers)
	}

	// send coalesced GRAFT/PRUNE messages (will piggyback gossip)
	gs.sendGraftPrune(tograft, toprune)

	// advance the message history window
	gs.mcache.Shift()
}

func (gs *GossipSubRouter) clearBackoff() {
	now := time.Now()
	for topic, backoff := range gs.backoff {
		for p, expire := range backoff {
			if expire.Before(now) {
				delete(backoff, p)
			}
		}
		if len(backoff) == 0 {
			delete(gs.backoff, topic)
		}
	}
}

func (gs *GossipSubRouter) sendGraftPrune(tograft, toprune map[peer.ID][]string) {
	for p, topics := range tograft {
		graft := make([]*pb.ControlGraft, 0, len(topics))
		for _, topic := range topics {
			graft = append(graft, &pb.ControlGraft{TopicID: &topic})
		}

		var prune []*pb.ControlPrune
		pruning, ok := toprune[p]
		if ok {
			delete(toprune, p)
			prune = make([]*pb.ControlPrune, 0, len(pruning))
			for _, topic := range pruning {
				prune = append(prune, gs.makePrune(p, topic))
			}
		}

		out := rpcWithControl(nil, nil, nil, graft, prune)
		gs.sendRPC(p, out)
	}

	for p, topics := range toprune {
		prune := make([]*pb.ControlPrune, 0, len(topics))
		for _, topic := range topics {
			prune = append(prune, gs.makePrune(p, topic))
		}

		out := rpcWithControl(nil, nil, nil, nil, prune)
		gs.sendRPC(p, out)
	}

}

// emitGossip emits IHAVE gossip advertising items in the message cache window
// of this topic.
func (gs *GossipSubRouter) emitGossip(topic string, exclude map[peer.ID]struct{}) {
	mids := gs.mcache.GetGossipIDs(topic)
	if len(mids) == 0 {
		return
	}

	// Send gossip to D peers, skipping over the exclude set.
	gpeers := gs.getPeers(topic, GossipSubD, func(p peer.ID) bool {
		_, ok := exclude[p]
		return !ok
	})

	// Emit the IHAVE gossip to the selected peers.
	for _, p := range gpeers {
		gs.enqueueGossip(p, &pb.ControlIHave{TopicID: &topic, MessageIDs: mids})
	}
}

func (gs *GossipSubRouter) flush() {
	// send gossip first, which will also piggyback control
	for p, ihave := range gs.gossip {
		delete(gs.gossip, p)
		out := rpcWithControl(nil, ihave, nil, nil, nil)
		gs.sendRPC(p, out)
	}

	// send the remaining control messages
	for p, ctl := range gs.control {
		delete(gs.control, p)
		out := rpcWithControl(nil, nil, nil, ctl.Graft, ctl.Prune)
		gs.sendRPC(p, out)
	}
}

func (gs *GossipSubRouter) enqueueGossip(p peer.ID, ihave *pb.ControlIHave) {
	gossip := gs.gossip[p]
	gossip = append(gossip, ihave)
	gs.gossip[p] = gossip
}

func (gs *GossipSubRouter) piggybackGossip(p peer.ID, out *RPC, ihave []*pb.ControlIHave) {
	ctl := out.GetControl()
	if ctl == nil {
		ctl = &pb.ControlMessage{}
		out.Control = ctl
	}

	ctl.Ihave = ihave
}

func (gs *GossipSubRouter) pushControl(p peer.ID, ctl *pb.ControlMessage) {
	// remove IHAVE/IWANT from control message, gossip is not retried
	ctl.Ihave = nil
	ctl.Iwant = nil
	if ctl.Graft != nil || ctl.Prune != nil {
		gs.control[p] = ctl
	}
}

func (gs *GossipSubRouter) piggybackControl(p peer.ID, out *RPC, ctl *pb.ControlMessage) {
	// check control message for staleness first
	var tograft []*pb.ControlGraft
	var toprune []*pb.ControlPrune

	for _, graft := range ctl.GetGraft() {
		topic := graft.GetTopicID()
		peers, ok := gs.mesh[topic]
		if !ok {
			continue
		}
		_, ok = peers[p]
		if ok {
			tograft = append(tograft, graft)
		}
	}

	for _, prune := range ctl.GetPrune() {
		topic := prune.GetTopicID()
		peers, ok := gs.mesh[topic]
		if !ok {
			toprune = append(toprune, prune)
			continue
		}
		_, ok = peers[p]
		if !ok {
			toprune = append(toprune, prune)
		}
	}

	if len(tograft) == 0 && len(toprune) == 0 {
		return
	}

	xctl := out.Control
	if xctl == nil {
		xctl = &pb.ControlMessage{}
		out.Control = xctl
	}

	if len(tograft) > 0 {
		xctl.Graft = append(xctl.Graft, tograft...)
	}
	if len(toprune) > 0 {
		xctl.Prune = append(xctl.Prune, toprune...)
	}
}

func (gs *GossipSubRouter) makePrune(p peer.ID, topic string) *pb.ControlPrune {
	if gs.peers[p] == GossipSubID_v10 {
		// GossipSub v1.0 -- no peer exchange, the peer won't be able to parse it anyway
		return &pb.ControlPrune{TopicID: &topic}
	}

	// select peers for Peer eXchange
	peers := gs.getPeers(topic, GossipSubPrunePeers, func(xp peer.ID) bool {
		return p != xp
	})

	px := make([]*pb.PeerInfo, 0, len(peers))
	for _, p := range peers {
		// see if we have a signed address record to send back; if we don't, just send
		// the peer ID and let the pruned peer find them in the DHT -- we can't trust
		// unsigned address records through px anyway.
		srr := gs.p.host.Peerstore().SignedRoutingState(p)
		var saddrs []byte
		var err error
		if srr != nil {
			saddrs, err = srr.Marshal()
			if err != nil {
				log.Warningf("error marshaling signed routing state for %s: %s", p, err)
			}
		}
		px = append(px, &pb.PeerInfo{PeerID: []byte(p), SignedAddrs: saddrs})
	}

	return &pb.ControlPrune{TopicID: &topic, Peers: px}
}

func (gs *GossipSubRouter) getPeers(topic string, count int, filter func(peer.ID) bool) []peer.ID {
	tmap, ok := gs.p.topics[topic]
	if !ok {
		return nil
	}

	peers := make([]peer.ID, 0, len(tmap))
	for p := range tmap {
		if (gs.peers[p] == GossipSubID_v10 || gs.peers[p] == GossipSubID_v11) && filter(p) {
			peers = append(peers, p)
		}
	}

	shufflePeers(peers)

	if count > 0 && len(peers) > count {
		peers = peers[:count]
	}

	return peers
}

func (gs *GossipSubRouter) tagPeer(p peer.ID, topic string) {
	tag := topicTag(topic)
	gs.p.host.ConnManager().TagPeer(p, tag, 2)
}

func (gs *GossipSubRouter) untagPeer(p peer.ID, topic string) {
	tag := topicTag(topic)
	gs.p.host.ConnManager().UntagPeer(p, tag)
}

func topicTag(topic string) string {
	return fmt.Sprintf("pubsub:%s", topic)
}

func peerListToMap(peers []peer.ID) map[peer.ID]struct{} {
	pmap := make(map[peer.ID]struct{})
	for _, p := range peers {
		pmap[p] = struct{}{}
	}
	return pmap
}

func peerMapToList(peers map[peer.ID]struct{}) []peer.ID {
	plst := make([]peer.ID, 0, len(peers))
	for p := range peers {
		plst = append(plst, p)
	}
	return plst
}

func shufflePeers(peers []peer.ID) {
	for i := range peers {
		j := rand.Intn(i + 1)
		peers[i], peers[j] = peers[j], peers[i]
	}
}

func shufflePeerInfo(peers []*pb.PeerInfo) {
	for i := range peers {
		j := rand.Intn(i + 1)
		peers[i], peers[j] = peers[j], peers[i]
	}
}
