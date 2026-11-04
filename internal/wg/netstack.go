package wg

// TUN userspace sur la pile TCP/IP gVisor, dérivé de
// golang.zx2c4.com/wireguard/tun/netstack (licence MIT, WireGuard LLC).
//
// Deux différences avec l'original, mesurées par le banc de débit :
//   - lecture par lots : WireGuard reçoit jusqu'à batchSize paquets par appel
//     et les envoie en un seul sendmmsg, au lieu d'un appel système par paquet ;
//   - TCP réglé pour le débit : buffers auto-ajustés jusqu'à 8 Mio, CUBIC.
// Le résolveur DNS de l'original est retiré : Cercle ne joint que des IP.

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"sync"
	"syscall"

	"golang.zx2c4.com/wireguard/tun"
	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv6"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/icmp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
)

const (
	batchSize   = 128
	queueLength = 1024
	tcpBufMax   = 8 << 20
)

// netTUN relie WireGuard (côté tun.Device) à la pile gVisor (côté stack.Stack).
type netTUN struct {
	ep       *channel.Endpoint
	stack    *stack.Stack
	events   chan tun.Event
	notify   *channel.NotificationHandle
	incoming chan *buffer.View
	done     chan struct{}
	once     sync.Once
	mtu      int
	addr     netip.Addr
}

func newNetTUN(addr netip.Addr, mtu int) (*netTUN, error) {
	s := stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol, ipv6.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol, icmp.NewProtocol4, icmp.NewProtocol6},
		HandleLocal:        true,
	})
	t := &netTUN{
		ep:       channel.New(queueLength, uint32(mtu), ""),
		stack:    s,
		events:   make(chan tun.Event, 10),
		incoming: make(chan *buffer.View, queueLength),
		done:     make(chan struct{}),
		mtu:      mtu,
		addr:     addr,
	}
	sack := tcpip.TCPSACKEnabled(true)
	moderate := tcpip.TCPModerateReceiveBufferOption(true)
	rcv := tcpip.TCPReceiveBufferSizeRangeOption{Min: 4 << 10, Default: 1 << 20, Max: tcpBufMax}
	snd := tcpip.TCPSendBufferSizeRangeOption{Min: 4 << 10, Default: 1 << 20, Max: tcpBufMax}
	cc := tcpip.CongestionControlOption("cubic")
	for _, opt := range []tcpip.SettableTransportProtocolOption{&sack, &moderate, &rcv, &snd, &cc} {
		if err := s.SetTransportProtocolOption(tcp.ProtocolNumber, opt); err != nil {
			return nil, fmt.Errorf("option TCP %T : %v", opt, err)
		}
	}
	t.notify = t.ep.AddNotify(t)
	if err := s.CreateNIC(1, t.ep); err != nil {
		return nil, fmt.Errorf("création de la NIC : %v", err)
	}
	proto := tcpip.ProtocolAddress{
		Protocol:          ipv4.ProtocolNumber,
		AddressWithPrefix: tcpip.AddrFromSlice(addr.AsSlice()).WithPrefix(),
	}
	if !addr.Is4() {
		return nil, errors.New("adresse de tunnel IPv4 attendue")
	}
	if err := s.AddProtocolAddress(1, proto, stack.AddressProperties{}); err != nil {
		return nil, fmt.Errorf("ajout de l'adresse %v : %v", addr, err)
	}
	s.AddRoute(tcpip.Route{Destination: header.IPv4EmptySubnet, NIC: 1})
	t.events <- tun.EventUp
	return t, nil
}

func (t *netTUN) Name() (string, error)    { return "cercle", nil }
func (t *netTUN) File() *os.File           { return nil }
func (t *netTUN) Events() <-chan tun.Event { return t.events }
func (t *netTUN) MTU() (int, error)        { return t.mtu, nil }
func (t *netTUN) BatchSize() int           { return batchSize }

// Read attend un paquet sortant de la pile, puis prend ceux déjà en attente.
func (t *netTUN) Read(bufs [][]byte, sizes []int, offset int) (int, error) {
	var v *buffer.View
	select {
	case v = <-t.incoming:
	case <-t.done:
		return 0, os.ErrClosed
	}
	n := 0
	for {
		sz, err := v.Read(bufs[n][offset:])
		v.Release()
		if err != nil {
			return n, err
		}
		sizes[n] = sz
		n++
		if n == len(bufs) {
			return n, nil
		}
		select {
		case v = <-t.incoming:
		default:
			return n, nil
		}
	}
}

// Write injecte dans la pile les paquets déchiffrés par WireGuard.
func (t *netTUN) Write(bufs [][]byte, offset int) (int, error) {
	for _, b := range bufs {
		pkt := b[offset:]
		if len(pkt) == 0 {
			continue
		}
		pkb := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithData(pkt)})
		switch pkt[0] >> 4 {
		case 4:
			t.ep.InjectInbound(header.IPv4ProtocolNumber, pkb)
		case 6:
			t.ep.InjectInbound(header.IPv6ProtocolNumber, pkb)
		default:
			pkb.DecRef()
			return 0, syscall.EAFNOSUPPORT
		}
		pkb.DecRef()
	}
	return len(bufs), nil
}

// WriteNotify est appelé par gVisor quand un paquet sort de la pile.
func (t *netTUN) WriteNotify() {
	pkt := t.ep.Read()
	if pkt == nil {
		return
	}
	v := pkt.ToView()
	pkt.DecRef()
	select {
	case t.incoming <- v:
	case <-t.done:
		v.Release()
	}
}

func (t *netTUN) Close() error {
	t.once.Do(func() {
		close(t.done)
		t.stack.RemoveNIC(1)
		t.stack.Close()
		t.ep.RemoveNotify(t.notify)
		t.ep.Close()
		close(t.events)
	})
	return nil
}

func (t *netTUN) listenTCP(port uint16) (net.Listener, error) {
	return gonet.ListenTCP(t.stack, tcpip.FullAddress{NIC: 1, Addr: tcpip.AddrFromSlice(t.addr.AsSlice()), Port: port}, ipv4.ProtocolNumber)
}

func (t *netTUN) dialTCP(ctx context.Context, to netip.AddrPort) (net.Conn, error) {
	if !to.Addr().Is4() {
		return nil, fmt.Errorf("adresse de tunnel IPv4 attendue : %v", to)
	}
	return gonet.DialContextTCP(ctx, t.stack, tcpip.FullAddress{NIC: 1, Addr: tcpip.AddrFromSlice(to.Addr().AsSlice()), Port: to.Port()}, ipv4.ProtocolNumber)
}
