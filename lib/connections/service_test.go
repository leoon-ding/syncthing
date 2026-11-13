// Copyright (C) 2026 The Syncthing Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package connections

import (
	"context"
	"errors"
	"fmt"
	"github.com/syncthing/syncthing/lib/config"
	"github.com/syncthing/syncthing/lib/events"
	"github.com/syncthing/syncthing/lib/protocol"
	relayprotocol "github.com/syncthing/syncthing/lib/relay/protocol"
	"github.com/syncthing/syncthing/lib/semaphore"
	"github.com/syncthing/syncthing/lib/stats"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

type admissionTestDialer struct {
	genericDialer
	calls int
}

func (d *admissionTestDialer) Dial(context.Context, protocol.DeviceID, *url.URL) (internalConn, error) {
	d.calls++
	return internalConn{}, errors.New("injected dial failure")
}

func TestPrivateLookupDoesNotPersistOrReplaceDirect(t *testing.T) {
	cfg := config.DeviceConfiguration{DeviceID: protocol.NewDeviceID([]byte("peer")), Addresses: []string{"tcp://192.0.2.1:22000"}}
	s := &service{}
	s.options.Lookup = func(context.Context, protocol.DeviceID) ([]string, error) {
		return []string{"quic://192.0.2.2:22000"}, nil
	}
	addrs, pending := s.resolveDeviceAddrs(context.Background(), cfg)
	if pending || len(addrs) != 2 || addrs[0] != cfg.Addresses[0] || len(cfg.Addresses) != 1 {
		t.Fatal(addrs, cfg.Addresses)
	}
	s.options.Lookup = func(context.Context, protocol.DeviceID) ([]string, error) { return nil, errors.New("revoked") }
	if addrs, pending = s.resolveDeviceAddrs(context.Background(), cfg); !pending || len(addrs) != 1 || addrs[0] != cfg.Addresses[0] {
		t.Fatal(addrs)
	}
}

func TestDialAdmissionFailureAndRelease(t *testing.T) {
	for _, mode := range []string{"denied", "canceled", "dial-failure", "nil-context"} {
		t.Run(mode, func(t *testing.T) {
			d := &admissionTestDialer{}
			releases := 0
			s := &service{options: Options{BeforeDial: func(ctx context.Context, _ protocol.DeviceID, _ string, _ bool) (context.Context, func(), error) {
				done := func() { releases++ }
				switch mode {
				case "denied":
					return nil, done, errors.New("denied")
				case "nil-context":
					return nil, done, nil
				case "canceled":
					scope, cancel := context.WithCancel(ctx)
					cancel()
					return scope, done, nil
				}
				return ctx, done, nil
			}}}
			_, err := s.dialWithAdmission(context.Background(), dialTarget{dialer: d})
			if err == nil || releases != 1 {
				t.Fatal(err, releases)
			}
			expected := 0
			if mode == "dial-failure" {
				expected = 1
			}
			if d.calls != expected {
				t.Fatal(d.calls)
			}
		})
	}
}

type heldNativeClose struct {
	protocol.Connection
	started chan struct{}
	done    chan struct{}
	once    sync.Once
}

func (c *heldNativeClose) Close(error)             { c.once.Do(func() { close(c.started) }); <-c.done }
func (c *heldNativeClose) Closed() <-chan struct{} { return c.done }
func TestNativeQuiesceRetainsIncompleteCloseAndSealsDial(t *testing.T) {
	conn := &heldNativeClose{started: make(chan struct{}), done: make(chan struct{})}
	s := &service{}
	s.connections = map[protocol.DeviceID][]protocol.Connection{protocol.EmptyDeviceID: {conn}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.Quiesce(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	<-conn.started
	dialer := &admissionTestDialer{}
	if _, err := s.dialWithAdmission(context.Background(), dialTarget{dialer: dialer}); !errors.Is(err, context.Canceled) || dialer.calls != 0 {
		t.Fatal("dial after quiesce", err)
	}
	close(conn.done)
	if err := s.Quiesce(context.Background()); err != nil {
		t.Fatal(err)
	}
}
func TestNativeRelayRetryStopsAndFreshEngineRecovers(t *testing.T) {
	for generation := 0; generation < 2; generation++ {
		network, cancel := context.WithCancel(context.Background())
		called, exited := make(chan struct{}), make(chan struct{})
		calls, releases := 0, 0
		s := &service{networkCtx: network, networkCancel: cancel}
		s.options.PrivateRelay = func(context.Context) (context.Context, *url.URL, relayprotocol.PermitToken, func(), error) {
			calls++
			close(called)
			return nil, nil, "", func() { releases++ }, errors.New("retryable")
		}
		go func() { defer close(exited); _ = s.servePrivateRelay(context.Background()) }()
		<-called
		if err := s.Quiesce(context.Background()); err != nil {
			t.Fatal(err)
		}
		select {
		case <-exited:
		case <-time.After(time.Second):
			t.Fatal("relay retry ignored stop")
		}
		if calls != 1 || releases != 1 {
			t.Fatal(calls, releases)
		}
	}
}

// The first equal-priority connection consumes the steady-state slot; a better
// path must still pass the same check for the reserved handoff slot.
type nativePolicyConnection struct {
	protocol.Connection
	local    bool
	priority int
}

func (c nativePolicyConnection) Closed() <-chan struct{} { return nil }
func (c nativePolicyConnection) IsLocal() bool           { return c.local }
func (c nativePolicyConnection) Priority() int           { return c.priority }
func TestNativeWANConnectionTargetAndHandoff(t *testing.T) {
	id := protocol.NewDeviceID([]byte("native-peer"))
	cfg := config.New(protocol.LocalDeviceID)
	cfg.Devices = append(cfg.Devices, config.DeviceConfiguration{DeviceID: id, RawNumConnections: 3})
	s := &service{cfg: config.Wrap("", cfg, protocol.LocalDeviceID, events.NoopLogger)}
	s.connections = map[protocol.DeviceID][]protocol.Connection{id: {nativePolicyConnection{priority: 50}}}
	s.wantConnections = map[protocol.DeviceID]int{id: 3}
	if got := s.desiredConnectionsToDevice(id, s.singleWANConnection()); got != 3 {
		t.Fatal("legacy target", got)
	}
	active := true
	s.options.SingleWANConnection = func() bool { return active }
	if got := s.desiredConnectionsToDevice(id, s.singleWANConnection()); got != 1 {
		t.Fatal("WAN target", got)
	}
	if s.helloForDevice(id).NumConnections != 3 {
		t.Fatal("WAN policy reduced the Hello total")
	}
	if err := s.connectionCheckEarly(id, internalConn{priority: 50}); !errors.Is(err, errDeviceAlreadyConnected) {
		t.Fatal("duplicate occupied upgrade slot", err)
	}
	if err := s.connectionCheckEarly(id, internalConn{priority: 30}); err != nil {
		t.Fatal("upgrade rejected", err)
	}
	s.connections[id] = []protocol.Connection{nativePolicyConnection{priority: 30}}
	if err := s.connectionCheckEarly(id, internalConn{priority: 30}); !errors.Is(err, errDeviceAlreadyConnected) {
		t.Fatal("stale concurrent Hello accepted", err)
	}
	s.connections[id] = []protocol.Connection{nativePolicyConnection{local: true, priority: 10}}
	if got := s.desiredConnectionsToDevice(id, s.singleWANConnection()); got != 3 {
		t.Fatal("LAN target changed", got)
	}
	active = false
	if s.helloForDevice(id).NumConnections != 3 {
		t.Fatal("legacy policy not restored")
	}
	if s.desiredConnectionsToDevice(protocol.NewDeviceID([]byte("unknown")), s.singleWANConnection()) != 0 {
		t.Fatal("unknown peer target")
	}
}

// Admission consumes the existing LAN classification without replacing the
// resolver or changing the native dialer's address-selection policy.
func TestDialAdmissionPreservesLANClassification(t *testing.T) {
	cfg := config.Wrap("/dev/null", config.Configuration{Options: config.OptionsConfiguration{
		AlwaysLocalNets: []string{"10.20.30.0/24", "fd00:1234::/64"},
	}}, protocol.LocalDeviceID, events.NoopLogger)
	for _, tc := range []struct {
		address string
		local   bool
	}{
		{"tcp://127.0.0.1:22000", true},
		{"tcp://[::1]:22000", true},
		{"quic://10.20.30.40:22000", true},
		{"quic://[fd00:1234::1]:22000", true},
		{"tcp://192.0.2.1:22000", false},
		{"tcp://[2001:db8::1]:22000", false},
		{"private-relay://127.0.0.1:22067", false},
	} {
		t.Run(tc.address, func(t *testing.T) {
			denied := errors.New("stop after admission observation")
			peer := protocol.NewDeviceID([]byte("peer"))
			calls, releases := 0, 0
			d := &admissionTestDialer{}
			s := &service{lanChecker: &lanChecker{cfg: cfg}}
			s.options.BeforeDial = func(ctx context.Context, id protocol.DeviceID, address string, local bool) (context.Context, func(), error) {
				calls++
				if id != peer || address != tc.address || local != tc.local {
					t.Errorf("unexpected admission input: %v %s %v", id, address, local)
				}
				return nil, func() { releases++ }, denied
			}
			_, err := s.dialWithAdmission(context.Background(), dialTarget{deviceID: peer, addr: tc.address, dialer: d})
			if !errors.Is(err, denied) || calls != 1 || releases != 1 || d.calls != 0 {
				t.Fatalf("err=%v calls=%d releases=%d dials=%d", err, calls, releases, d.calls)
			}
		})
	}
}

func TestNativeWANLimitExcludesLANConnections(t *testing.T) {
	id := protocol.NewDeviceID([]byte("mixed-peer"))
	cfg := config.New(protocol.LocalDeviceID)
	cfg.Devices = append(cfg.Devices, config.DeviceConfiguration{DeviceID: id, RawNumConnections: 3})
	s := &service{cfg: config.Wrap("", cfg, protocol.LocalDeviceID, events.NoopLogger)}
	s.options.SingleWANConnection = func() bool { return true }
	s.wantConnections = map[protocol.DeviceID]int{id: 3}
	for _, tc := range []struct {
		name     string
		conns    []protocol.Connection
		priority int
		reject   bool
	}{
		{"LAN does not occupy WAN", []protocol.Connection{nativePolicyConnection{local: true, priority: 50}}, 50, false},
		{"mixed duplicate WAN", []protocol.Connection{nativePolicyConnection{local: true, priority: 50}, nativePolicyConnection{priority: 50}}, 50, true},
		{"mixed WAN upgrade", []protocol.Connection{nativePolicyConnection{local: true, priority: 50}, nativePolicyConnection{priority: 50}}, 30, false},
		{"total cap preserved", []protocol.Connection{nativePolicyConnection{local: true, priority: 50}, nativePolicyConnection{local: true, priority: 50}, nativePolicyConnection{local: true, priority: 50}}, 50, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s.connections = map[protocol.DeviceID][]protocol.Connection{id: tc.conns}
			err := s.connectionCheckEarly(id, internalConn{priority: tc.priority})
			if errors.Is(err, errDeviceAlreadyConnected) != tc.reject {
				t.Fatalf("admission: %v", err)
			}
			if s.desiredConnectionsToDevice(id, s.singleWANConnection()) != 3 {
				t.Fatal("WAN policy reduced LAN connection target")
			}
		})
	}
}

func TestWANHelloPreservesPeerMixedConnectionBudget(t *testing.T) {
	id := protocol.NewDeviceID([]byte("mixed-hello"))
	cfg := config.New(protocol.LocalDeviceID)
	cfg.Devices = append(cfg.Devices, config.DeviceConfiguration{DeviceID: id, RawNumConnections: 3})
	sender := &service{cfg: config.Wrap("", cfg, protocol.LocalDeviceID, events.NoopLogger)}
	sender.options.SingleWANConnection = func() bool { return true }
	for _, limited := range []bool{false, true} {
		peer := &service{cfg: sender.cfg}
		peer.options.SingleWANConnection = func() bool { return limited }
		peer.wantConnections = map[protocol.DeviceID]int{id: sender.helloForDevice(id).NumConnections}
		peer.connections = map[protocol.DeviceID][]protocol.Connection{id: {nativePolicyConnection{priority: 50}, nativePolicyConnection{local: true, priority: 10}}}
		if got := peer.negotiatedConnectionsToDevice(id); got != 3 {
			t.Fatalf("peer policy=%v total=%d", limited, got)
		}
		if err := peer.connectionCheckEarly(id, internalConn{priority: 10, isLocal: true}); err != nil {
			t.Fatalf("peer policy=%v rejected additional LAN: %v", limited, err)
		}
	}
}

type wanLimitTestConnection struct {
	protocol.Connection
	id       string
	local    bool
	priority int
	started  chan struct{}
	done     chan struct{}
	once     sync.Once
}

func (c *wanLimitTestConnection) ConnectionID() string    { return c.id }
func (c *wanLimitTestConnection) IsLocal() bool           { return c.local }
func (c *wanLimitTestConnection) Priority() int           { return c.priority }
func (c *wanLimitTestConnection) Close(error)             { c.once.Do(func() { close(c.started) }); <-c.done }
func (c *wanLimitTestConnection) Closed() <-chan struct{} { return c.done }

func TestWANLimitActivationAndDrain(t *testing.T) {
	for _, mixed := range []bool{false, true} {
		t.Run(fmt.Sprintf("mixed=%v", mixed), func(t *testing.T) {
			id := protocol.NewDeviceID([]byte("peer"))
			makeConn := func(name string, priority int, local bool) *wanLimitTestConnection {
				return &wanLimitTestConnection{id: name, priority: priority, local: local, started: make(chan struct{}), done: make(chan struct{})}
			}
			a, b, c := makeConn("a", 30, false), makeConn("b", 30, false), makeConn("c", 50, false)
			lan := makeConn("lan", 10, true)
			lan2 := makeConn("lan2", 10, true)
			group := []protocol.Connection{c, b, a}
			if mixed {
				group = append(group, lan, lan2)
			}
			s := &service{}
			s.connections = map[protocol.DeviceID][]protocol.Connection{id: group}
			var active atomic.Bool
			s.options.SingleWANConnection = active.Load
			ctx, cancel := context.WithCancel(context.Background())
			ticks := make(chan time.Time)
			finished := make(chan error, 1)
			go func() { finished <- s.runWANLimit(ctx, ticks) }()
			defer func() {
				cancel()
				close(a.done)
				close(b.done)
				close(c.done)
				close(lan.done)
				close(lan2.done)
				<-finished
			}()
			tick := func() {
				t.Helper()
				select {
				case ticks <- time.Now():
				case <-time.After(time.Second):
					t.Fatal("reconciler stopped")
				}
			}
			tick() // Completed an iteration with the legacy policy.
			for _, conn := range []*wanLimitTestConnection{a, b, c, lan, lan2} {
				select {
				case <-conn.started:
					t.Fatal("legacy link closed")
				default:
				}
			}
			active.Store(true)
			tick()
			for _, conn := range []*wanLimitTestConnection{b, c} {
				select {
				case <-conn.started:
				case <-time.After(time.Second):
					t.Fatal("excess WAN not closed")
				}
			}
			// Simulate an attachment that completed after the activation snapshot.
			late := makeConn("late", 50, false)
			defer close(late.done)
			s.connectionsMut.Lock()
			s.connections[id] = append(s.connections[id], late)
			s.connectionsMut.Unlock()
			tick()
			select {
			case <-late.started:
			case <-time.After(time.Second):
				t.Fatal("late attachment escaped limit")
			}
			tick() // Closing links remain accounted but are not selected again.
			for _, conn := range []*wanLimitTestConnection{a, lan, lan2} {
				select {
				case <-conn.started:
					t.Fatal("best WAN or LAN closed")
				default:
				}
			}
			cancel()
			select {
			case <-finished:
				t.Fatal("stopped before owned closes drained")
			default:
			}
		})
	}
}

func TestWANLimitLegacyHostAndIdleStop(t *testing.T) {
	s := &service{}
	ctx, cancel := context.WithCancel(context.Background())
	ticks := make(chan time.Time)
	done := make(chan error, 1)
	go func() { done <- s.runWANLimit(ctx, ticks) }()
	select {
	case ticks <- time.Now():
	case <-time.After(time.Second):
		t.Fatal("legacy loop stalled")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("idle stop blocked")
	}
}

func TestWANLimitRetainsLiveAlternativeDuringClosedLinkRelease(t *testing.T) {
	id := protocol.NewDeviceID([]byte("closed-release-peer"))
	dead := &wanLimitTestConnection{id: "dead", priority: 10, started: make(chan struct{}), done: make(chan struct{})}
	live := &wanLimitTestConnection{id: "live", priority: 30, started: make(chan struct{}), done: make(chan struct{})}
	// Closed has fired, but the embedding release has not returned, so the
	// dead connection remains in the tracker exactly as in handleHellos.
	close(dead.done)
	s := &service{}
	s.connections = map[protocol.DeviceID][]protocol.Connection{id: {dead, live}}
	s.options.SingleWANConnection = func() bool { return true }
	ctx, cancel := context.WithCancel(context.Background())
	ticks := make(chan time.Time)
	finished := make(chan struct{})
	go func() { defer close(finished); _ = s.runWANLimit(ctx, ticks) }()
	defer func() { cancel(); close(live.done); <-finished }()
	for i := 0; i < 2; i++ {
		select {
		case ticks <- time.Now():
		case <-time.After(time.Second):
			t.Fatal("reconciler stalled")
		}
	}
	// The live alternative must survive both the delayed release and its
	// eventual tracker removal, without requesting another close of dead.
	s.connectionsMut.Lock()
	s.connections[id] = []protocol.Connection{live}
	s.connectionsMut.Unlock()
	select {
	case ticks <- time.Now():
	case <-time.After(time.Second):
		t.Fatal("reconciler stalled")
	}
	cancel()
	select {
	case <-finished:
	case <-live.started:
		t.Fatal("live alternative retired in favor of closed link")
	case <-time.After(time.Second):
		t.Fatal("stop blocked")
	}
	for _, conn := range []*wanLimitTestConnection{dead, live} {
		select {
		case <-conn.started:
			t.Fatalf("unexpected close: %s", conn.id)
		default:
		}
	}
}

func TestClosedWANAdmissionAndCapacity(t *testing.T) {
	for _, tc := range []struct {
		name     string
		limit    int
		lan, wan int
		active   bool
		reject   bool
	}{
		{"single-slot reconnect", 1, 0, 0, true, false},
		{"mixed reconnect", 3, 2, 0, true, false},
		{"live WAN still occupies slot", 3, 0, 1, true, true},
		{"live total limit still enforced", 2, 2, 0, true, true},
		{"legacy accounting unchanged", 1, 0, 0, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id := protocol.NewDeviceID([]byte("reconnect"))
			cfg := config.New(protocol.LocalDeviceID)
			cfg.Devices = append(cfg.Devices, config.DeviceConfiguration{DeviceID: id, RawNumConnections: tc.limit})
			s := &service{cfg: config.Wrap("", cfg, protocol.LocalDeviceID, events.NoopLogger)}
			s.options.SingleWANConnection = func() bool { return tc.active }
			s.wantConnections = map[protocol.DeviceID]int{id: tc.limit}
			dead := &wanLimitTestConnection{id: "dead", priority: 50, done: make(chan struct{}), started: make(chan struct{})}
			close(dead.done)
			group := []protocol.Connection{dead}
			for i := 0; i < tc.lan; i++ {
				group = append(group, nativePolicyConnection{local: true, priority: 50})
			}
			for i := 0; i < tc.wan; i++ {
				group = append(group, nativePolicyConnection{priority: 50})
			}
			s.connections = map[protocol.DeviceID][]protocol.Connection{id: group}
			// The release callback is blocked: keep the closed connection in
			// the resource tracker throughout repeated admission snapshots.
			for i := 0; i < 2; i++ {
				err := s.connectionCheckEarly(id, internalConn{priority: 50})
				if errors.Is(err, errDeviceAlreadyConnected) != tc.reject {
					t.Fatalf("admission=%v want rejection=%v", err, tc.reject)
				}
				if got := s.numConnectionsForDevice(id); got != len(group) {
					t.Fatalf("resources released early: %d", got)
				}
				count, _, _, _ := s.admissionConnections(id, s.singleWANConnection())
				want := len(group)
				if tc.active {
					want--
				}
				if count != want {
					t.Fatalf("dial/admission snapshot=%d want=%d", count, want)
				}
			}
			if !tc.reject {
				// A successful replacement must immediately occupy the live WAN
				// slot, even though the old resource is still charged.
				s.connections[id] = append(s.connections[id], nativePolicyConnection{priority: 50})
				if err := s.connectionCheckEarly(id, internalConn{priority: 50}); !errors.Is(err, errDeviceAlreadyConnected) {
					t.Fatalf("second replacement accepted: %v", err)
				}
				if got := s.numConnectionsForDevice(id); got != len(group)+1 {
					t.Fatalf("old resource dropped: %d", got)
				}
			}
		})
	}
}

func TestWANPolicySampledOnceDuringAdmission(t *testing.T) {
	for _, initial := range []bool{false, true} {
		t.Run(fmt.Sprint(initial), func(t *testing.T) {
			id := protocol.NewDeviceID([]byte("policy-transition"))
			cfg := config.New(protocol.LocalDeviceID)
			cfg.Devices = append(cfg.Devices, config.DeviceConfiguration{DeviceID: id, RawNumConnections: 3})
			s := &service{cfg: config.Wrap("", cfg, protocol.LocalDeviceID, events.NoopLogger)}
			s.wantConnections = map[protocol.DeviceID]int{id: 3}
			dead := &wanLimitTestConnection{id: "dead", priority: 50, done: make(chan struct{}), started: make(chan struct{})}
			close(dead.done)
			s.connections = map[protocol.DeviceID][]protocol.Connection{id: {dead}}
			policy, reads := initial, 0
			s.options.SingleWANConnection = func() bool {
				reads++
				result := policy
				policy = !policy // Deterministically switch immediately after sampling.
				return result
			}
			for attempt := 0; attempt < 2; attempt++ {
				before := reads
				if err := s.connectionCheckEarly(id, internalConn{priority: 50}); err != nil {
					t.Fatalf("same-priority reconnect: %v", err)
				}
				if reads != before+1 {
					t.Fatalf("policy sampled %d times", reads-before)
				}
				if s.numConnectionsForDevice(id) != 1 {
					t.Fatal("admission refunded an unreleased resource")
				}
			}
		})
	}
}

func TestWANPolicySnapshotControlsConnectionTarget(t *testing.T) {
	id := protocol.NewDeviceID([]byte("target-transition"))
	cfg := config.New(protocol.LocalDeviceID)
	cfg.Devices = append(cfg.Devices, config.DeviceConfiguration{DeviceID: id, RawNumConnections: 3})
	s := &service{cfg: config.Wrap("", cfg, protocol.LocalDeviceID, events.NoopLogger)}
	s.wantConnections = map[protocol.DeviceID]int{id: 3}
	dead := &wanLimitTestConnection{id: "dead", priority: 50, done: make(chan struct{}), started: make(chan struct{})}
	close(dead.done)
	s.connections = map[protocol.DeviceID][]protocol.Connection{id: {dead}}
	for _, initial := range []bool{false, true} {
		reads := 0
		policy := initial
		s.options.SingleWANConnection = func() bool {
			reads++
			result := policy
			policy = !policy
			return result
		}
		sampled := s.singleWANConnection()
		count, _, _, _ := s.admissionConnections(id, sampled)
		if got := s.desiredConnectionsToDevice(id, sampled); got != 3 {
			t.Fatalf("closed WAN incorrectly reduced target: %d", got)
		}
		if (count == 0) != initial || reads != 1 {
			t.Fatalf("inconsistent policy: initial=%v count=%d reads=%d", initial, count, reads)
		}
		if s.numConnectionsForDevice(id) != 1 {
			t.Fatal("target selection refunded an unreleased resource")
		}
	}
}

// This model has no filesystem or network work; tests run the real connection
// scheduler with fake time and stop at the admission boundary.
type dialLoopModel struct{ Model }

func (dialLoopModel) DeviceStatistics() (map[protocol.DeviceID]stats.DeviceStatistics, error) {
	return nil, nil
}

func TestPrivateLookupArrivingAfterResumeDoesNotWaitForMinuteLoop(t *testing.T) {
	for _, mode := range []string{"new", "resume", "vpn-recovery"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				peer := protocol.NewDeviceID([]byte("peer"))
				cfg := config.Configuration{}
				if mode != "new" {
					cfg.Devices = []config.DeviceConfiguration{{DeviceID: peer, Paused: true}}
				}
				wrapped := config.Wrap("", cfg, protocol.LocalDeviceID, events.NoopLogger)
				ctx, cancel := context.WithCancel(context.Background())
				configDone := make(chan struct{})
				go func() { defer close(configDone); _ = wrapped.Serve(ctx) }()
				var ready atomic.Bool
				var lookups, attempts atomic.Int32
				s := &service{cfg: wrapped, myID: protocol.LocalDeviceID, model: dialLoopModel{}, dialNow: make(chan struct{}, 1), dialNowDevices: make(map[protocol.DeviceID]struct{}), connectionStatusHandler: newConnectionStatusHandler(), lanChecker: &lanChecker{cfg: wrapped}}
				s.options.Lookup = func(context.Context, protocol.DeviceID) ([]string, error) {
					lookups.Add(1)
					if !ready.Load() {
						if mode == "vpn-recovery" {
							return nil, errors.New("candidate snapshot not ready")
						}
						return nil, nil
					}
					return []string{"private-relay://127.0.0.1:22067"}, nil
				}
				s.options.BeforeDial = func(context.Context, protocol.DeviceID, string, bool) (context.Context, func(), error) {
					attempts.Add(1)
					return nil, nil, errors.New("test admission rejection")
				}
				done := make(chan struct{})
				go func() { defer close(done); _ = s.connect(ctx) }()
				defer func() { cancel(); <-done; <-configDone }()
				// End initial ramp-up before adding/resuming the device, as in the WAN run.
				time.Sleep(300 * time.Second)
				synctest.Wait()
				from := wrapped.RawCopy()
				waiter, err := wrapped.Modify(func(c *config.Configuration) { c.Devices = []config.DeviceConfiguration{{DeviceID: peer}} })
				if err != nil {
					t.Fatal(err)
				}
				waiter.Wait()
				s.checkAndSignalConnectLoopOnUpdatedDevices(from, wrapped.RawCopy())
				synctest.Wait()
				if lookups.Load() != 1 || attempts.Load() != 0 {
					t.Fatal(lookups.Load(), attempts.Load())
				}
				ready.Store(true)
				time.Sleep(minConnectionLoopSleep)
				synctest.Wait()
				if attempts.Load() != 1 {
					t.Fatalf("new hints stranded: lookups=%d attempts=%d", lookups.Load(), attempts.Load())
				}
				// Hints do not grant admission or shorten the failed address's 30s backoff.
				time.Sleep(minConnectionLoopSleep)
				synctest.Wait()
				if attempts.Load() != 1 {
					t.Fatal("address cooldown bypassed", attempts.Load())
				}
			})
		})
	}
}

func TestDialTimeIsNotAddedToReconnectDelay(t *testing.T) {
	for _, latency := range []time.Duration{10 * time.Second, 35 * time.Second} {
		t.Run(latency.String(), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				peer := protocol.NewDeviceID([]byte("peer"))
				wrapped := config.Wrap("", config.Configuration{}, protocol.LocalDeviceID, events.NoopLogger)
				ctx, cancel := context.WithCancel(context.Background())
				configDone := make(chan struct{})
				go func() { defer close(configDone); _ = wrapped.Serve(ctx) }()
				s := &service{cfg: wrapped, myID: protocol.LocalDeviceID, model: dialLoopModel{}, dialNow: make(chan struct{}, 1), dialNowDevices: make(map[protocol.DeviceID]struct{}), connectionStatusHandler: newConnectionStatusHandler(), lanChecker: &lanChecker{cfg: wrapped}}
				s.options.Lookup = func(context.Context, protocol.DeviceID) ([]string, error) {
					return []string{"private-relay://127.0.0.1:22067"}, nil
				}
				var mu sync.Mutex
				var attempts []time.Time
				s.options.BeforeDial = func(ctx context.Context, _ protocol.DeviceID, _ string, _ bool) (context.Context, func(), error) {
					mu.Lock()
					attempts = append(attempts, time.Now())
					first := len(attempts) == 1
					mu.Unlock()
					if first {
						select {
						case <-time.After(latency):
						case <-ctx.Done():
						}
					}
					return nil, nil, errors.New("test admission rejection")
				}
				done := make(chan struct{})
				go func() { defer close(done); _ = s.connect(ctx) }()
				defer func() { cancel(); <-done; <-configDone }()
				time.Sleep(300 * time.Second)
				synctest.Wait()
				from := wrapped.RawCopy()
				waiter, err := wrapped.Modify(func(c *config.Configuration) {
					c.Devices = []config.DeviceConfiguration{{DeviceID: peer, Addresses: []string{"dynamic"}}}
				})
				if err != nil {
					t.Fatal(err)
				}
				waiter.Wait()
				s.checkAndSignalConnectLoopOnUpdatedDevices(from, wrapped.RawCopy())
				synctest.Wait()
				expected := 30 * time.Second
				if latency >= expected {
					expected = latency + minConnectionLoopSleep
				}
				time.Sleep(expected)
				synctest.Wait()
				mu.Lock()
				defer mu.Unlock()
				if len(attempts) != 2 || attempts[1].Sub(attempts[0]) != expected {
					t.Fatalf("dial time added to backoff: %v", attempts)
				}
			})
		})
	}
}

func TestDisconnectedPrivateRelayStartsWithWANAttempt(t *testing.T) {
	for _, private := range []bool{false, true} {
		name := "legacy remains serial"
		if private {
			name = "private relay does not wait for WAN timeout"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				cfg := config.Wrap("", config.Configuration{Options: config.OptionsConfiguration{AlwaysLocalNets: []string{"10.20.30.0/24"}}}, protocol.LocalDeviceID, events.NoopLogger)
				s := &service{lanChecker: &lanChecker{cfg: cfg}, connectionStatusHandler: newConnectionStatusHandler()}
				if private {
					s.options.Lookup = func(context.Context, protocol.DeviceID) ([]string, error) { return nil, nil }
					s.options.SingleWANConnection = func() bool { return true }
				}
				attempts := make(chan string, 3)
				releaseLAN, releaseWAN := make(chan struct{}), make(chan struct{})
				s.options.BeforeDial = func(ctx context.Context, _ protocol.DeviceID, addr string, _ bool) (context.Context, func(), error) {
					attempts <- addr
					switch addr {
					case "tcp://10.20.30.1:22000":
						<-releaseLAN
					case "tcp://192.0.2.1:22000":
						<-releaseWAN
					}
					return nil, nil, errors.New("test dial failure")
				}
				target := func(addr string, priority int) dialTarget {
					u, err := url.Parse(addr)
					if err != nil {
						t.Fatal(err)
					}
					return dialTarget{addr: addr, uri: u, priority: priority}
				}
				targets := []dialTarget{target("tcp://10.20.30.1:22000", 10), target("tcp://192.0.2.1:22000", 30), target("private-relay://192.0.2.2:22067", 50)}
				done := make(chan struct{})
				go func() {
					defer close(done)
					s.dialParallel(context.Background(), protocol.LocalDeviceID, targets, semaphore.New(8))
				}()
				synctest.Wait()
				if len(attempts) != 1 || <-attempts != targets[0].addr {
					t.Error("LAN must get its original first attempt")
				}
				close(releaseLAN)
				synctest.Wait()
				want := 1
				if private {
					want = 2
				}
				if len(attempts) != want {
					t.Errorf("got %d WAN attempts before direct completed, want %d", len(attempts), want)
				}
				close(releaseWAN)
				<-done
				if targets[2].priority != 50 {
					t.Error("relay connection priority changed")
				}
			})
		})
	}
}

func TestHintChangeCancelsOnlyObservedAttempt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a, b := make(chan struct{}), make(chan struct{})
		ctx, stop := cancelOnHintsChanged(context.Background(), a)
		defer stop()
		other, stopOther := cancelOnHintsChanged(context.Background(), b)
		defer stopOther()
		done := make(chan struct{})
		go func() { <-ctx.Done(); close(done) }()
		synctest.Wait()
		close(a)
		synctest.Wait()
		select {
		case <-done:
		default:
			t.Fatal("blocked dial not interrupted")
		}
		if other.Err() != nil {
			t.Fatal("unrelated peer canceled")
		}
	})
}

func TestHintWatcherStopAndAlreadyPublished(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		changed := make(chan struct{})
		close(changed)
		ctx, stop := cancelOnHintsChanged(context.Background(), changed)
		synctest.Wait()
		if ctx.Err() == nil {
			t.Fatal("publication between subscribe and dial lost")
		}
		stop()
		ctx, stop = cancelOnHintsChanged(context.Background(), make(chan struct{}))
		stop()
		if ctx.Err() == nil {
			t.Fatal("attempt context retained")
		}
	})
}

func TestHintUpdateWakesSleepingAndActiveDialLoop(t *testing.T) {
	for _, active := range []bool{false, true} {
		t.Run(fmt.Sprint(active), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				peer := protocol.NewDeviceID([]byte("peer"))
				other := protocol.NewDeviceID([]byte("other"))
				otherChanged := make(chan struct{})
				var otherCanceled atomic.Bool
				wrapped := config.Wrap("", config.Configuration{}, protocol.LocalDeviceID, events.NoopLogger)
				ctx, cancel := context.WithCancel(context.Background())
				configDone := make(chan struct{})
				go func() { defer close(configDone); _ = wrapped.Serve(ctx) }()
				s := &service{cfg: wrapped, myID: protocol.LocalDeviceID, model: dialLoopModel{}, dialNow: make(chan struct{}, 1), dialNowDevices: make(map[protocol.DeviceID]struct{}), connectionStatusHandler: newConnectionStatusHandler(), lanChecker: &lanChecker{cfg: wrapped}}
				changed, next := make(chan struct{}), make(chan struct{})
				var published atomic.Bool
				var oldCalls, newCalls atomic.Int32
				s.options.LookupChanged = func(id protocol.DeviceID) <-chan struct{} {
					if id == other {
						return otherChanged
					}
					if published.Load() {
						return next
					}
					return changed
				}
				s.options.Lookup = func(_ context.Context, id protocol.DeviceID) ([]string, error) {
					if id == other {
						return []string{"tcp://192.0.2.3:22000"}, nil
					}
					out := []string{"tcp://192.0.2.1:22000"}
					if published.Load() {
						out = append(out, "private-relay://192.0.2.2:22067")
					}
					return out, nil
				}
				s.options.BeforeDial = func(ctx context.Context, _ protocol.DeviceID, addr string, _ bool) (context.Context, func(), error) {
					if addr == "tcp://192.0.2.3:22000" {
						<-ctx.Done()
						otherCanceled.Store(true)
						return nil, nil, ctx.Err()
					}
					if addr == "tcp://192.0.2.1:22000" {
						oldCalls.Add(1)
						if active {
							<-ctx.Done()
						}
					} else {
						newCalls.Add(1)
					}
					return nil, nil, errors.New("test admission rejected")
				}
				done := make(chan struct{})
				go func() { defer close(done); _ = s.connect(ctx) }()
				defer func() { cancel(); <-done; <-configDone }()
				time.Sleep(300 * time.Second)
				synctest.Wait()
				from := wrapped.RawCopy()
				w, err := wrapped.Modify(func(c *config.Configuration) {
					c.Devices = []config.DeviceConfiguration{{DeviceID: peer}, {DeviceID: other}}
				})
				if err != nil {
					t.Fatal(err)
				}
				w.Wait()
				s.checkAndSignalConnectLoopOnUpdatedDevices(from, wrapped.RawCopy())
				synctest.Wait()
				if oldCalls.Load() != 1 {
					t.Fatal(oldCalls.Load())
				}
				published.Store(true)
				close(changed)
				synctest.Wait()
				if otherCanceled.Load() {
					t.Fatal("other peer canceled")
				}
				if newCalls.Load() != 1 {
					t.Fatal("new candidate remained blocked", newCalls.Load())
				}
				if oldCalls.Load() != 1 {
					t.Fatal("candidate wake bypassed old address backoff", oldCalls.Load())
				}
			})
		})
	}
}

func TestSequentialPeerHintUpdatesDoNotReopenIdleBatch(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a, b := protocol.NewDeviceID([]byte("a")), protocol.NewDeviceID([]byte("b"))
		wrapped := config.Wrap("", config.Configuration{}, protocol.LocalDeviceID, events.NoopLogger)
		ctx, cancel := context.WithCancel(context.Background())
		configDone := make(chan struct{})
		go func() { defer close(configDone); _ = wrapped.Serve(ctx) }()
		s := &service{cfg: wrapped, myID: protocol.LocalDeviceID, model: dialLoopModel{}, dialNow: make(chan struct{}, 1), dialNowDevices: make(map[protocol.DeviceID]struct{}), connectionStatusHandler: newConnectionStatusHandler(), lanChecker: &lanChecker{cfg: wrapped}}
		ca, cb, ga, gb, never := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
		var phase atomic.Int32
		var aNew, bNew atomic.Int32
		var aCanceled atomic.Bool
		s.options.LookupChanged = func(id protocol.DeviceID) <-chan struct{} {
			p := phase.Load()
			if id == a {
				if p == 0 {
					return ca
				}
				return never
			}
			if id == b {
				if p < 2 {
					return cb
				}
				return never
			}
			if p == 0 {
				return ga
			}
			if p == 1 {
				return gb
			}
			return never
		}
		s.options.Lookup = func(_ context.Context, id protocol.DeviceID) ([]string, error) {
			if id == a {
				v := []string{"tcp://192.0.2.1:22000"}
				if phase.Load() > 0 {
					v = append(v, "tcp://192.0.2.11:22000")
				}
				return v, nil
			}
			v := []string{"tcp://192.0.2.2:22000"}
			if phase.Load() > 1 {
				v = append(v, "tcp://192.0.2.12:22000")
			}
			return v, nil
		}
		s.options.BeforeDial = func(ctx context.Context, _ protocol.DeviceID, addr string, _ bool) (context.Context, func(), error) {
			switch addr {
			case "tcp://192.0.2.1:22000":
				<-ctx.Done()
			case "tcp://192.0.2.11:22000":
				aNew.Add(1)
				<-ctx.Done()
				aCanceled.Store(true)
			case "tcp://192.0.2.12:22000":
				bNew.Add(1)
			}
			return nil, nil, errors.New("injected rejection")
		}
		done := make(chan struct{})
		go func() { defer close(done); _ = s.connect(ctx) }()
		defer func() { cancel(); <-done; <-configDone }()
		time.Sleep(300 * time.Second)
		synctest.Wait()
		from := wrapped.RawCopy()
		w, err := wrapped.Modify(func(c *config.Configuration) { c.Devices = []config.DeviceConfiguration{{DeviceID: a}, {DeviceID: b}} })
		if err != nil {
			t.Fatal(err)
		}
		w.Wait()
		s.checkAndSignalConnectLoopOnUpdatedDevices(from, wrapped.RawCopy())
		synctest.Wait()
		phase.Store(1)
		close(ca)
		close(ga)
		synctest.Wait()
		if aNew.Load() != 1 {
			t.Fatal("A not redialed", aNew.Load())
		}
		phase.Store(2)
		close(cb)
		close(gb)
		synctest.Wait()
		if bNew.Load() != 1 || aCanceled.Load() {
			t.Fatal("B stranded behind restarted A", bNew.Load(), aCanceled.Load())
		}
	})
}
