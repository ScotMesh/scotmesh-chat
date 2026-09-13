package rns

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"sync"
	"time"
)

// AutoInterface — IPv6 link-local multicast peer discovery, mirroring
// upstream RNS/Interfaces/AutoInterface.py.
//
// This one is NOT spec-normative. SPEC §8 is explicit that the discovery
// announce body belongs to the AutoInterface protocol rather than to
// Reticulum, so everything here is mirrored from upstream source rather
// than derived from the spec, and the derivations are pinned by tests
// against values computed with upstream itself.
//
// Shape of the protocol: each peer periodically multicasts a discovery
// token to a group address derived from a shared group id. A peer that
// receives one learns the sender's link-local address and exchanges
// Reticulum packets with it over unicast UDP on the data port.
const (
	// DefaultDiscoveryPort is the UDP port discovery tokens go to.
	DefaultDiscoveryPort = 29716
	// DefaultDataPort carries the actual Reticulum packets.
	DefaultDataPort = 42671
	// DefaultGroupID is upstream's default; peers must share it to find
	// each other, and a different one partitions the mesh cleanly.
	DefaultGroupID = "reticulum"

	// PeeringTimeout drops a peer that has not announced within this
	// window (upstream PEERING_TIMEOUT).
	PeeringTimeout = 22 * time.Second

	// MulticastTemporaryAddressType / MulticastPermanentAddressType are
	// the flags nibble of the multicast address.
	MulticastTemporaryAddressType = "1"
	MulticastPermanentAddressType = "0"

	// MulticastScopeLink is the scope nibble for link-local.
	MulticastScopeLink = "2"
)

// AutoInterfaceGroupHash is SHA-256 of the group id — the seed for both
// the multicast address and the discovery token.
func AutoInterfaceGroupHash(groupID string) []byte {
	sum := sha256.Sum256([]byte(groupID))
	return sum[:]
}

// MulticastDiscoveryAddress derives the IPv6 multicast group from the
// group id, exactly as upstream does (AutoInterface.py:202-212):
//
//	"ff" || address_type || scope || ":0" then six ":%02x" groups built
//	from group_hash bytes [2..14] as big-endian 16-bit pairs.
//
// Note the first group is the literal "0", not a hash-derived value —
// upstream computes one and then discards it (the line is commented out
// in the source). Deriving it instead produces an address on which no
// upstream peer is listening.
func MulticastDiscoveryAddress(groupID, addressType, scope string) string {
	g := AutoInterfaceGroupHash(groupID)
	addr := "ff" + addressType + scope + ":0"
	for i := 3; i <= 13; i += 2 {
		addr += fmt.Sprintf(":%02x", int(g[i])+(int(g[i-1])<<8))
	}
	return addr
}

// DiscoveryToken is what a peer multicasts to announce itself:
// SHA-256(group_id || link_local_address_string)
// (AutoInterface.py:502-503).
//
// The address is hashed as its TEXT form, not its bytes. It doubles as
// proof the sender knows the group id and as a binding to the address
// they are announcing from, so a token replayed from a different address
// does not match what the receiver computes.
func DiscoveryToken(groupID, linkLocalAddress string) []byte {
	sum := sha256.Sum256([]byte(groupID + linkLocalAddress))
	return sum[:]
}

// autoPeer is one discovered neighbour.
type autoPeer struct {
	addr     string
	lastSeen time.Time
}

// AutoInterface discovers peers by IPv6 link-local multicast and
// exchanges Reticulum packets with them over unicast UDP.
type AutoInterface struct {
	groupID  string
	scope    string
	addrType string

	discoveryPort int
	dataPort      int

	inbox chan []byte
	done  chan struct{}

	mu    sync.Mutex
	peers map[string]*autoPeer

	closed  bool
	timeout time.Duration
}

// NewAutoInterface builds an interface for a group. It does no I/O:
// binding sockets is the caller's job via Start, so the discovery and
// peer-table logic can be exercised without a multicast-capable network.
func NewAutoInterface(groupID string) *AutoInterface {
	if groupID == "" {
		groupID = DefaultGroupID
	}
	return &AutoInterface{
		groupID:       groupID,
		scope:         MulticastScopeLink,
		addrType:      MulticastTemporaryAddressType,
		discoveryPort: DefaultDiscoveryPort,
		dataPort:      DefaultDataPort,
		inbox:         make(chan []byte, 128),
		done:          make(chan struct{}),
		peers:         map[string]*autoPeer{},
		timeout:       PeeringTimeout,
	}
}

// MulticastAddress is the group this interface discovers on.
func (a *AutoInterface) MulticastAddress() string {
	return MulticastDiscoveryAddress(a.groupID, a.addrType, a.scope)
}

// Token is the discovery token this interface announces from `addr`.
func (a *AutoInterface) Token(addr string) []byte {
	return DiscoveryToken(a.groupID, addr)
}

// HandleDiscovery processes an inbound discovery datagram from `addr`.
//
// The token must match what we compute for THAT address: it is the only
// thing separating a peer of ours from any other UDP sender that can
// reach the group, and binding it to the source address stops a token
// observed on the wire being replayed from somewhere else.
func (a *AutoInterface) HandleDiscovery(token []byte, addr string, now time.Time) bool {
	want := DiscoveryToken(a.groupID, addr)
	if len(token) != len(want) {
		return false
	}
	var diff byte
	for i := range want {
		diff |= want[i] ^ token[i]
	}
	if diff != 0 {
		return false
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	a.expireLocked(now)
	if p, ok := a.peers[addr]; ok {
		p.lastSeen = now
		return true
	}
	a.peers[addr] = &autoPeer{addr: addr, lastSeen: now}
	return true
}

func (a *AutoInterface) expireLocked(now time.Time) {
	for k, p := range a.peers {
		if now.Sub(p.lastSeen) > a.timeout {
			delete(a.peers, k)
		}
	}
}

// Peers returns the currently-live peer addresses.
func (a *AutoInterface) Peers(now time.Time) []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.expireLocked(now)
	out := make([]string, 0, len(a.peers))
	for addr := range a.peers {
		out = append(out, addr)
	}
	return out
}

// Send delivers a packet to every live peer over unicast UDP.
//
// Upstream spawns one interface per peer and lets the Transport
// broadcast; presenting one Interface and fanning out here is the same
// choice TCPServer makes, and for the same reason — the peer set is
// simpler to manage in one place than spread across Transport state.
func (a *AutoInterface) Send(packet []byte) error {
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return fmt.Errorf("auto interface closed")
	}
	a.expireLocked(time.Now())
	targets := make([]string, 0, len(a.peers))
	for addr := range a.peers {
		targets = append(targets, addr)
	}
	a.mu.Unlock()

	var failed int
	for _, addr := range targets {
		if err := a.sendTo(addr, packet); err != nil {
			failed++
		}
	}
	if failed > 0 {
		return fmt.Errorf("auto interface: %d of %d peers failed", failed, len(targets))
	}
	return nil
}

// sendTo is the unicast UDP write to one peer.
func (a *AutoInterface) sendTo(addr string, packet []byte) error {
	conn, err := net.DialTimeout("udp6", net.JoinHostPort(addr, fmt.Sprint(a.dataPort)), 2*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()
	_ = conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	_, err = conn.Write(packet)
	return err
}

// Deliver injects an inbound packet, for the data-socket reader.
func (a *AutoInterface) Deliver(packet []byte) {
	select {
	case a.inbox <- append([]byte(nil), packet...):
	case <-a.done:
	default:
		// Bounded like every other inbound path: a full inbox drops
		// rather than blocking whatever is pumping the socket.
	}
}

// Inbox returns inbound packets.
func (a *AutoInterface) Inbox() <-chan []byte { return a.inbox }

// Done is closed when the interface stops.
func (a *AutoInterface) Done() <-chan struct{} { return a.done }

// Close stops the interface. Idempotent.
func (a *AutoInterface) Close() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return nil
	}
	a.closed = true
	close(a.done)
	return nil
}

// PeerKey is the map key a peer address is tracked under, exposed for
// tests and diagnostics.
func PeerKey(addr string) string { return hex.EncodeToString([]byte(addr)) }

// Start binds the discovery and data sockets on `ifaceName` and begins
// announcing.
//
// This is the part that needs a real network: an IPv6 link-local
// multicast join on a specific interface. Everything above it —
// derivations, token verification, the peer table, fan-out — works
// without one and is tested; this is not.
func (a *AutoInterface) Start(ifaceName, linkLocalAddr string, announceEvery time.Duration) error {
	iface, err := net.InterfaceByName(ifaceName)
	if err != nil {
		return fmt.Errorf("interface %s: %w", ifaceName, err)
	}
	group := &net.UDPAddr{IP: net.ParseIP(a.MulticastAddress()), Port: a.discoveryPort}
	if group.IP == nil {
		return fmt.Errorf("derived multicast address %q is not parseable", a.MulticastAddress())
	}

	disc, err := net.ListenMulticastUDP("udp6", iface, group)
	if err != nil {
		return fmt.Errorf("join %s on %s: %w", group.IP, ifaceName, err)
	}
	data, err := net.ListenUDP("udp6", &net.UDPAddr{Port: a.dataPort})
	if err != nil {
		disc.Close()
		return fmt.Errorf("bind data port %d: %w", a.dataPort, err)
	}

	go a.discoveryLoop(disc)
	go a.dataLoop(data)
	go a.announceLoop(group, ifaceName, linkLocalAddr, announceEvery)

	go func() {
		<-a.done
		disc.Close()
		data.Close()
	}()
	return nil
}

func (a *AutoInterface) discoveryLoop(conn *net.UDPConn) {
	buf := make([]byte, 512)
	for {
		n, src, err := conn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		// The source address is what the token must bind to; strip any
		// zone suffix, since that is local to us rather than part of
		// what the peer hashed.
		addr := src.IP.String()
		if !a.HandleDiscovery(buf[:n], addr, time.Now()) {
			// Not a peer of ours: a different group, or a replay from
			// the wrong address. Silent — the multicast group is shared
			// with anyone who can reach it.
			continue
		}
	}
}

func (a *AutoInterface) dataLoop(conn *net.UDPConn) {
	buf := make([]byte, 2048)
	for {
		n, _, err := conn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		if n < header1MinLen {
			continue
		}
		a.Deliver(buf[:n])
	}
}

func (a *AutoInterface) announceLoop(group *net.UDPAddr, ifaceName, linkLocalAddr string, every time.Duration) {
	if every <= 0 {
		every = PeeringTimeout / 3
	}
	token := a.Token(linkLocalAddr)
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		conn, err := net.DialUDP("udp6", nil, group)
		if err == nil {
			_, _ = conn.Write(token)
			conn.Close()
		}
		select {
		case <-ticker.C:
		case <-a.done:
			return
		}
	}
}
