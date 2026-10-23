// Copyright (C) 2016 The Syncthing Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package connections

import (
	"context"
	"crypto/tls"
	"errors"
	"log/slog"
	"net/url"
	"sync"
	"time"

	"github.com/syncthing/syncthing/internal/slogutil"
	"github.com/syncthing/syncthing/lib/config"
	"github.com/syncthing/syncthing/lib/connections/registry"
	"github.com/syncthing/syncthing/lib/dialer"
	"github.com/syncthing/syncthing/lib/nat"
	"github.com/syncthing/syncthing/lib/relay/client"
	relayprotocol "github.com/syncthing/syncthing/lib/relay/protocol"
	"github.com/syncthing/syncthing/lib/svcutil"
)

func init() {
	factory := &relayListenerFactory{}
	listeners["relay"] = factory
	listeners["dynamic+http"] = factory
	listeners["dynamic+https"] = factory
}

type relayListener struct {
	privateClient client.RelayClient
	svcutil.ServiceWithError
	onAddressesChangedNotifier

	uri     *url.URL
	cfg     config.Wrapper
	tlsCfg  *tls.Config
	conns   chan internalConn
	factory listenerFactory

	client client.RelayClient
	mut    sync.RWMutex
}

func (t *relayListener) serve(ctx context.Context) error {
	clnt := t.privateClient
	var err error
	if clnt == nil {
		clnt, err = client.NewClient(t.uri, t.tlsCfg.Certificates, 10*time.Second)
	}
	if err != nil {
		slog.WarnContext(ctx, "Failed to listen (relay)", slogutil.Error(err))
		return err
	}

	t.mut.Lock()
	t.client = clnt
	t.mut.Unlock()

	slog.InfoContext(ctx, "Relay listener starting", "id", t.String())
	defer slog.InfoContext(ctx, "Relay listener shutting down", "id", t.String())
	defer t.clearAddresses(t)

	invitationCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	defer func() { cancel(); <-done }()
	go func() { defer close(done); t.handleInvitations(invitationCtx, clnt) }()

	return clnt.Serve(ctx)
}

func (t *relayListener) handleInvitations(ctx context.Context, clnt client.RelayClient) {
	invitations := clnt.Invitations()

	// Start with nil, so that we send a addresses changed notification as soon as we connect somewhere.
	var oldURI *url.URL

	for {
		select {
		case inv := <-invitations:
			conn, err := client.JoinSession(ctx, inv)
			if err != nil {
				if !errors.Is(err, context.Canceled) {
					slog.InfoContext(ctx, "Failed to join session", slogutil.Error(err))
				}
				continue
			}

			err = dialer.SetTCPOptions(conn)
			if err != nil {
				slog.DebugContext(ctx, "Failed to set TCP options", slogutil.Error(err))
			}

			err = dialer.SetTrafficClass(conn, t.cfg.Options().TrafficClass)
			if err != nil {
				slog.DebugContext(ctx, "Failed to set traffic class", slogutil.Error(err))
			}

			var tc *tls.Conn
			if inv.ServerSocket {
				tc = tls.Server(conn, t.tlsCfg)
			} else {
				tc = tls.Client(conn, t.tlsCfg)
			}

			handshakeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			err = tc.HandshakeContext(handshakeCtx)
			cancel()
			if err != nil {
				tc.Close()
				slog.WarnContext(ctx, "Failed TLS handshake", slogutil.Error(err))
				continue
			}

			select {
			case t.conns <- newInternalConn(tc, connTypeRelayServer, false, t.cfg.Options().ConnectionPriorityRelay):
			case <-ctx.Done():
				tc.Close()
				return
			}

		// Poor mans notifier that informs the connection service that the
		// relay URI has changed. This can only happen when we connect to a
		// relay via dynamic+http(s) pool, which upon a relay failing/dropping
		// us, would pick a different one.
		case <-time.After(10 * time.Second):
			currentURI := clnt.URI()
			if currentURI != oldURI {
				oldURI = currentURI
				t.notifyAddressesChanged(t)
			}

		case <-ctx.Done():
			return
		}
	}
}

func (t *relayListener) URI() *url.URL {
	return t.uri
}

func (t *relayListener) WANAddresses() []*url.URL {
	t.mut.RLock()
	client := t.client
	t.mut.RUnlock()

	if client == nil {
		return nil
	}

	curi := client.URI()
	if curi == nil {
		return nil
	}

	if t.privateClient != nil {
		snapshot := *curi
		snapshot.Scheme = "private-relay"
		curi = &snapshot
	}
	return []*url.URL{curi}
}

func (t *relayListener) LANAddresses() []*url.URL {
	return t.WANAddresses()
}

func (t *relayListener) Error() error {
	err := t.ServiceWithError.Error()
	if err != nil {
		return err
	}
	t.mut.RLock()
	defer t.mut.RUnlock()
	if t.client != nil {
		return t.client.Error()
	}
	return nil
}

func (t *relayListener) Factory() listenerFactory {
	return t.factory
}

func (t *relayListener) String() string {
	return t.uri.String()
}

func (*relayListener) NATType() string {
	return "unknown"
}

type relayListenerFactory struct{}

func (f *relayListenerFactory) New(uri *url.URL, cfg config.Wrapper, tlsCfg *tls.Config, conns chan internalConn, _ *nat.Service, _ *registry.Registry, _ *lanChecker) genericListener {
	t := &relayListener{
		uri:     uri,
		cfg:     cfg,
		tlsCfg:  tlsCfg,
		conns:   conns,
		factory: f,
	}
	t.ServiceWithError = svcutil.AsService(t.serve, t.String())
	return t
}

func (relayListenerFactory) Valid(cfg config.Configuration) error {
	if !cfg.Options.RelaysEnabled {
		return errDisabled
	}
	return nil
}

// servePrivateRelay borrows one embedding run while preserving the native
// listener/session implementation. No endpoint is added to persisted config.
func (s *service) servePrivateRelay(ctx context.Context) error {
	if s.networkCtx != nil {
		scope, cancel := context.WithCancel(ctx)
		stop := context.AfterFunc(s.networkCtx, cancel) //nolint:contextcheck // Merge the run fence with the caller-derived scope; neither owner may outlive shutdown.
		defer stop()
		defer cancel()
		ctx = scope
	}

	backoff := time.Second
	for ctx.Err() == nil {
		scope, uri, token, release, err := s.options.PrivateRelay(ctx)
		if err == nil && scope != nil && uri != nil {
			run, cancel := context.WithCancel(ctx)
			stop := context.AfterFunc(scope, cancel)
			first := true
			cl, e := client.NewPrivateClient(uri, s.tlsCfg.Certificates, 10*time.Second, func(call context.Context) (relayprotocol.PermitToken, error) {
				if first {
					first = false
					return token, nil
				}
				return s.options.PrivateRenew(call, uri)
			})
			if e == nil {
				listener := &relayListener{uri: uri, cfg: s.cfg, tlsCfg: s.tlsCfg, conns: s.conns, privateClient: cl}
				s.listenersMut.Lock()
				s.privateRelay = listener
				s.listenersMut.Unlock()
				_ = listener.serve(run)
				s.listenersMut.Lock()
				if s.privateRelay == listener {
					s.privateRelay = nil
				}
				s.listenersMut.Unlock()
			}
			stop()
			cancel()
		}
		if release != nil {
			release()
		}
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
		if backoff < time.Minute {
			backoff *= 2
			if backoff > time.Minute {
				backoff = time.Minute
			}
		}
	}
	return ctx.Err()
}
