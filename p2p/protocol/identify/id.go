package identify

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/event"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/peerstore"
	"github.com/libp2p/go-libp2p/core/protocol"
	"github.com/libp2p/go-libp2p/core/record"
	"github.com/libp2p/go-libp2p/p2p/host/eventbus"
	"github.com/libp2p/go-libp2p/p2p/protocol/identify/pb"

	logging "github.com/ipfs/go-log/v2"
	"github.com/libp2p/go-msgio/pbio"
	ma "github.com/multiformats/go-multiaddr"
	manet "github.com/multiformats/go-multiaddr/net"
	msmux "github.com/multiformats/go-multistream"
	"google.golang.org/protobuf/proto"
)

//go:generate protoc --proto_path=$PWD:$PWD/../../.. --go_out=. --go_opt=Mpb/identify.proto=./pb pb/identify.proto

var log = logging.Logger("net/identify")

const (
	// ID is the protocol.ID of version 1.0.0 of the identify service.
	ID = "/ipfs/id/1.0.0"
	// IDPush is the protocol.ID of the Identify push protocol.
	// It sends full identify messages containing the current state of the peer.
	IDPush = "/ipfs/id/push/1.0.0"
)

const DefaultProtocolVersion = "ipfs/0.1.0"

const ServiceName = "libp2p.identify"

const maxPushConcurrency = 32

// StreamReadTimeout is the read timeout on all incoming Identify family streams.
var StreamReadTimeout = 60 * time.Second

const (
	legacyIDSize = 2 * 1024 // 2k Bytes
	signedIDSize = 8 * 1024 // 8K
	maxMessages  = 10
)

var defaultUserAgent = "github.com/libp2p/go-libp2p"

type identifySnapshot struct {
	timestamp time.Time
	protocols []protocol.ID
	addrs     []ma.Multiaddr
	record    *record.Envelope
}

type IDService interface {
	// IdentifyConn synchronously triggers an identify request on the connection and
	// waits for it to complete. If the connection is being identified by another
	// caller, this call will wait. If the connection has already been identified,
	// it will return immediately.
	IdentifyConn(network.Conn)
	// IdentifyWait triggers an identify (if the connection has not already been
	// identified) and returns a channel that is closed when the identify protocol
	// completes.
	IdentifyWait(network.Conn) <-chan struct{}
	// OwnObservedAddrs returns the addresses peers have reported we've dialed from
	OwnObservedAddrs() []ma.Multiaddr
	// ObservedAddrsFor returns the addresses peers have reported we've dialed from,
	// for a specific local address.
	ObservedAddrsFor(local ma.Multiaddr) []ma.Multiaddr
	Start()
	io.Closer
}

type identifyPushSupport uint8

const (
	identifyPushSupportUnknown identifyPushSupport = iota
	identifyPushSupported
	identifyPushUnsupported
)

type entry struct {
	// The IdentifyWaitChan is created when IdentifyWait is called for the first time.
	// IdentifyWait closes this channel when the Identify request completes, or when it fails.
	IdentifyWaitChan chan struct{}

	// PushSupport saves our knowledge about the peer's support of the Identify Push protocol.
	// Before the identify request returns, we don't know yet if the peer supports Identify Push.
	PushSupport identifyPushSupport
	// Timestamp is the time of the last snapshot we sent to this peer.
	Timestamp time.Time
}

// idService is a structure that implements ProtocolIdentify.
// It is a trivial service that gives the other peer some
// useful information about the local peer. A sort of hello.
//
// The idService sends:
//   - Our libp2p Protocol Version
//   - Our libp2p Agent Version
//   - Our public Listen Addresses
type idService struct {
	Host            host.Host
	UserAgent       string
	ProtocolVersion string

	ctx       context.Context
	ctxCancel context.CancelFunc
	// track resources that need to be shut down before we shut down
	refCount sync.WaitGroup

	disableSignedPeerRecord bool

	connsMu sync.RWMutex
	// The conns map contains all connections we're currently handling.
	// Connections are inserted as soon as they're available in the swarm, and - crucially -
	// before any stream can be opened or accepted on that connection.
	// Connections are removed from the map when the connection disconnects.
	// It is therefore safe to assume that a connection was (recently) closed if there's no entry in this map.
	conns map[network.Conn]entry

	addrMu sync.Mutex

	// our own observed addresses.
	observedAddrs *ObservedAddrManager

	emitters struct {
		evtPeerProtocolsUpdated        event.Emitter
		evtPeerIdentificationCompleted event.Emitter
		evtPeerIdentificationFailed    event.Emitter
	}

	currentSnapshot struct {
		sync.Mutex
		snapshot *identifySnapshot
	}

	pushSemaphore chan struct{} // makes sure that only a single push task is running at a time
}

// NewIDService constructs a new *idService and activates it by
// attaching its stream handler to the given host.Host.
func NewIDService(h host.Host, opts ...Option) (*idService, error) {
	var cfg config
	for _, opt := range opts {
		opt(&cfg)
	}

	userAgent := defaultUserAgent
	if cfg.userAgent != "" {
		userAgent = cfg.userAgent
	}

	protocolVersion := DefaultProtocolVersion
	if cfg.protocolVersion != "" {
		protocolVersion = cfg.protocolVersion
	}

	ctx, cancel := context.WithCancel(context.Background())
	s := &idService{
		Host:                    h,
		UserAgent:               userAgent,
		ProtocolVersion:         protocolVersion,
		ctx:                     ctx,
		ctxCancel:               cancel,
		conns:                   make(map[network.Conn]entry),
		disableSignedPeerRecord: cfg.disableSignedPeerRecord,
		pushSemaphore:           make(chan struct{}, 1),
	}

	observedAddrs, err := NewObservedAddrManager(h)
	if err != nil {
		return nil, fmt.Errorf("failed to create observed address manager: %s", err)
	}
	s.observedAddrs = observedAddrs

	s.emitters.evtPeerProtocolsUpdated, err = h.EventBus().Emitter(&event.EvtPeerProtocolsUpdated{})
	if err != nil {
		log.Warnf("identify service not emitting peer protocol updates; err: %s", err)
	}
	s.emitters.evtPeerIdentificationCompleted, err = h.EventBus().Emitter(&event.EvtPeerIdentificationCompleted{})
	if err != nil {
		log.Warnf("identify service not emitting identification completed events; err: %s", err)
	}
	s.emitters.evtPeerIdentificationFailed, err = h.EventBus().Emitter(&event.EvtPeerIdentificationFailed{})
	if err != nil {
		log.Warnf("identify service not emitting identification failed events; err: %s", err)
	}

	// register protocols that do not depend on peer records.
	h.SetStreamHandler(ID, s.handleIdentifyRequest)
	h.SetStreamHandler(IDPush, s.handlePush)

	return s, nil
}

func (ids *idService) Start() {
	ids.updateSnapshot()
	ids.Host.Network().Notify((*netNotifiee)(ids))
	ids.refCount.Add(1)
	go ids.loop(ids.ctx)
}

func (ids *idService) loop(ctx context.Context) {
	defer ids.refCount.Done()

	sub, err := ids.Host.EventBus().Subscribe(
		[]any{&event.EvtLocalProtocolsUpdated{}, &event.EvtLocalAddressesUpdated{}},
		eventbus.BufSize(256),
		eventbus.Name("identify (loop)"),
	)
	if err != nil {
		log.Errorf("failed to subscribe to events on the bus, err=%s", err)
		return
	}
	defer sub.Close()

	// Send pushes from a separate Go routine.
	// That way, we can end up with
	// * this Go routine busy looping over all peers in sendPushes
	// * another push being queued in the triggerPush channel
	triggerPush := make(chan struct{}, 1)
	ids.refCount.Add(1)
	go func() {
		defer ids.refCount.Done()

		for {
			select {
			case <-ctx.Done():
				return
			case <-triggerPush:
				ids.sendPushes(ctx)
			}
		}
	}()

	for {
		select {
		case <-sub.Out():
			ids.updateSnapshot()
			select {
			case triggerPush <- struct{}{}:
			default: // we already have one more push queued, no need to queue another one
			}
		case <-ctx.Done():
			return
		}
	}
}

func (ids *idService) sendPushes(ctx context.Context) {
	select {
	case ids.pushSemaphore <- struct{}{}:
	default:
		// another sendPushes call is currently running
		return
	}
	defer func() { <-ids.pushSemaphore }()

	ids.connsMu.RLock()
	conns := make([]network.Conn, 0, len(ids.conns))
	for c, e := range ids.conns {
		// Push even if we don't know if push is supported.
		// This will be only the case while the IdentifyWaitChan call is in flight.
		if e.PushSupport == identifyPushSupported || e.PushSupport == identifyPushSupportUnknown {
			conns = append(conns, c)
		}
	}
	ids.connsMu.RUnlock()

	sem := make(chan struct{}, maxPushConcurrency)
	var wg sync.WaitGroup
	for _, c := range conns {
		// check if the connection is still alive
		ids.connsMu.RLock()
		e, ok := ids.conns[c]
		ids.connsMu.RUnlock()
		if !ok {
			continue
		}
		// check if we already sent the current snapshot to this peer
		ids.currentSnapshot.Lock()
		snapshot := ids.currentSnapshot.snapshot
		ids.currentSnapshot.Unlock()
		if !e.Timestamp.Before(snapshot.timestamp) {
			log.Debugw("already sent this snapshot to peer", "peer", c.RemotePeer(), "timestamp", snapshot.timestamp)
			continue
		}
		// we haven't, send it now
		sem <- struct{}{}
		wg.Add(1)
		go func(c network.Conn) {
			defer wg.Done()
			defer func() { <-sem }()
			ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			str, err := ids.Host.NewStream(ctx, c.RemotePeer(), IDPush)
			if err != nil { // connection might have been closed recently
				return
			}
			// TODO: find out if the peer supports push if we didn't have any information about push support
			if err := ids.sendIdentifyResp(str); err != nil {
				log.Debugw("failed to send identify push", "peer", c.RemotePeer(), "error", err)
				return
			}
		}(c)
	}
	wg.Wait()
}

// Close shuts down the idService
func (ids *idService) Close() error {
	ids.ctxCancel()
	ids.observedAddrs.Close()
	ids.refCount.Wait()
	return nil
}

func (ids *idService) OwnObservedAddrs() []ma.Multiaddr {
	return ids.observedAddrs.Addrs()
}

func (ids *idService) ObservedAddrsFor(local ma.Multiaddr) []ma.Multiaddr {
	return ids.observedAddrs.AddrsFor(local)
}

// IdentifyConn runs the Identify protocol on a connection.
// It returns when we've received the peer's Identify message (or the request fails).
// If successful, the peer store will contain the peer's addresses and supported protocols.
func (ids *idService) IdentifyConn(c network.Conn) {
	<-ids.IdentifyWait(c)
}

// IdentifyWait runs the Identify protocol on a connection.
// It doesn't block and returns a channel that is closed when we receive
// the peer's Identify message (or the request fails).
// If successful, the peer store will contain the peer's addresses and supported protocols.
func (ids *idService) IdentifyWait(c network.Conn) <-chan struct{} {
	ids.connsMu.Lock()
	defer ids.connsMu.Unlock()

	e, found := ids.conns[c]
	if !found { // No entry found. Connection was most likely closed (and removed from this map) recently.
		ch := make(chan struct{})
		close(ch)
		return ch
	}

	if e.IdentifyWaitChan != nil {
		return e.IdentifyWaitChan
	}
	// First call to IdentifyWait for this connection. Create the channel.
	e.IdentifyWaitChan = make(chan struct{})
	ids.conns[c] = e

	// Spawn an identify. The connection may actually be closed
	// already, but that doesn't really matter. We'll fail to open a
	// stream then forget the connection.
	go func() {
		defer close(e.IdentifyWaitChan)
		if err := ids.identifyConn(c); err != nil {
			log.Warnf("failed to identify %s: %s", c.RemotePeer(), err)
			ids.emitters.evtPeerIdentificationFailed.Emit(event.EvtPeerIdentificationFailed{Peer: c.RemotePeer(), Reason: err})
			return
		}

		ids.emitters.evtPeerIdentificationCompleted.Emit(event.EvtPeerIdentificationCompleted{Peer: c.RemotePeer()})
	}()

	return e.IdentifyWaitChan
}

func (ids *idService) identifyConn(c network.Conn) error {
	s, err := c.NewStream(network.WithUseTransient(context.TODO(), "identify"))
	if err != nil {
		log.Debugw("error opening identify stream", "peer", c.RemotePeer(), "error", err)
		return err
	}

	if err := s.SetProtocol(ID); err != nil {
		log.Warnf("error setting identify protocol for stream: %s", err)
		s.Reset()
	}

	// ok give the response to our handler.
	if err := msmux.SelectProtoOrFail(ID, s); err != nil {
		log.Infow("failed negotiate identify protocol with peer", "peer", c.RemotePeer(), "error", err)
		s.Reset()
		return err
	}

	return ids.handleIdentifyResponse(s, false)
}

// handlePush handles incoming identify push streams
func (ids *idService) handlePush(s network.Stream) {
	ids.handleIdentifyResponse(s, true)
}

func (ids *idService) handleIdentifyRequest(s network.Stream) {
	_ = ids.sendIdentifyResp(s)
}

func (ids *idService) sendIdentifyResp(s network.Stream) error {
	if err := s.Scope().SetService(ServiceName); err != nil {
		s.Reset()
		return fmt.Errorf("failed to attaching stream to identify service: %w", err)
	}
	defer s.Close()

	ids.currentSnapshot.Lock()
	snapshot := ids.currentSnapshot.snapshot
	ids.currentSnapshot.Unlock()
	log.Debugf("%s sending message to %s %s", ID, s.Conn().RemotePeer(), s.Conn().RemoteMultiaddr())
	return ids.writeChunkedIdentifyMsg(s, snapshot)
}

func (ids *idService) handleIdentifyResponse(s network.Stream, isPush bool) error {
	if err := s.Scope().SetService(ServiceName); err != nil {
		log.Warnf("error attaching stream to identify service: %s", err)
		s.Reset()
		return err
	}

	if err := s.Scope().ReserveMemory(signedIDSize, network.ReservationPriorityAlways); err != nil {
		log.Warnf("error reserving memory for identify stream: %s", err)
		s.Reset()
		return err
	}
	defer s.Scope().ReleaseMemory(signedIDSize)

	_ = s.SetReadDeadline(time.Now().Add(StreamReadTimeout))

	c := s.Conn()

	r := pbio.NewDelimitedReader(s, signedIDSize)
	mes := &pb.Identify{}

	if err := readAllIDMessages(r, mes); err != nil {
		log.Warn("error reading identify message: ", err)
		s.Reset()
		return err
	}

	defer s.Close()

	log.Debugf("%s received message from %s %s", s.Protocol(), c.RemotePeer(), c.RemoteMultiaddr())

	ids.consumeMessage(mes, c, isPush)

	ids.connsMu.Lock()
	defer ids.connsMu.Unlock()
	e, ok := ids.conns[c]
	if !ok { // might already have disconnected
		return nil
	}
	sup, err := ids.Host.Peerstore().SupportsProtocols(c.RemotePeer(), IDPush)
	if supportsIdentifyPush := err == nil && len(sup) > 0; supportsIdentifyPush {
		e.PushSupport = identifyPushSupported
	} else {
		e.PushSupport = identifyPushUnsupported
	}
	ids.conns[c] = e
	return nil
}

func readAllIDMessages(r pbio.Reader, finalMsg proto.Message) error {
	mes := &pb.Identify{}
	for i := 0; i < maxMessages; i++ {
		switch err := r.ReadMsg(mes); err {
		case io.EOF:
			return nil
		case nil:
			proto.Merge(finalMsg, mes)
		default:
			return err
		}
	}

	return fmt.Errorf("too many parts")
}

func (ids *idService) updateSnapshot() {
	snapshot := &identifySnapshot{
		timestamp: time.Now(),
		addrs:     ids.Host.Addrs(),
		protocols: ids.Host.Mux().Protocols(),
	}
	if !ids.disableSignedPeerRecord {
		if cab, ok := peerstore.GetCertifiedAddrBook(ids.Host.Peerstore()); ok {
			snapshot.record = cab.GetPeerRecord(ids.Host.ID())
		}
	}

	ids.currentSnapshot.Lock()
	defer ids.currentSnapshot.Unlock()
	ids.currentSnapshot.snapshot = snapshot
}

func (ids *idService) writeChunkedIdentifyMsg(s network.Stream, snapshot *identifySnapshot) error {
	c := s.Conn()
	log.Debugw("sending snapshot with protocols", "protos", snapshot.protocols)

	mes := ids.createBaseIdentifyResponse(c, snapshot)
	sr := ids.getSignedRecord(snapshot)
	mes.SignedPeerRecord = sr
	writer := pbio.NewDelimitedWriter(s)

	if sr == nil || proto.Size(mes) <= legacyIDSize {
		return writer.WriteMsg(mes)
	}

	mes.SignedPeerRecord = nil
	if err := writer.WriteMsg(mes); err != nil {
		return err
	}
	// then write just the signed record
	return writer.WriteMsg(&pb.Identify{SignedPeerRecord: sr})
}

func (ids *idService) createBaseIdentifyResponse(conn network.Conn, snapshot *identifySnapshot) *pb.Identify {
	mes := &pb.Identify{}

	remoteAddr := conn.RemoteMultiaddr()
	localAddr := conn.LocalMultiaddr()

	// set protocols this node is currently handling
	mes.Protocols = protocol.ConvertToStrings(snapshot.protocols)

	// observed address so other side is informed of their
	// "public" address, at least in relation to us.
	mes.ObservedAddr = remoteAddr.Bytes()

	// populate unsigned addresses.
	// peers that do not yet support signed addresses will need this.
	// Note: LocalMultiaddr is sometimes 0.0.0.0
	viaLoopback := manet.IsIPLoopback(localAddr) || manet.IsIPLoopback(remoteAddr)
	mes.ListenAddrs = make([][]byte, 0, len(snapshot.addrs))
	for _, addr := range snapshot.addrs {
		if !viaLoopback && manet.IsIPLoopback(addr) {
			continue
		}
		mes.ListenAddrs = append(mes.ListenAddrs, addr.Bytes())
	}
	// set our public key
	ownKey := ids.Host.Peerstore().PubKey(ids.Host.ID())

	// check if we even have a public key.
	if ownKey == nil {
		// public key is nil. We are either using insecure transport or something erratic happened.
		// check if we're even operating in "secure mode"
		if ids.Host.Peerstore().PrivKey(ids.Host.ID()) != nil {
			// private key is present. But NO public key. Something bad happened.
			log.Errorf("did not have own public key in Peerstore")
		}
		// if neither of the key is present it is safe to assume that we are using an insecure transport.
	} else {
		// public key is present. Safe to proceed.
		if kb, err := crypto.MarshalPublicKey(ownKey); err != nil {
			log.Errorf("failed to convert key to bytes")
		} else {
			mes.PublicKey = kb
		}
	}

	// set protocol versions
	mes.ProtocolVersion = &ids.ProtocolVersion
	mes.AgentVersion = &ids.UserAgent

	return mes
}

func (ids *idService) getSignedRecord(snapshot *identifySnapshot) []byte {
	if ids.disableSignedPeerRecord || snapshot.record == nil {
		return nil
	}

	recBytes, err := snapshot.record.Marshal()
	if err != nil {
		log.Errorw("failed to marshal signed record", "err", err)
		return nil
	}

	return recBytes
}

// diff takes two slices of strings (a and b) and computes which elements were added and removed in b
func diff(a, b []protocol.ID) (added, removed []protocol.ID) {
	// This is O(n^2), but it's fine because the slices are small.
	for _, x := range b {
		var found bool
		for _, y := range a {
			if x == y {
				found = true
				break
			}
		}
		if !found {
			added = append(added, x)
		}
	}
	for _, x := range a {
		var found bool
		for _, y := range b {
			if x == y {
				found = true
				break
			}
		}
		if !found {
			removed = append(removed, x)
		}
	}
	return
}

func (ids *idService) consumeMessage(mes *pb.Identify, c network.Conn, isPush bool) {
	p := c.RemotePeer()

	supported, _ := ids.Host.Peerstore().GetProtocols(p)
	mesProtocols := protocol.ConvertFromStrings(mes.Protocols)
	added, removed := diff(supported, mesProtocols)
	ids.Host.Peerstore().SetProtocols(p, mesProtocols...)
	if isPush {
		ids.emitters.evtPeerProtocolsUpdated.Emit(event.EvtPeerProtocolsUpdated{
			Peer:    p,
			Added:   added,
			Removed: removed,
		})
	}

	// mes.ObservedAddr
	ids.consumeObservedAddress(mes.GetObservedAddr(), c)

	// mes.ListenAddrs
	laddrs := mes.GetListenAddrs()
	lmaddrs := make([]ma.Multiaddr, 0, len(laddrs))
	for _, addr := range laddrs {
		maddr, err := ma.NewMultiaddrBytes(addr)
		if err != nil {
			log.Debugf("%s failed to parse multiaddr from %s %s", ID,
				p, c.RemoteMultiaddr())
			continue
		}
		lmaddrs = append(lmaddrs, maddr)
	}

	// NOTE: Do not add `c.RemoteMultiaddr()` to the peerstore if the remote
	// peer doesn't tell us to do so. Otherwise, we'll advertise it.
	//
	// This can cause an "addr-splosion" issue where the network will slowly
	// gossip and collect observed but unadvertised addresses. Given a NAT
	// that picks random source ports, this can cause DHT nodes to collect
	// many undialable addresses for other peers.

	// add certified addresses for the peer, if they sent us a signed peer record
	// otherwise use the unsigned addresses.
	signedPeerRecord, err := signedPeerRecordFromMessage(mes)
	if err != nil {
		log.Errorf("error getting peer record from Identify message: %v", err)
	}

	// Extend the TTLs on the known (probably) good addresses.
	// Taking the lock ensures that we don't concurrently process a disconnect.
	ids.addrMu.Lock()
	ttl := peerstore.RecentlyConnectedAddrTTL
	if ids.Host.Network().Connectedness(p) == network.Connected {
		ttl = peerstore.ConnectedAddrTTL
	}

	// Downgrade connected and recently connected addrs to a temporary TTL.
	for _, ttl := range []time.Duration{
		peerstore.RecentlyConnectedAddrTTL,
		peerstore.ConnectedAddrTTL,
	} {
		ids.Host.Peerstore().UpdateAddrs(p, ttl, peerstore.TempAddrTTL)
	}

	// add signed addrs if we have them and the peerstore supports them
	cab, ok := peerstore.GetCertifiedAddrBook(ids.Host.Peerstore())
	if ok && signedPeerRecord != nil {
		_, addErr := cab.ConsumePeerRecord(signedPeerRecord, ttl)
		if addErr != nil {
			log.Debugf("error adding signed addrs to peerstore: %v", addErr)
		}
	} else {
		ids.Host.Peerstore().AddAddrs(p, lmaddrs, ttl)
	}

	// Finally, expire all temporary addrs.
	ids.Host.Peerstore().UpdateAddrs(p, peerstore.TempAddrTTL, 0)
	ids.addrMu.Unlock()

	log.Debugf("%s received listen addrs for %s: %s", c.LocalPeer(), c.RemotePeer(), lmaddrs)

	// get protocol versions
	pv := mes.GetProtocolVersion()
	av := mes.GetAgentVersion()

	ids.Host.Peerstore().Put(p, "ProtocolVersion", pv)
	ids.Host.Peerstore().Put(p, "AgentVersion", av)

	// get the key from the other side. we may not have it (no-auth transport)
	ids.consumeReceivedPubKey(c, mes.PublicKey)
}

func (ids *idService) consumeReceivedPubKey(c network.Conn, kb []byte) {
	lp := c.LocalPeer()
	rp := c.RemotePeer()

	if kb == nil {
		log.Debugf("%s did not receive public key for remote peer: %s", lp, rp)
		return
	}

	newKey, err := crypto.UnmarshalPublicKey(kb)
	if err != nil {
		log.Warnf("%s cannot unmarshal key from remote peer: %s, %s", lp, rp, err)
		return
	}

	// verify key matches peer.ID
	np, err := peer.IDFromPublicKey(newKey)
	if err != nil {
		log.Debugf("%s cannot get peer.ID from key of remote peer: %s, %s", lp, rp, err)
		return
	}

	if np != rp {
		// if the newKey's peer.ID does not match known peer.ID...

		if rp == "" && np != "" {
			// if local peerid is empty, then use the new, sent key.
			err := ids.Host.Peerstore().AddPubKey(rp, newKey)
			if err != nil {
				log.Debugf("%s could not add key for %s to peerstore: %s", lp, rp, err)
			}

		} else {
			// we have a local peer.ID and it does not match the sent key... error.
			log.Errorf("%s received key for remote peer %s mismatch: %s", lp, rp, np)
		}
		return
	}

	currKey := ids.Host.Peerstore().PubKey(rp)
	if currKey == nil {
		// no key? no auth transport. set this one.
		err := ids.Host.Peerstore().AddPubKey(rp, newKey)
		if err != nil {
			log.Debugf("%s could not add key for %s to peerstore: %s", lp, rp, err)
		}
		return
	}

	// ok, we have a local key, we should verify they match.
	if currKey.Equals(newKey) {
		return // ok great. we're done.
	}

	// weird, got a different key... but the different key MATCHES the peer.ID.
	// this odd. let's log error and investigate. this should basically never happen
	// and it means we have something funky going on and possibly a bug.
	log.Errorf("%s identify got a different key for: %s", lp, rp)

	// okay... does ours NOT match the remote peer.ID?
	cp, err := peer.IDFromPublicKey(currKey)
	if err != nil {
		log.Errorf("%s cannot get peer.ID from local key of remote peer: %s, %s", lp, rp, err)
		return
	}
	if cp != rp {
		log.Errorf("%s local key for remote peer %s yields different peer.ID: %s", lp, rp, cp)
		return
	}

	// okay... curr key DOES NOT match new key. both match peer.ID. wat?
	log.Errorf("%s local key and received key for %s do not match, but match peer.ID", lp, rp)
}

// HasConsistentTransport returns true if the address 'a' shares a
// protocol set with any address in the green set. This is used
// to check if a given address might be one of the addresses a peer is
// listening on.
func HasConsistentTransport(a ma.Multiaddr, green []ma.Multiaddr) bool {
	protosMatch := func(a, b []ma.Protocol) bool {
		if len(a) != len(b) {
			return false
		}

		for i, p := range a {
			if b[i].Code != p.Code {
				return false
			}
		}
		return true
	}

	protos := a.Protocols()

	for _, ga := range green {
		if protosMatch(protos, ga.Protocols()) {
			return true
		}
	}

	return false
}

func (ids *idService) consumeObservedAddress(observed []byte, c network.Conn) {
	if observed == nil {
		return
	}

	maddr, err := ma.NewMultiaddrBytes(observed)
	if err != nil {
		log.Debugf("error parsing received observed addr for %s: %s", c, err)
		return
	}

	ids.observedAddrs.Record(c, maddr)
}

func signedPeerRecordFromMessage(msg *pb.Identify) (*record.Envelope, error) {
	if msg.SignedPeerRecord == nil || len(msg.SignedPeerRecord) == 0 {
		return nil, nil
	}
	env, _, err := record.ConsumeEnvelope(msg.SignedPeerRecord, peer.PeerRecordEnvelopeDomain)
	return env, err
}

// netNotifiee defines methods to be used with the swarm
type netNotifiee idService

func (nn *netNotifiee) IDService() *idService {
	return (*idService)(nn)
}

func (nn *netNotifiee) Connected(_ network.Network, c network.Conn) {
	// We rely on this notification being received before we receive any incoming streams on the connection.
	// The swarm implementation guarantees this.
	ids := nn.IDService()
	ids.connsMu.Lock()
	ids.conns[c] = entry{}
	ids.connsMu.Unlock()

	nn.IDService().IdentifyWait(c)
}

func (nn *netNotifiee) Disconnected(_ network.Network, c network.Conn) {
	ids := nn.IDService()

	// Stop tracking the connection.
	ids.connsMu.Lock()
	delete(ids.conns, c)
	ids.connsMu.Unlock()

	if ids.Host.Network().Connectedness(c.RemotePeer()) != network.Connected {
		// Last disconnect.
		// Undo the setting of addresses to peer.ConnectedAddrTTL we did
		ids.addrMu.Lock()
		defer ids.addrMu.Unlock()
		ids.Host.Peerstore().UpdateAddrs(c.RemotePeer(), peerstore.ConnectedAddrTTL, peerstore.RecentlyConnectedAddrTTL)
	}
}

func (nn *netNotifiee) Listen(n network.Network, a ma.Multiaddr)      {}
func (nn *netNotifiee) ListenClose(n network.Network, a ma.Multiaddr) {}
