// Copyright (C) 2026 The Syncthing Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package stun

import (
	"context"
	"encoding/binary"
	"errors"
	"github.com/syncthing/syncthing/lib/config"
	"github.com/syncthing/syncthing/lib/events"
	"github.com/syncthing/syncthing/lib/protocol"
	"net"
	"testing"
	"time"
)

type policySubscriber struct{}

func (policySubscriber) OnNATTypeChanged(NATType)               {}
func (policySubscriber) OnExternalAddressChanged(*Host, string) {}

func TestControlledPolicyIndependentOfRouterMapping(t *testing.T) {
	cfg := config.Wrap("", config.Configuration{Options: config.OptionsConfiguration{NATEnabled: false, StunKeepaliveMinS: 1, StunKeepaliveStartS: 1, RawStunServers: []string{"default"}}}, protocol.LocalDeviceID, events.NoopLogger)
	s := &Service{cfg: cfg, subscriber: policySubscriber{}}
	if !s.disabled() {
		t.Fatal("standalone NAT-disabled behavior changed")
	}
	s.SetServerPolicy(func() []string { return []string{"default", "127.0.0.1:3478"} })
	if s.disabled() {
		t.Fatal("explicit STUN still depends on router mapping")
	}
	servers := s.servers()
	if len(servers) != 1 || servers[0] != "127.0.0.1:3478" {
		t.Fatal(servers)
	}
	if cfg.Options().NATEnabled {
		t.Fatal("STUN enabled router mapping")
	}
	s.SetServerPolicy(func() []string { return nil })
	if !s.disabled() {
		t.Fatal("empty policy enabled fallback")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx) }()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("disabled STUN did not stop")
	}
}

func TestKeepaliveRejectsReplacedServer(t *testing.T) {
	cfg := config.Wrap("", config.Configuration{Options: config.OptionsConfiguration{StunKeepaliveMinS: 1, StunKeepaliveStartS: 1}}, protocol.LocalDeviceID, events.NoopLogger)
	s := &Service{cfg: cfg, subscriber: policySubscriber{}}
	current := []string{"127.0.0.1:3478"}
	s.SetServerPolicy(func() []string { return current })
	if !s.serverAllowed(current[0]) {
		t.Fatal("initial endpoint rejected")
	}
	old := current[0]
	current = []string{"127.0.0.1:3479"}
	// A nil client makes any attempted network send fail immediately. The policy
	// must stop the old keepalive even though STUN remains enabled via B.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.stunKeepAlive(ctx, old, nil); !errors.Is(err, errServerRevoked) {
		t.Fatal(err)
	}
	if !s.serverAllowed(current[0]) || s.disabled() {
		t.Fatal("replacement endpoint unavailable")
	}
	if err := s.runStunForServer(ctx, old); !errors.Is(err, errServerRevoked) {
		t.Fatal(err)
	}
	s.SetServerPolicy(nil)
	if !s.serverAllowed(old) {
		t.Fatal("standalone behavior changed")
	}
}

func TestPolicySnapshotClearRestoreAndReplace(t *testing.T) {
	for _, tc := range []struct {
		name      string
		snapshots [][]string
	}{
		{"clear and replace", [][]string{{"127.0.0.1:3478"}, nil, {"127.0.0.1:3479"}, nil}},
		{"empty then restore", [][]string{nil, {"127.0.0.1:3478"}, nil}},
		{"invalid then restore", [][]string{{"default", "missing-port"}, {"127.0.0.1:3478"}, nil}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Wrap("", config.Configuration{Options: config.OptionsConfiguration{StunKeepaliveMinS: 1, StunKeepaliveStartS: 1}}, protocol.LocalDeviceID, events.NoopLogger)
			s := &Service{cfg: cfg, subscriber: policySubscriber{}}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			reads := 0
			s.SetServerPolicy(func() []string {
				if reads == len(tc.snapshots) {
					cancel()
					return nil
				}
				result := tc.snapshots[reads]
				reads++
				return result
			})
			// Each nonempty round snapshot is revoked on the access check. A nil
			// client proves that no stale endpoint reaches a network operation.
			if err := s.Serve(ctx); !errors.Is(err, context.Canceled) {
				t.Fatalf("policy refresh stalled: reads=%d err=%v", reads, err)
			}
			if reads != len(tc.snapshots) {
				t.Fatalf("policy snapshots consumed=%d want=%d", reads, len(tc.snapshots))
			}
		})
	}
}

func TestKeepaliveRejectsClearedPolicyAndAllowsRestore(t *testing.T) {
	cfg := config.Wrap("", config.Configuration{Options: config.OptionsConfiguration{StunKeepaliveMinS: 1, StunKeepaliveStartS: 1}}, protocol.LocalDeviceID, events.NoopLogger)
	s := &Service{cfg: cfg, subscriber: policySubscriber{}}
	const endpoint = "127.0.0.1:3478"
	current := []string{endpoint}
	s.SetServerPolicy(func() []string { return current })
	current = nil
	if !s.disabled() {
		t.Fatal("cleared policy enabled")
	}
	if err := s.stunKeepAlive(context.Background(), endpoint, nil); !errors.Is(err, errServerRevoked) {
		t.Fatal(err)
	}
	if err := s.runStunForServer(context.Background(), endpoint); !errors.Is(err, errServerRevoked) {
		t.Fatal(err)
	}
	current = []string{endpoint}
	if s.disabled() || !s.serverAllowed(endpoint) {
		t.Fatal("restored endpoint rejected")
	}
}

// bindingConn returns only XOR-MAPPED-ADDRESS, as a single-IP STUN server does.
// No OTHER-ADDRESS or CHANGE-REQUEST support is available.
type bindingConn struct {
	request    []byte
	writes     int
	beforeRead func()
	ctx        context.Context
	entered    chan struct{}
	invalid    bool
}

func (c *bindingConn) WriteTo(p []byte, _ net.Addr) (int, error) {
	c.request = append([]byte(nil), p...)
	c.writes++
	return len(p), nil
}
func (c *bindingConn) ReadFrom(p []byte) (int, net.Addr, error) {
	if c.entered != nil {
		close(c.entered)
		<-c.ctx.Done()
		return 0, nil, c.ctx.Err()
	}
	if c.beforeRead != nil {
		c.beforeRead()
	}
	response := make([]byte, 32)
	binary.BigEndian.PutUint16(response, 0x101)
	binary.BigEndian.PutUint16(response[2:], 12)
	copy(response[4:20], c.request[4:20])
	binary.BigEndian.PutUint16(response[20:], 0x20)
	binary.BigEndian.PutUint16(response[22:], 8)
	response[25] = 1
	port := uint16(40000 + c.writes)
	if c.invalid {
		port = 0
	}
	binary.BigEndian.PutUint16(response[26:], port^0x2112)
	binary.BigEndian.PutUint32(response[28:], 0xcb007101^0x2112a442)
	return copy(p, response), &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 3478}, nil
}
func (*bindingConn) Close() error { return nil }
func (*bindingConn) LocalAddr() net.Addr {
	return &net.UDPAddr{IP: net.IPv4(192, 0, 2, 1), Port: 22000}
}
func (*bindingConn) SetDeadline(time.Time) error      { return nil }
func (*bindingConn) SetReadDeadline(time.Time) error  { return nil }
func (*bindingConn) SetWriteDeadline(time.Time) error { return nil }

type mappingSubscriber struct {
	addresses []string
	onAddress func()
	types     []NATType
}

func (s *mappingSubscriber) OnNATTypeChanged(v NATType) { s.types = append(s.types, v) }
func (s *mappingSubscriber) OnExternalAddressChanged(h *Host, _ string) {
	if h == nil {
		s.addresses = append(s.addresses, "")
		return
	}
	s.addresses = append(s.addresses, h.TransportAddr())
	if s.onAddress != nil {
		s.onAddress()
	}
}
func newMappingTestService(c *bindingConn, sub Subscriber) *Service {
	cfg := config.Wrap("", config.Configuration{Options: config.OptionsConfiguration{StunKeepaliveMinS: 1, StunKeepaliveStartS: 2}}, protocol.LocalDeviceID, events.NoopLogger)
	s := New(cfg, sub, c)
	s.SetServerPolicy(func() []string { return []string{"127.0.0.1:3478"} })
	return s
}
func TestMappingOnlyBindingAndRefresh(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := &bindingConn{}
	sub := &mappingSubscriber{}
	sub.onAddress = func() {
		if len(sub.addresses) == 2 {
			cancel()
		}
	}
	s := newMappingTestService(c, sub)
	if err := s.runStunForServer(ctx, "127.0.0.1:3478"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if len(sub.addresses) != 3 || sub.addresses[0] != "203.0.113.1:40001" || sub.addresses[1] != "203.0.113.1:40002" || sub.addresses[2] != "" {
		t.Fatal(sub.addresses)
	}
	if c.writes != 2 || len(sub.types) != 0 || s.addr != nil {
		t.Fatalf("writes=%d types=%v address=%v", c.writes, sub.types, s.addr)
	}
	for offset := 20; offset+4 <= len(c.request); {
		typ := binary.BigEndian.Uint16(c.request[offset:])
		size := int(binary.BigEndian.Uint16(c.request[offset+2:]))
		if typ == 3 {
			t.Fatal("sent CHANGE-REQUEST")
		}
		offset += 4 + (size+3)/4*4
	}
}
func TestMappingRejectsLateOrInvalidResponse(t *testing.T) {
	for _, mode := range []string{"cancel", "revoke", "replace", "disabled", "invalid"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			c := &bindingConn{}
			sub := &mappingSubscriber{}
			s := newMappingTestService(c, sub)
			c.beforeRead = func() {
				switch mode {
				case "cancel":
					cancel()
				case "revoke":
					s.SetServerPolicy(func() []string { return nil })
				case "replace":
					s.SetServerPolicy(func() []string { return []string{"127.0.0.1:3479"} })
				case "disabled":
					s.cfg = config.Wrap("", config.Configuration{}, protocol.LocalDeviceID, events.NoopLogger)
				case "invalid":
					c.invalid = true
				}
			}
			if err := s.runStunForServer(ctx, "127.0.0.1:3478"); err == nil {
				t.Fatal("accepted response")
			}
			if len(sub.addresses) != 0 || c.writes != 1 {
				t.Fatalf("published=%v writes=%d", sub.addresses, c.writes)
			}
		})
	}
}
func TestMappingCanceledReadDrainsBeforeReturn(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := &bindingConn{ctx: ctx, entered: make(chan struct{})}
	sub := &mappingSubscriber{}
	s := newMappingTestService(c, sub)
	done := make(chan error, 1)
	go func() { done <- s.runStunForServer(ctx, "127.0.0.1:3478") }()
	<-c.entered
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("read did not drain")
	}
	if c.writes != 1 || len(sub.addresses) != 0 {
		t.Fatal("canceled binding published or retried")
	}
}
func TestStandaloneStillRequiresNATDiscovery(t *testing.T) {
	c := &bindingConn{}
	sub := &mappingSubscriber{}
	s := newMappingTestService(c, sub)
	s.SetServerPolicy(nil)
	if err := s.runStunForServer(context.Background(), "127.0.0.1:3478"); err == nil || err.Error() != "Server error: no changed address" {
		t.Fatal(err)
	}
	if len(sub.addresses) != 0 {
		t.Fatal(sub.addresses)
	}
}
