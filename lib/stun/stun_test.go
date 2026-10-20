package stun

import (
	"context"
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
