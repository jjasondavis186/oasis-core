// Package p2p implements the worker committee gossip network.
package p2p

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/libp2p/go-libp2p"
	pubsub "github.com/libp2p/go-libp2p-pubsub"
	pb "github.com/libp2p/go-libp2p-pubsub/pb"
	"github.com/libp2p/go-libp2p/core"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/p2p/net/conngater"
	"github.com/libp2p/go-libp2p/p2p/net/connmgr"
	"github.com/multiformats/go-multiaddr"
	manet "github.com/multiformats/go-multiaddr/net"
	"github.com/spf13/viper"

	"github.com/oasisprotocol/oasis-core/go/common"
	"github.com/oasisprotocol/oasis-core/go/common/cbor"
	"github.com/oasisprotocol/oasis-core/go/common/crypto/signature"
	"github.com/oasisprotocol/oasis-core/go/common/crypto/tuplehash"
	"github.com/oasisprotocol/oasis-core/go/common/identity"
	"github.com/oasisprotocol/oasis-core/go/common/logging"
	"github.com/oasisprotocol/oasis-core/go/common/node"
	"github.com/oasisprotocol/oasis-core/go/common/persistent"
	"github.com/oasisprotocol/oasis-core/go/common/version"
	consensus "github.com/oasisprotocol/oasis-core/go/consensus/api"
	"github.com/oasisprotocol/oasis-core/go/p2p/api"
	"github.com/oasisprotocol/oasis-core/go/p2p/peermgmt"
	"github.com/oasisprotocol/oasis-core/go/p2p/protocol"
	"github.com/oasisprotocol/oasis-core/go/p2p/rpc"
	registryAPI "github.com/oasisprotocol/oasis-core/go/registry/api"
	"github.com/oasisprotocol/oasis-core/go/worker/common/configparser"
)

const (
	// peersHighWatermarkDelta specifies how many peers after the maximum peer count is reached we
	// ask the connection manager to start pruning peers.
	peersHighWatermarkDelta = 30

	// seenMessagesTTL is the amount of time pubsub messages will be remembered as seen and any
	// duplicates will be dropped before propagation.
	seenMessagesTTL = 120 * time.Second

	// minTopicPeers is the minimum number of peers from the registry we want to have connected
	// for a topic.
	minTopicPeers = 10

	// totalTopicPeers is the number of peers we want to have connected for a topic.
	totalTopicPeers = 20
)

// messageIdContext is the domain separation context for computing message identifier hashes.
var messageIdContext = []byte("oasis-core/p2p: message id") // nolint: revive

var allowUnroutableAddresses bool

// DebugForceAllowUnroutableAddresses allows unroutable addresses.
func DebugForceAllowUnroutableAddresses() {
	allowUnroutableAddresses = true
}

// p2p is a peer-to-peer node using libp2p.
type p2p struct {
	sync.RWMutex

	ctx       context.Context
	ctxCancel context.CancelFunc
	quitCh    chan struct{}

	chainContext string
	signer       signature.Signer

	host   core.Host
	pubsub *pubsub.PubSub

	gater   *conngater.BasicConnectionGater
	peerMgr *peermgmt.PeerManager

	registerAddresses []multiaddr.Multiaddr
	topics            map[string]*topicHandler

	logger *logging.Logger
}

// Implements api.Service.
func (p *p2p) Cleanup() {
}

// Implements api.Service.
func (p *p2p) Name() string {
	return "worker p2p"
}

// Implements api.Service.
func (p *p2p) Start() error {
	// Unfortunately, we cannot start the host as libp2p starts everything on construction.
	// However, we can start everything else.
	p.peerMgr.Start()
	return nil
}

// Implements api.Service.
func (p *p2p) Stop() {
	defer close(p.quitCh)

	p.ctxCancel()

	var wg sync.WaitGroup
	defer wg.Wait()
	wg.Add(2)

	go func() {
		defer wg.Done()
		p.peerMgr.Stop() // This blocks until the manager stops.
	}()

	go func() {
		defer wg.Done()
		_ = p.host.Close() // This blocks until the host stops.
	}()
}

// Implements api.Service.
func (p *p2p) Quit() <-chan struct{} {
	return p.quitCh
}

// Implements api.Service.
func (p *p2p) GetStatus() *api.Status {
	protocols := make(map[core.ProtocolID]int)
	for _, protocol := range p.peerMgr.Protocols() {
		protocols[protocol] = p.peerMgr.NumProtocolPeers(protocol)
	}

	topics := make(map[string]int)
	for _, topic := range p.peerMgr.Topics() {
		topics[topic] = p.peerMgr.NumTopicPeers(topic)
	}

	return &api.Status{
		PubKey:         p.signer.Public(),
		PeerID:         p.host.ID(),
		Addresses:      p.Addresses(),
		NumPeers:       len(p.host.Network().Peers()),
		NumConnections: len(p.host.Network().Conns()),
		Protocols:      protocols,
		Topics:         topics,
	}
}

// Implements api.Service.
func (p *p2p) Addresses() []node.Address {
	var addrs []multiaddr.Multiaddr
	if len(p.registerAddresses) == 0 {
		addrs = p.host.Addrs()
	} else {
		addrs = p.registerAddresses
	}

	allowUnroutable := allowUnroutableAddresses

	var addresses []node.Address
	for _, v := range addrs {
		netAddr, err := manet.ToNetAddr(v)
		if err != nil {
			panic(err)
		}
		tcpAddr := (netAddr).(*net.TCPAddr)
		nodeAddr := node.Address{
			IP:   tcpAddr.IP,
			Port: int64(tcpAddr.Port),
			Zone: tcpAddr.Zone,
		}

		if err := registryAPI.VerifyAddress(nodeAddr, allowUnroutable); err != nil {
			continue
		}

		addresses = append(addresses, nodeAddr)
	}

	return addresses
}

// Implements api.Service.
func (p *p2p) Peers(runtimeID common.Namespace) []string {
	allPeers := p.pubsub.ListPeers(protocol.NewTopicKindCommitteeID(p.chainContext, runtimeID))
	allPeers = append(allPeers, p.pubsub.ListPeers(protocol.NewTopicKindTxID(p.chainContext, runtimeID))...)

	var peers []string
	peerMap := make(map[core.PeerID]bool)
	for _, peerID := range allPeers {
		if peerMap[peerID] {
			continue
		}
		peerMap[peerID] = true

		addrs := p.host.Peerstore().Addrs(peerID)
		if len(addrs) == 0 {
			continue
		}
		if reachableAddrs := filterGloballyReachableAddresses(addrs); len(reachableAddrs) > 0 {
			addrs = reachableAddrs
		}

		peers = append(peers, fmt.Sprintf("%s/p2p/%s", addrs[0].String(), peerID.Pretty()))
	}
	return peers
}

func filterGloballyReachableAddresses(addrs []multiaddr.Multiaddr) []multiaddr.Multiaddr {
	ret := make([]multiaddr.Multiaddr, 0, len(addrs))
	for _, addr := range addrs {
		// Ugh, this multiaddr stuff is extremely obnoxious to work with.
		addrStr, err := addr.ValueForProtocol(multiaddr.P_IP4)
		if err != nil {
			addrStr, err = addr.ValueForProtocol(multiaddr.P_IP6)
			if err != nil {
				continue
			}
		}

		ip := net.ParseIP(addrStr)
		if ip == nil {
			continue
		}
		if !common.IsProbablyGloballyReachable(ip) {
			continue
		}

		// I have no idea if multiaddr.Multiaddr copies correctly.
		addrCopy, err := multiaddr.NewMultiaddr(addr.String())
		if err != nil {
			continue
		}
		ret = append(ret, addrCopy)
	}
	return ret
}

// Implements api.Service.
func (p *p2p) Publish(ctx context.Context, topic string, msg interface{}) {
	rawMsg := cbor.Marshal(msg)

	p.RLock()
	defer p.RUnlock()

	h := p.topics[topic]
	if h == nil {
		p.logger.Error("attempted to publish message for unsupported topic",
			"topic", topic,
		)
		return
	}

	if err := h.tryPublishing(rawMsg); err != nil {
		h.logger.Error("failed to publish message to the network",
			"err", err,
		)
	}

	p.logger.Debug("published message",
		"topic", topic,
	)
}

// Implements api.Service.
func (p *p2p) RegisterHandler(topic string, handler api.Handler) {
	protocol.ValidateTopicID(topic)

	p.Lock()
	defer p.Unlock()

	if _, ok := p.topics[topic]; ok {
		panic(fmt.Sprintf("p2p: handler for topic '%s' already registered", topic))
	}

	h, err := newTopicHandler(p, topic, handler)
	if err != nil {
		panic(fmt.Sprintf("p2p: failed to initialize topic handler: %s", err))
	}
	p.topics[topic] = h
	_ = p.pubsub.RegisterTopicValidator(
		topic,
		h.topicMessageValidator,
		pubsub.WithValidatorConcurrency(viper.GetInt(CfgGossipsubValidateConcurrency)),
	)

	p.logger.Debug("registered new topic handler",
		"topic", topic,
	)

	p.peerMgr.RegisterTopic(topic, minTopicPeers, totalTopicPeers)
}

// Implements api.Service.
func (p *p2p) BlockPeer(peerID core.PeerID) {
	p.logger.Warn("blocking peer",
		"peer_id", peerID,
	)

	p.pubsub.BlacklistPeer(peerID)
	_ = p.gater.BlockPeer(peerID)
	_ = p.host.Network().ClosePeer(peerID)
}

// Implements api.Service.
func (p *p2p) RegisterProtocol(pid core.ProtocolID, min int, total int) {
	p.peerMgr.RegisterProtocol(pid, min, total)
}

// Implements api.Service.
func (p *p2p) Host() core.Host {
	return p.host
}

// Implements api.Service.
func (p *p2p) PeerManager() api.PeerManager {
	return p.peerMgr
}

// Implements api.Service.
func (p *p2p) RegisterProtocolServer(srv rpc.Server) {
	protocol.ValidateProtocolID(srv.Protocol())

	p.host.SetStreamHandler(srv.Protocol(), srv.HandleStream)

	p.logger.Info("registered protocol server",
		"protocol_id", srv.Protocol(),
	)
}

// Implements api.Service.
func (p *p2p) GetMinRepublishInterval() time.Duration {
	return seenMessagesTTL + 5*time.Second
}

func messageIdFn(pmsg *pb.Message) string { // nolint: revive
	// id := TupleHash[messageIdContext](topic, data)
	h := tuplehash.New256(32, messageIdContext)
	_, _ = h.Write([]byte(pmsg.GetTopic()))
	_, _ = h.Write(pmsg.Data)
	return string(h.Sum(nil))
}

// New creates a new P2P node.
func New(identity *identity.Identity, consensus consensus.Backend, store *persistent.CommonStore) (api.Service, error) {
	// Instantiate the libp2p host.
	addresses, err := configparser.ParseAddressList(viper.GetStringSlice(CfgRegistrationAddresses))
	if err != nil {
		return nil, err
	}
	port := uint16(viper.GetInt(CfgHostPort))

	var registerAddresses []multiaddr.Multiaddr
	for _, addr := range addresses {
		var mAddr multiaddr.Multiaddr
		mAddr, err = manet.FromNetAddr(addr.ToTCPAddr())
		if err != nil {
			return nil, err
		}
		registerAddresses = append(registerAddresses, mAddr)
	}

	sourceMultiAddr, _ := multiaddr.NewMultiaddr(
		fmt.Sprintf("/ip4/0.0.0.0/tcp/%d", port),
	)

	// Set up a connection manager so we can limit the number of connections.
	low := int(viper.GetUint32(CfgConnMgrMaxNumPeers))
	cm, err := connmgr.NewConnManager(
		low,
		low+peersHighWatermarkDelta,
		connmgr.WithGracePeriod(viper.GetDuration(CfgConnMgrPeerGracePeriod)),
	)
	if err != nil {
		return nil, fmt.Errorf("p2p: failed to create connection manager: %w", err)
	}

	// Set up a connection gater.
	cg, err := conngater.NewBasicConnectionGater(nil)
	if err != nil {
		return nil, fmt.Errorf("p2p: failed to create connection gater: %w", err)
	}

	// Block peers specified in the blacklist.
	blacklist := viper.GetStringSlice(CfgConnGaterBlockedPeerIPs)
	for _, blockedIP := range blacklist {
		parsedIP := net.ParseIP(blockedIP)
		if parsedIP == nil {
			return nil, fmt.Errorf("p2p: malformed blocked IP: %s", blockedIP)
		}

		if grr := cg.BlockAddr(parsedIP); grr != nil {
			return nil, fmt.Errorf("p2p: failed to block IP (%s): %w", blockedIP, err)
		}
	}

	// Maintain persistent peers.
	persistentPeers := make(map[core.PeerID]bool)
	persistentPeersAI := []peer.AddrInfo{}
	for _, pp := range viper.GetStringSlice(CfgConnMgrPersistentPeers) {
		// The persistent peer addresses are in the format pubkey@IP:port,
		// because we use a similar format elsewhere and it's easier for users
		// to understand than a multiaddr.

		var addr node.ConsensusAddress
		if grr := addr.UnmarshalText([]byte(pp)); grr != nil {
			return nil, fmt.Errorf("p2p: malformed persistent peer address (expected pubkey@IP:port): %w", grr)
		}

		pid, grr := api.PublicKeyToPeerID(addr.ID)
		if grr != nil {
			return nil, fmt.Errorf("p2p: invalid persistent peer public key (%s): %w", addr.ID, grr)
		}

		ma, grr := addr.Address.MultiAddress()
		if grr != nil {
			return nil, fmt.Errorf("p2p: failed to convert persistent peer address to multi address (%s): %w", addr, grr)
		}

		if _, exists := persistentPeers[pid]; exists {
			// If we already have this peer ID, append to its addresses.
			for _, p := range persistentPeersAI {
				if p.ID == pid {
					p.Addrs = append(p.Addrs, ma)
					break
				}
			}
		} else {
			// Fresh entry.
			ai := peer.AddrInfo{
				ID:    pid,
				Addrs: []multiaddr.Multiaddr{ma},
			}
			persistentPeersAI = append(persistentPeersAI, ai)
		}
		persistentPeers[pid] = true
		cm.Protect(pid, "")
	}

	// Create the P2P host.
	host, err := libp2p.New(
		libp2p.UserAgent(fmt.Sprintf("oasis-core/%s", version.SoftwareVersion)),
		libp2p.ListenAddrs(sourceMultiAddr),
		libp2p.Identity(api.SignerToPrivKey(identity.P2PSigner)),
		libp2p.ConnectionManager(cm),
		libp2p.ConnectionGater(cg),
	)
	if err != nil {
		return nil, fmt.Errorf("p2p: failed to initialize libp2p host: %w", err)
	}

	// Initialize the gossipsub router.
	ctx, ctxCancel := context.WithCancel(context.Background())
	pubsub, err := pubsub.NewGossipSub(
		ctx,
		host,
		pubsub.WithMessageSigning(true),
		pubsub.WithStrictSignatureVerification(true),
		pubsub.WithFloodPublish(true),
		pubsub.WithPeerExchange(true),
		pubsub.WithPeerOutboundQueueSize(viper.GetInt(CfgGossipsubPeerOutboundQueueSize)),
		pubsub.WithValidateQueueSize(viper.GetInt(CfgGossipsubValidateQueueSize)),
		pubsub.WithValidateThrottle(viper.GetInt(CfgGossipsubValidateThrottle)),
		pubsub.WithMessageIdFn(messageIdFn),
		pubsub.WithDirectPeers(persistentPeersAI),
		pubsub.WithSeenMessagesTTL(seenMessagesTTL),
	)
	if err != nil {
		ctxCancel()
		_ = host.Close()
		return nil, fmt.Errorf("p2p: failed to initialize libp2p gossipsub: %w", err)
	}

	chainContext, err := consensus.GetChainContext(ctx)
	if err != nil {
		ctxCancel()
		_ = host.Close()
		return nil, fmt.Errorf("p2p: failed to get consensus chain context: %w", err)
	}

	mgr := peermgmt.NewPeerManager(host, cg, pubsub, consensus, chainContext, store)

	p := &p2p{
		ctx:               ctx,
		ctxCancel:         ctxCancel,
		quitCh:            make(chan struct{}),
		chainContext:      chainContext,
		signer:            identity.P2PSigner,
		host:              host,
		gater:             cg,
		peerMgr:           mgr,
		pubsub:            pubsub,
		registerAddresses: registerAddresses,
		topics:            make(map[string]*topicHandler),
		logger:            logging.GetLogger("p2p"),
	}

	p.logger.Info("p2p host initialized",
		"address", fmt.Sprintf("%+v", host.Addrs()),
	)

	if len(blacklist) > 0 {
		p.logger.Info("p2p blacklist initialized",
			"num_blocked_peers", len(blacklist),
		)
	}

	return p, nil
}