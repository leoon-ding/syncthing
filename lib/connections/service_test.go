package connections

import (
	"context"
	"errors"
	"github.com/syncthing/syncthing/lib/config"
	"github.com/syncthing/syncthing/lib/events"
	"github.com/syncthing/syncthing/lib/protocol"
	relayprotocol "github.com/syncthing/syncthing/lib/relay/protocol"
	"net/url"
	"sync"
	"testing"
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
	addrs := s.resolveDeviceAddrs(context.Background(), cfg)
	if len(addrs) != 2 || addrs[0] != cfg.Addresses[0] || len(cfg.Addresses) != 1 {
		t.Fatal(addrs, cfg.Addresses)
	}
	s.options.Lookup = func(context.Context, protocol.DeviceID) ([]string, error) { return nil, errors.New("revoked") }
	if addrs = s.resolveDeviceAddrs(context.Background(), cfg); len(addrs) != 1 || addrs[0] != cfg.Addresses[0] {
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

func (c nativePolicyConnection) IsLocal() bool { return c.local }
func (c nativePolicyConnection) Priority() int { return c.priority }
func TestNativeWANConnectionTargetAndHandoff(t *testing.T) {
	id := protocol.NewDeviceID([]byte("native-peer"))
	cfg := config.New(protocol.LocalDeviceID)
	cfg.Devices = append(cfg.Devices, config.DeviceConfiguration{DeviceID: id, RawNumConnections: 3})
	s := &service{cfg: config.Wrap("", cfg, protocol.LocalDeviceID, events.NoopLogger)}
	s.connections = map[protocol.DeviceID][]protocol.Connection{id: {nativePolicyConnection{priority: 50}}}
	s.wantConnections = map[protocol.DeviceID]int{id: 3}
	if got := s.desiredConnectionsToDevice(id); got != 3 {
		t.Fatal("legacy target", got)
	}
	active := true
	s.options.SingleWANConnection = func() bool { return active }
	if got := s.desiredConnectionsToDevice(id); got != 1 {
		t.Fatal("WAN target", got)
	}
	if s.helloForDevice(id, false).NumConnections != 1 || s.helloForDevice(id, true).NumConnections != 3 {
		t.Fatal("Hello policy changed LAN or missed WAN")
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
	if got := s.desiredConnectionsToDevice(id); got != 3 {
		t.Fatal("LAN target changed", got)
	}
	active = false
	if s.helloForDevice(id, false).NumConnections != 3 {
		t.Fatal("legacy policy not restored")
	}
	if s.desiredConnectionsToDevice(protocol.NewDeviceID([]byte("unknown"))) != 0 {
		t.Fatal("unknown peer target")
	}
}
