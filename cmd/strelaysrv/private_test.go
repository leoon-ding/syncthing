// Copyright (C) 2026 The Syncthing Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	native "github.com/syncthing/syncthing/lib/protocol"
	"github.com/syncthing/syncthing/lib/relay/client"
	"github.com/syncthing/syncthing/lib/relay/protocol"
	"io"
	"math/big"
	"net"
	"net/url"
	"sync"
	"testing"
	"time"
)

type privateCloseBarrier struct {
	net.Conn
	entered, release chan struct{}
	once             sync.Once
}

func (c *privateCloseBarrier) Close() error {
	c.once.Do(func() { close(c.entered) })
	<-c.release
	return c.Conn.Close()
}

func TestPrivateAllocationReplayQuotaAndRelease(t *testing.T) {
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	p, err := newPrivateRelay(privateRelayConfig{Issuer: "discovery", Keys: map[string][]byte{"current": pub}}, native.NewDeviceID([]byte("relay")))
	if err != nil {
		t.Fatal(err)
	}
	source, target := native.NewDeviceID([]byte("source")), native.NewDeviceID([]byte("target"))
	mint := func(use string, id native.DeviceID, target string, n int) protocol.PermitToken {
		t.Helper()
		now := time.Now()
		ttl := int64(30)
		if use == "join" {
			ttl = 300
		}
		tok, e := protocol.SignPermit(protocol.Permit{Version: 1, KeyID: "current", Issuer: p.issuer, Audience: p.audience, Boot: p.boot, Purpose: use, Subject: id.String(), Target: target, ID: fmt.Sprintf("%032x", n), IssuedAt: now.Unix(), ExpiresAt: now.Unix() + ttl}, key, now)
		if e != nil {
			t.Fatal(e)
		}
		return tok
	}
	for _, id := range []native.DeviceID{source, target} {
		if _, e := p.register(id, mint("join", id, "", 1), make(chan interface{})); e != nil {
			t.Fatal(e)
		}
	}
	var sessions []*session
	defer func() {
		for _, s := range sessions {
			s.abort()
		}
	}()
	token := mint("connect", source, target.String(), 2)
	first, _, created, e := p.allocate(source, token)
	if e != nil || !created {
		t.Fatal(e, created)
	}
	sessions = append(sessions, first)
	again, _, created, e := p.allocate(source, token)
	if e != nil || created || again != first {
		t.Fatal("retry allocated another session", e)
	}
	if _, _, _, e = p.allocate(target, token); e == nil {
		t.Fatal("wrong native identity accepted")
	}
	for i := 3; i <= 5; i++ {
		s, _, ok, e := p.allocate(source, mint("connect", source, target.String(), i))
		if e != nil || !ok {
			t.Fatal(e)
		}
		sessions = append(sessions, s)
	}
	if _, _, _, e = p.allocate(source, mint("connect", source, target.String(), 6)); e == nil {
		t.Fatal("per-device capacity exceeded")
	}
	left, right := net.Pipe()
	defer right.Close()
	barrier := &privateCloseBarrier{Conn: left, entered: make(chan struct{}), release: make(chan struct{})}
	select {
	case first.connsChan <- barrier:
	case <-time.After(time.Second):
		t.Fatal("session did not accept socket")
	}
	aborted := make(chan struct{})
	go func() { first.abort(); close(aborted) }()
	select {
	case <-barrier.entered:
	case <-time.After(time.Second):
		t.Fatal("close did not begin")
	}
	p.mu.Lock()
	charged := p.active
	p.mu.Unlock()
	if charged != 4 {
		t.Error("capacity released before socket close", charged)
	}
	close(barrier.release)
	<-aborted
	// Completion is observed only after Serve has removed both invitations and
	// closed owned sockets. No timer guesses substitute for this boundary.
	for _, s := range sessions {
		s.abort()
	}
	for _, s := range sessions {
		select {
		case <-s.done:
		case <-time.After(5 * time.Second):
			t.Fatal("quota did not drain")
		}
	}
	p.mu.Lock()
	if len(p.counts) != 0 {
		t.Error("device quota leaked")
	}
	p.mu.Unlock()
	for _, s := range sessions {
		if findSession(string(s.clientkey)) != nil || findSession(string(s.serverkey)) != nil {
			t.Error("invitation leaked")
		}
	}
	if _, _, _, e = p.allocate(source, token); e == nil {
		t.Fatal("closed token allocated again")
	}
	p.mu.Lock()
	out := p.registrations[source].out
	p.mu.Unlock()
	p.unregister(source, out)
	if _, e = p.register(source, mint("join", source, "", 8), make(chan interface{})); e != nil {
		t.Fatal(e)
	}
	if _, _, _, e = p.allocate(source, mint("connect", source, target.String(), 7)); e == nil {
		t.Fatal("reconnect reset rate budget")
	}
	p.now = func() time.Time { return time.Now().Add(time.Second) }
	recovered, _, created, e := p.allocate(source, mint("connect", source, target.String(), 7))
	if e != nil || !created {
		t.Fatal("capacity did not recover", e)
	}
	recovered.abort()
	<-recovered.done

}

func TestPrivateRelayTLSInvitationAndBytes(t *testing.T) {
	oldBuffer := networkBufferSize
	networkBufferSize = 65536
	defer func() { networkBufferSize = oldBuffer }()
	cert := func() tls.Certificate {
		pub, key, _ := ed25519.GenerateKey(rand.Reader)
		tpl := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature}
		raw, e := x509.CreateCertificate(rand.Reader, tpl, tpl, pub, key)
		if e != nil {
			t.Fatal(e)
		}
		return tls.Certificate{Certificate: [][]byte{raw}, PrivateKey: key}
	}
	relayCert, a, b := cert(), cert(), cert()
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	relayID := native.NewDeviceID(relayCert.Certificate[0])
	p, e := newPrivateRelay(privateRelayConfig{Issuer: "discovery", Keys: map[string][]byte{"current": pub}}, relayID)
	if e != nil {
		t.Fatal(e)
	}
	source, target := native.NewDeviceID(a.Certificate[0]), native.NewDeviceID(b.Certificate[0])
	mint := func(use string, id native.DeviceID, other string, n int) protocol.PermitToken {
		now := time.Now()
		ttl := int64(30)
		if use == "join" {
			ttl = 300
		}
		tok, e := protocol.SignPermit(protocol.Permit{Version: 1, KeyID: "current", Issuer: p.issuer, Audience: p.audience, Boot: p.boot, Purpose: use, Subject: id.String(), Target: other, ID: fmt.Sprintf("%032x", n), IssuedAt: now.Unix(), ExpiresAt: now.Unix() + ttl}, key, now)
		if e != nil {
			t.Fatal(e)
		}
		return tok
	}
	cfg := &tls.Config{Certificates: []tls.Certificate{relayCert}, ClientAuth: tls.RequireAnyClientCert, NextProtos: []string{protocol.ControlledProtocolName}, MinVersion: tls.VersionTLS12}
	listener, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	oldPort, oldAddress := sessionPort, sessionAddress
	sessionPort = uint16(listener.Addr().(*net.TCPAddr).Port)
	sessionAddress = net.ParseIP("127.0.0.1").To4()
	t.Cleanup(func() { sessionPort = oldPort; sessionAddress = oldAddress })
	acceptDone := make(chan struct{})
	go func() { defer close(acceptDone); p.listen(listener, cfg) }()
	t.Cleanup(func() {
		listener.Close()
		select {
		case <-acceptDone:
		case <-time.After(5 * time.Second):
			t.Error("listener did not drain")
		}
	})
	connect := func(c tls.Certificate) *tls.Conn {
		conn, e := tls.Dial("tcp", listener.Addr().String(), &tls.Config{Certificates: []tls.Certificate{c}, InsecureSkipVerify: true, NextProtos: []string{protocol.ControlledProtocolName}, MinVersion: tls.VersionTLS12})
		if e != nil {
			t.Fatal(e)
		}
		conn.SetDeadline(time.Now().Add(5 * time.Second))
		t.Cleanup(func() { conn.Close() })
		return conn
	}
	register := func(c tls.Certificate, id native.DeviceID) *tls.Conn {
		conn := connect(c)
		if e := protocol.WriteMessage(conn, protocol.PrivateJoinRequest{Permit: mint("join", id, "", 1)}); e != nil {
			t.Fatal(e)
		}
		msg, e := protocol.ReadMessage(conn)
		if e != nil {
			t.Fatal(e)
		}
		if r, ok := msg.(protocol.Response); !ok || r.Code != 0 {
			t.Fatal(msg)
		}
		return conn
	}
	ac, bc := register(a, source), register(b, target)
	defer ac.Close()
	defer bc.Close()
	uri, _ := url.Parse("relay://" + listener.Addr().String() + "?id=" + relayID.String())
	invitation, err := client.GetPrivateInvitationFromRelay(context.Background(), uri, []tls.Certificate{a}, mint("connect", source, target.String(), 2), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	msg, e := protocol.ReadMessage(bc)
	if e != nil {
		t.Fatal(e)
	}
	targetInvitation, ok := msg.(protocol.SessionInvitation)
	if !ok {
		t.Fatal(msg)
	}

	p.mu.Lock()
	var ses *session
	for _, v := range p.allocations {
		ses = v.session
	}
	p.mu.Unlock()
	if ses == nil {
		t.Fatal("missing session")
	}
	defer func() { ses.abort(); <-ses.done }()
	leftPeer, e := client.JoinSession(context.Background(), invitation)
	if e != nil {
		t.Fatal(e)
	}
	defer leftPeer.Close()
	rightPeer, e := client.JoinSession(context.Background(), targetInvitation)
	if e != nil {
		t.Fatal(e)
	}
	defer rightPeer.Close()
	for _, pair := range [][2]net.Conn{{leftPeer, rightPeer}, {rightPeer, leftPeer}} {
		pair[0].SetDeadline(time.Now().Add(time.Second))
		pair[1].SetDeadline(time.Now().Add(time.Second))
		sent := make(chan error, 1)
		go func() { _, e := pair[0].Write([]byte("native-data")); sent <- e }()
		buf := make([]byte, 11)
		if _, e := io.ReadFull(pair[1], buf); e != nil {
			t.Fatal(e)
		}
		if e := <-sent; e != nil || string(buf) != "native-data" {
			t.Fatal(e, string(buf))
		}
	}
}

func TestPrivateRelayPreviousKeyWindowAndBoot(t *testing.T) {
	current, _, _ := ed25519.GenerateKey(rand.Reader)
	previous, key, _ := ed25519.GenerateKey(rand.Reader)
	now := time.Now()
	cfg := privateRelayConfig{Issuer: "discovery", Keys: map[string][]byte{"current": current, "previous": previous}, KeyNotAfter: map[string]int64{"previous": now.Unix() + 10}}
	p, e := newPrivateRelay(cfg, native.NewDeviceID([]byte("relay")))
	if e != nil {
		t.Fatal(e)
	}
	p.now = func() time.Time { return now }
	id := native.NewDeviceID([]byte("source"))
	token, e := protocol.SignPermit(protocol.Permit{Version: 1, KeyID: "previous", Issuer: p.issuer, Audience: p.audience, Boot: p.boot, Purpose: "join", Subject: id.String(), ID: fmt.Sprintf("%032x", 1), IssuedAt: now.Unix(), ExpiresAt: now.Unix() + 300}, key, now)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = p.verify(token, id, "join"); e != nil {
		t.Fatal(e)
	}
	p.now = func() time.Time { return now.Add(10 * time.Second) }
	if _, e = p.verify(token, id, "join"); e == nil {
		t.Fatal("old key outlived overlap")
	}
	replacement, e := newPrivateRelay(cfg, id)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = replacement.verify(token, id, "join"); e == nil {
		t.Fatal("old boot token accepted")
	}
	cfg.KeyNotAfter = nil
	if _, e = newPrivateRelay(cfg, id); e == nil {
		t.Fatal("unbounded previous key")
	}
}

func TestPrivateRelaySourceBudgetAndExpiry(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	p, e := newPrivateRelay(privateRelayConfig{Issuer: "discovery", Keys: map[string][]byte{"current": pub}}, native.NewDeviceID([]byte("relay")))
	if e != nil {
		t.Fatal(e)
	}
	now := time.Now()
	p.now = func() time.Time { return now }
	first := &net.TCPAddr{IP: net.ParseIP("2001:db8::1"), Port: 1000}
	sameNetwork := &net.TCPAddr{IP: net.ParseIP("2001:db8::2"), Port: 2000}
	for i := 0; i < 40; i++ {
		if !p.admitSource(first) {
			t.Fatal("early source rate failure", i)
		}
	}
	if p.admitSource(sameNetwork) {
		t.Fatal("IPv6 address change bypassed source budget")
	}
	now = now.Add(time.Second)
	if !p.admitSource(sameNetwork) {
		t.Fatal("source budget did not recover")
	}
	now = now.Add(61 * time.Second)
	other := &net.TCPAddr{IP: net.ParseIP("192.0.2.1"), Port: 3000}
	if !p.admitSource(other) || len(p.sources) != 1 {
		t.Fatal("expired source budget retained")
	}
}

func TestPrivateRelayDefaultCapacityAndRecovery(t *testing.T) {
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	p, err := newPrivateRelay(privateRelayConfig{Issuer: "capacity", Keys: map[string][]byte{"current": pub}}, native.NewDeviceID([]byte("capacity-relay")))
	if err != nil {
		t.Fatal(err)
	}
	token := func(use string, id, target native.DeviceID, n int) protocol.PermitToken {
		now := time.Now()
		ttl := int64(30)
		targetID := target.String()
		if use == "join" {
			ttl = 300
			targetID = ""
		}
		out, err := protocol.SignPermit(protocol.Permit{Version: 1, KeyID: "current", Issuer: p.issuer, Audience: p.audience, Boot: p.boot, Purpose: use, Subject: id.String(), Target: targetID, ID: fmt.Sprintf("%032x", n), IssuedAt: now.Unix(), ExpiresAt: now.Unix() + ttl}, key, now)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	ids := make([]native.DeviceID, 1025)
	outs := make([]chan interface{}, 1025)
	for i := range ids {
		ids[i] = native.NewDeviceID([]byte(fmt.Sprint("capacity-device-", i)))
		outs[i] = make(chan interface{})
		_, err := p.register(ids[i], token("join", ids[i], native.EmptyDeviceID, i+1), outs[i])
		if (i < 1024) != (err == nil) {
			t.Fatalf("registration %d: %v", i, err)
		}
	}
	p.unregister(ids[1023], outs[1023])
	if _, err := p.register(ids[1024], token("join", ids[1024], native.EmptyDeviceID, 1025), outs[1024]); err != nil {
		t.Fatal("registration capacity did not recover", err)
	}
	var sessions []*session
	defer func() {
		for _, s := range sessions {
			s.abort()
			<-s.done
		}
	}()
	for i := 0; i < 256; i++ {
		s, _, created, err := p.allocate(ids[2*i], token("connect", ids[2*i], ids[2*i+1], 2048+i))
		if err != nil || !created {
			t.Fatalf("allocation %d: %v", i, err)
		}
		sessions = append(sessions, s)
	}
	if _, _, _, err := p.allocate(ids[512], token("connect", ids[512], ids[513], 4096)); err == nil {
		t.Fatal("global allocation capacity exceeded")
	}
	sessions[0].abort()
	<-sessions[0].done
	s, _, created, err := p.allocate(ids[512], token("connect", ids[512], ids[513], 4097))
	if err != nil || !created {
		t.Fatal("allocation capacity did not recover", err)
	}
	sessions = append(sessions, s)
}
