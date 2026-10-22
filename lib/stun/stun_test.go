// Copyright (C) 2026 The Syncthing Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package stun

import (
	"context"
	"errors"
	"github.com/syncthing/syncthing/lib/config"
	"github.com/syncthing/syncthing/lib/events"
	"github.com/syncthing/syncthing/lib/protocol"
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
