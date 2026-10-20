package connections

import (
	"context"
	"errors"
	"github.com/syncthing/syncthing/lib/config"
	"github.com/syncthing/syncthing/lib/protocol"
	"net/url"
	"testing"
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
