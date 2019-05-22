// Copyright (C) 2019 The Syncthing Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at http://mozilla.org/MPL/2.0/.

package connections

import (
	"crypto/tls"
	"net"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/AudriusButkevicius/pfilter"
	"github.com/ccding/go-stun/stun"
	"github.com/lucas-clemente/quic-go"

	"github.com/syncthing/syncthing/lib/config"
	"github.com/syncthing/syncthing/lib/connections/registry"
	"github.com/syncthing/syncthing/lib/nat"
)

const stunRetryInterval = 5 * time.Minute

func init() {
	factory := &quicListenerFactory{}
	for _, scheme := range []string{"quic", "quic4", "quic6"} {
		listeners[scheme] = factory
	}
}

type quicListener struct {
	onAddressesChangedNotifier

	uri     *url.URL
	cfg     config.Wrapper
	tlsCfg  *tls.Config
	stop    chan struct{}
	conns   chan internalConn
	factory listenerFactory
	nat     atomic.Value

	address *url.URL
	err     error
	mut     sync.RWMutex
}

func (t *quicListener) Serve() {
	t.mut.Lock()
	t.err = nil
	t.mut.Unlock()

	network := strings.Replace(t.uri.Scheme, "quic", "udp", -1)

	packetConn, err := net.ListenPacket(network, t.uri.Host)
	if err != nil {
		t.mut.Lock()
		t.err = err
		t.mut.Unlock()
		l.Infoln("Listen (BEP/quic):", err)
		return
	}
	filterConn := pfilter.NewPacketFilter(packetConn)
	quicConn := filterConn.NewConn(quicFilterPriority, nil)
	stunConn := filterConn.NewConn(stunFilterPriority, &stunFilter{
		ids: make(map[string]time.Time),
	})

	filterConn.Start()
	registry.Register(t.uri.Scheme, quicConn)

	listener, err := quic.Listen(quicConn, t.tlsCfg, quicConfig)
	if err != nil {
		t.mut.Lock()
		t.err = err
		t.mut.Unlock()
		l.Infoln("Listen (BEP/quic):", err)
		return
	}

	defer listener.Close()
	defer stunConn.Close()
	defer quicConn.Close()
	defer registry.Unregister(t.uri.Scheme, quicConn)
	defer filterConn.Close()
	defer packetConn.Close()

	l.Infof("QUIC listener (%v) starting", quicConn.LocalAddr())
	defer l.Infof("QUIC listener (%v) shutting down", quicConn.LocalAddr())

	go t.stunRenewal(stunConn)

	// Accept is forever, so handle stops externally.
	go func() {
		select {
		case <-t.stop:
			_ = listener.Close()
		}
	}()

	for {
		// Blocks forever, see https://github.com/lucas-clemente/quic-go/issues/1915
		session, err := listener.Accept()

		select {
		case <-t.stop:
			if err == nil {
				_ = session.Close()
			}
			return
		default:
		}
		if err != nil {
			if err, ok := err.(net.Error); !ok || !err.Timeout() {
				l.Warnln("Listen (BEP/quic): Accepting connection:", err)
			}
			continue
		}

		l.Debugln("connect from", session.RemoteAddr())

		// Accept blocks forever, give it 10s to do it's thing.
		ok := make(chan struct{})
		go func() {
			select {
			case <-ok:
				return
			case <-time.After(10 * time.Second):
				l.Debugln("timed out waiting for AcceptStream on", session.RemoteAddr())
				_ = session.Close()
			}
		}()

		stream, err := session.AcceptStream()
		close(ok)
		if err != nil {
			l.Debugln("failed to accept stream from", session.RemoteAddr(), err.Error())
			_ = session.Close()
			continue
		}

		t.conns <- internalConn{&quicTlsConn{session, stream}, connTypeQUICServer, quicPriority}
	}
}

func (t *quicListener) Stop() {
	close(t.stop)
}

func (t *quicListener) URI() *url.URL {
	return t.uri
}

func (t *quicListener) WANAddresses() []*url.URL {
	uris := t.LANAddresses()
	t.mut.RLock()
	if t.address != nil {
		uris = append(uris, t.address)
	}
	t.mut.RUnlock()
	return uris
}

func (t *quicListener) LANAddresses() []*url.URL {
	return []*url.URL{t.uri}
}

func (t *quicListener) Error() error {
	t.mut.RLock()
	err := t.err
	t.mut.RUnlock()
	return err
}

func (t *quicListener) String() string {
	return t.uri.String()
}

func (t *quicListener) Factory() listenerFactory {
	return t.factory
}

func (t *quicListener) NATType() string {
	v := t.nat.Load().(stun.NATType)
	if v == stun.NATUnknown || v == stun.NATError {
		return "unknown"
	}
	return v.String()
}

func (t *quicListener) stunRenewal(listener net.PacketConn) {
	client := stun.NewClientWithConnection(listener)
	client.SetSoftwareName("")

	var natType stun.NATType
	var extAddr *stun.Host
	var udpAddr *net.UDPAddr
	var err error

	oldType := stun.NATUnknown

	for {

	disabled:
		if t.cfg.Options().StunKeepaliveS < 1 || !t.cfg.Options().NATEnabled {
			time.Sleep(time.Second)
			oldType = stun.NATUnknown
			t.nat.Store(stun.NATUnknown)
			t.mut.Lock()
			t.address = nil
			t.mut.Unlock()
			continue
		}

		for _, addr := range t.cfg.StunServers() {
			// Resolve the address, so that in case the server advertises two
			// IPs, we always hit the same one, as otherwise, the mapping might
			// expire as we hit the other address, and cause us to flip flop
			// between servers/external addresses, as a result flooding discovery
			// servers.
			udpAddr, err = net.ResolveUDPAddr("udp", addr)
			if err != nil {
				l.Debugf("%s stun addr resolution on %s: %s", t.uri, addr, err)
				continue
			}
			client.SetServerAddr(udpAddr.String())

			natType, extAddr, err = client.Discover()
			if err != nil || extAddr == nil {
				l.Debugf("%s stun discovery on %s: %s", t.uri, addr, err)
				continue
			}

			// The stun server is most likely borked, try another one.
			if natType == stun.NATError || natType == stun.NATUnknown || natType == stun.NATBlocked {
				l.Debugf("%s stun discovery on %s resolved to %s", t.uri, addr, natType)
				continue
			}

			if oldType != natType {
				l.Infof("%s detected NAT type: %s", t.uri, natType)
				t.nat.Store(natType)
				oldType = natType
			}

			// We can't punch through this one, so no point doing keepalives
			// and such, just try again in a minute and hope that the NAT type changes.
			if !isPunchable(natType) {
				break
			}

			addressChanges := 1
			for loops := 1; ; loops++ {
				changed := false

				uri := *t.uri
				uri.Host = extAddr.TransportAddr()

				t.mut.Lock()

				if t.address == nil || t.address.String() != uri.String() {
					l.Infof("%s resolved external address %s (via %s)", t.uri, uri.String(), addr)
					t.address = &uri
					changed = true
					addressChanges++
				}
				t.mut.Unlock()

				// Check that after a few rounds we're not changing addresses at a stupid rate.
				// If we're changing addresses on every second request, something is stuffed with the stun server
				// or router...
				if loops > 3 && loops/addressChanges < 2 {
					break
				}

				// This will most likely result in a call to WANAddresses() which tries to
				// get t.mut, so notify while unlocked.
				if changed {
					t.notifyAddressesChanged(t)
				}

				select {
				case <-time.After(time.Duration(t.cfg.Options().StunKeepaliveS) * time.Second):
				case <-t.stop:
					return
				}

				if t.cfg.Options().StunKeepaliveS < 1 || !t.cfg.Options().NATEnabled {
					goto disabled
				}

				extAddr, err = client.Keepalive()
				if err != nil {
					l.Debugf("%s stun keepalive on %s: %s (%v)", t.uri, addr, err, extAddr)
					break
				}
			}
		}

		// We failed to contact all provided stun servers or the nat is not punchable.
		// Chillout for a while.
		time.Sleep(stunRetryInterval)
	}
}

type quicListenerFactory struct{}

func (f *quicListenerFactory) Valid(config.Configuration) error {
	return nil
}

func (f *quicListenerFactory) New(uri *url.URL, cfg config.Wrapper, tlsCfg *tls.Config, conns chan internalConn, natService *nat.Service) genericListener {
	l := &quicListener{
		uri:     fixupPort(uri, config.DefaultQUICPort),
		cfg:     cfg,
		tlsCfg:  tlsCfg,
		conns:   conns,
		stop:    make(chan struct{}),
		factory: f,
	}
	l.nat.Store(stun.NATUnknown)
	return l
}

func (quicListenerFactory) Enabled(cfg config.Configuration) bool {
	return true
}

func isPunchable(natType stun.NATType) bool {
	return natType == stun.NATNone || natType == stun.NATPortRestricted || natType == stun.NATRestricted || natType == stun.NATFull
}
