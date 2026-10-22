// Copyright (C) 2026 The Syncthing Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package main

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/netip"
	"strings"
	"sync"
	"time"

	native "github.com/syncthing/syncthing/lib/protocol"
	"github.com/syncthing/syncthing/lib/relay/protocol"
	"golang.org/x/time/rate"
)

type privateRelayConfig struct {
	Issuer      string            `json:"issuer"`
	KeyNotAfter map[string]int64  `json:"key_not_after,omitempty"`
	Keys        map[string][]byte `json:"keys"`
}
type privateDevice struct {
	limiter *rate.Limiter
	retain  int64
}
type privateRegistration struct {
	expires int64
	out     chan any
	limiter *rate.Limiter
}
type privateAllocation struct {
	hash    [32]byte
	expires int64
	session *session
}

// privateRelay owns permission replay and quotas, while session remains the
// sole owner of native relay sockets, invitations and bidirectional copying.
type privateRelay struct {
	mu                     sync.Mutex
	issuer, audience, boot string
	keys                   map[string]ed25519.PublicKey
	keyNotAfter            map[string]int64
	registrations          map[native.DeviceID]*privateRegistration
	devices                map[native.DeviceID]*privateDevice
	sources                map[string]*privateDevice
	allocations            map[string]*privateAllocation
	counts                 map[native.DeviceID]int
	active                 int
	now                    func() time.Time
	handshakes             chan struct{}
	admission              *rate.Limiter
}

func newPrivateRelay(config privateRelayConfig, id native.DeviceID) (*privateRelay, error) {
	if config.Issuer == "" || len(config.Issuer) > 128 || len(config.Keys) < 1 || len(config.Keys) > 2 {
		return nil, protocol.ErrPermit
	}
	keys := map[string]ed25519.PublicKey{}
	for id, key := range config.Keys {
		if id == "" || len(id) > 64 || len(key) != ed25519.PublicKeySize {
			return nil, protocol.ErrPermit
		}
		keys[id] = append(ed25519.PublicKey(nil), key...)
	}
	expiry := make(map[string]int64)
	current := 0
	for id := range keys {
		until := config.KeyNotAfter[id]
		if until == 0 {
			current++
		} else if until <= time.Now().Unix() || until > time.Now().Add(300*time.Second).Unix() {
			return nil, protocol.ErrPermit
		}
		expiry[id] = until
	}
	for id := range config.KeyNotAfter {
		if _, ok := keys[id]; !ok {
			return nil, protocol.ErrPermit
		}
	}
	if current != 1 {
		return nil, protocol.ErrPermit
	}
	b := make([]byte, 16)
	if _, e := rand.Read(b); e != nil {
		return nil, e
	}
	return &privateRelay{issuer: config.Issuer, audience: id.String(), boot: hex.EncodeToString(b), keys: keys, keyNotAfter: expiry, registrations: map[native.DeviceID]*privateRegistration{}, devices: map[native.DeviceID]*privateDevice{}, sources: map[string]*privateDevice{}, allocations: map[string]*privateAllocation{}, counts: map[native.DeviceID]int{}, now: time.Now, handshakes: make(chan struct{}, 512), admission: rate.NewLimiter(200, 512)}, nil
}

func (p *privateRelay) verify(token protocol.PermitToken, id native.DeviceID, use string) (protocol.Permit, error) {
	// The target is extracted only to supply an expected field to the verifier;
	// no existence lookup is performed until the full signature is verified.
	if len(token) > 4096 {
		return protocol.Permit{}, protocol.ErrPermit
	}
	parts := strings.Split(string(token), ".")
	if len(parts) != 2 {
		return protocol.Permit{}, protocol.ErrPermit
	}
	raw, e := base64.RawURLEncoding.Strict().DecodeString(parts[0])
	if e != nil {
		return protocol.Permit{}, protocol.ErrPermit
	}
	var hint struct {
		Target string `json:"target"`
	}
	if json.Unmarshal(raw, &hint) != nil {
		return protocol.Permit{}, protocol.ErrPermit
	}
	keys := make(map[string]ed25519.PublicKey, len(p.keys))
	for id, key := range p.keys {
		if until := p.keyNotAfter[id]; until == 0 || p.now().Unix() < until {
			keys[id] = key
		}
	}
	return protocol.VerifyPermit(token, keys, p.issuer, p.audience, p.boot, use, id.String(), hint.Target, p.now())
}

func (p *privateRelay) register(id native.DeviceID, token protocol.PermitToken, out chan any) (time.Time, error) {
	grant, e := p.verify(token, id, "join")
	if e != nil {
		return time.Time{}, protocol.ErrPermit
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	for device, d := range p.devices {
		if d.retain <= now.Unix() && p.registrations[device] == nil && p.counts[device] == 0 {
			delete(p.devices, device)
		}
	}
	d := p.devices[id]
	if d == nil && len(p.devices) >= 4096 {
		return time.Time{}, protocol.ErrPermit
	}
	if old := p.registrations[id]; old != nil {
		if old.out != out {
			return time.Time{}, protocol.ErrPermit
		}
	} else {
		if len(p.registrations) >= 1024 {
			return time.Time{}, protocol.ErrPermit
		}
		if d == nil {
			d = &privateDevice{limiter: rate.NewLimiter(2, 4)}
			p.devices[id] = d
		}
		p.registrations[id] = &privateRegistration{out: out, limiter: d.limiter}
	}
	p.registrations[id].expires = grant.ExpiresAt
	d.retain = now.Add(300 * time.Second).Unix()
	return time.Unix(grant.ExpiresAt, 0), nil
}

func (p *privateRelay) unregister(id native.DeviceID, out chan any) {
	p.mu.Lock()
	if r := p.registrations[id]; r != nil && r.out == out {
		delete(p.registrations, id)
	}
	p.mu.Unlock()
}

func (p *privateRelay) allocate(id native.DeviceID, token protocol.PermitToken) (*session, chan any, bool, error) {
	grant, e := p.verify(token, id, "connect")
	if e != nil {
		return nil, nil, false, protocol.ErrPermit
	}
	target, e := native.DeviceIDFromString(grant.Target)
	if e != nil {
		return nil, nil, false, protocol.ErrPermit
	}
	hash := sha256.Sum256([]byte(token))
	key := id.String() + ":" + grant.ID
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	for id, a := range p.allocations {
		if now.Unix() >= a.expires && a.session == nil {
			delete(p.allocations, id)
		}
	}
	if a := p.allocations[key]; a != nil {
		if a.hash != hash || a.session == nil {
			return nil, nil, false, protocol.ErrPermit
		}
		return a.session, nil, false, nil
	}
	// Verification precedes both source/target existence checks and allocation.
	source, destination := p.registrations[id], p.registrations[target]
	if source == nil || destination == nil || source.expires <= now.Unix() || destination.expires <= now.Unix() || p.active >= 256 || p.counts[id] >= 4 || p.counts[target] >= 4 || len(p.allocations) >= 4096 || !source.limiter.AllowN(now, 1) {
		return nil, nil, false, protocol.ErrPermit
	}
	ses := newSession(target, id, sessionLimitBps, globalLimiter)
	if ses == nil {
		return nil, nil, false, protocol.ErrPermit
	}
	ses.joinDeadline = time.Now().Add(10 * time.Second)
	if expiry := time.Unix(grant.ExpiresAt, 0); expiry.Before(ses.joinDeadline) {
		ses.joinDeadline = expiry
	}
	p.active++
	p.counts[id]++
	p.counts[target]++
	p.allocations[key] = &privateAllocation{hash: hash, expires: grant.ExpiresAt, session: ses}
	ses.onDone = func() {
		p.mu.Lock()
		defer p.mu.Unlock()
		p.active--
		p.counts[id]--
		p.counts[target]--
		if p.counts[id] == 0 {
			delete(p.counts, id)
		}
		if p.counts[target] == 0 {
			delete(p.counts, target)
		}
		p.allocations[key].session = nil
	}
	go ses.Serve()
	return ses, destination.out, true, nil
}

func (p *privateRelay) handle(tcp net.Conn, config *tls.Config, releaseHandshake func(), deadline time.Time) {
	var released sync.Once
	release := func() { released.Do(releaseHandshake) }
	defer release()
	defer tcp.Close()
	conn := tls.Server(tcp, config)
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	if conn.SetDeadline(deadline) != nil || conn.HandshakeContext(ctx) != nil {
		return
	}
	cs := conn.ConnectionState()
	if cs.NegotiatedProtocol != protocol.ControlledProtocolName || len(cs.PeerCertificates) != 1 {
		return
	}
	id := native.NewDeviceID(cs.PeerCertificates[0].Raw)
	out := make(chan any)
	defer p.unregister(id, out)
	messages := make(chan any)
	errs := make(chan error, 1)
	readerDone := make(chan struct{})
	readerStop := make(chan struct{})
	go func() {
		defer close(readerDone)
		for {
			msg, err := protocol.ReadMessage(conn)
			if err != nil {
				errs <- err
				return
			}
			select {
			case messages <- msg:
			case <-readerStop:
				return
			}
		}
	}()
	defer func() { close(readerStop); conn.Close(); <-readerDone }()
	expiry := time.NewTimer(time.Until(deadline))
	defer expiry.Stop()
	ping := time.NewTicker(30 * time.Second)
	defer ping.Stop()
	joined := false
	deny := func() {
		_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
		_ = protocol.WriteMessage(conn, protocol.ResponseNotFound)
	}
	for {
		select {
		case <-expiry.C:
			return
		case err := <-errs:
			// Read errors end this session; deferred close joins the reader.
			_ = err
			return
		case <-ping.C:
			if !joined {
				return
			}
			_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if protocol.WriteMessage(conn, protocol.Ping{}) != nil {
				return
			}
		case msg := <-out:
			_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if protocol.WriteMessage(conn, msg) != nil {
				return
			}
		case msg := <-messages:
			_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			switch q := msg.(type) {
			case protocol.PrivateJoinRequest:
				until, e := p.register(id, q.Permit, out)
				if e != nil {
					deny()
					return
				}
				release()
				joined = true
				expiry.Reset(time.Until(until))
				_ = conn.SetReadDeadline(time.Time{})
				if protocol.WriteMessage(conn, protocol.ResponseSuccess) != nil {
					return
				}
			case protocol.PrivateConnectRequest:
				ses, destination, created, e := p.allocate(id, q.Permit)
				if e != nil {
					deny()
					return
				}
				release()
				if created {
					timer := time.NewTimer(time.Second)
					select {
					case destination <- ses.GetServerInvitationMessage():
						timer.Stop()
					case <-timer.C:
						ses.abort()
						deny()
						return
					}
				}
				// Retain the same invitation after a lost response; quota remains charged
				// until Serve really exits. A consumed/closed ticket cannot allocate again.
				_ = protocol.WriteMessage(conn, ses.GetClientInvitationMessage())
				return
			case protocol.Ping:
				if protocol.WriteMessage(conn, protocol.Pong{}) != nil {
					return
				}
			case protocol.Pong:
			default:
				deny()
				return
			}
		}
	}
}

// The private listener budgets before reading a protocol byte. Keeping this
// separate preserves the historical public relay listener behavior.
func (p *privateRelay) admitSource(address net.Addr) bool {
	host, _, err := net.SplitHostPort(address.String())
	if err != nil {
		return false
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	ip = ip.Unmap()
	source := ip.String()
	if ip.Is6() {
		source = netip.PrefixFrom(ip, 64).Masked().String()
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	for key, s := range p.sources {
		if s.retain <= now.Unix() {
			delete(p.sources, key)
		}
	}
	s := p.sources[source]
	if s == nil {
		if len(p.sources) >= 4096 {
			return false
		}
		s = &privateDevice{limiter: rate.NewLimiter(20, 40)}
		p.sources[source] = s
	}
	s.retain = now.Add(time.Minute).Unix()
	return s.limiter.AllowN(now, 1) && p.admission.AllowN(now, 1)
}

type privateBufferedConn struct {
	net.Conn

	reader io.Reader
}

func (c *privateBufferedConn) Read(b []byte) (int, error) { return c.reader.Read(b) }

func (p *privateRelay) listen(listener net.Listener, config *tls.Config) {
	var workers sync.WaitGroup
	var mu sync.Mutex
	connections := map[net.Conn]struct{}{}
	defer func() {
		mu.Lock()
		snapshot := make([]net.Conn, 0, len(connections))
		for c := range connections {
			snapshot = append(snapshot, c)
		}
		mu.Unlock()
		for _, c := range snapshot {
			c.Close()
		}
		workers.Wait()
		p.mu.Lock()
		var sessions []*session
		for _, a := range p.allocations {
			if a.session != nil {
				sessions = append(sessions, a.session)
			}
		}
		p.mu.Unlock()
		for _, s := range sessions {
			s.abort()
		}
		for _, s := range sessions {
			<-s.done
		}
	}()

	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		if !p.admitSource(conn.RemoteAddr()) {
			conn.Close()
			continue
		}
		select {
		case p.handshakes <- struct{}{}:
		default:
			conn.Close()
			continue
		}
		mu.Lock()
		connections[conn] = struct{}{}
		mu.Unlock()
		workers.Add(1)
		go func() {
			defer workers.Done()
			defer func() { mu.Lock(); delete(connections, conn); mu.Unlock() }()
			var once sync.Once
			release := func() { once.Do(func() { <-p.handshakes }) }
			defer release()
			if err := setTCPOptions(conn); err != nil {
				conn.Close()
				return
			}
			deadline := time.Now().Add(10 * time.Second)
			if conn.SetReadDeadline(deadline) != nil {
				conn.Close()
				return
			}
			reader := bufio.NewReaderSize(conn, 4096)
			first, err := reader.Peek(1)
			if err != nil {
				conn.Close()
				return
			}
			wrapped := &privateBufferedConn{Conn: conn, reader: reader}
			if first[0] == 0x16 {
				p.handle(wrapped, config, release, deadline)
			} else {
				sessionConnectionHandlerTimeout(wrapped, time.Until(deadline))
			}
		}()
	}
}
