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
	"net/url"
	"time"

	"github.com/syncthing/syncthing/lib/config"
	"github.com/syncthing/syncthing/lib/connections/registry"
	"github.com/syncthing/syncthing/lib/dialer"
	"github.com/syncthing/syncthing/lib/protocol"
	"github.com/syncthing/syncthing/lib/relay/client"
	relayprotocol "github.com/syncthing/syncthing/lib/relay/protocol"
)

func init() {
	dialers["relay"] = relayDialerFactory{}
	dialers["private-relay"] = privateRelayDialerFactory{}
}

type relayDialer struct {
	commonDialer
	privateConnect func(context.Context, protocol.DeviceID, *url.URL) (relayprotocol.PermitToken, error)
}

func (d *relayDialer) Dial(ctx context.Context, id protocol.DeviceID, uri *url.URL) (internalConn, error) {
	var inv relayprotocol.SessionInvitation
	var err error
	if uri.Scheme == "private-relay" {
		if d.privateConnect == nil {
			return internalConn{}, errors.New("private relay unavailable")
		}
		token, e := d.privateConnect(ctx, id, uri)
		if e != nil {
			return internalConn{}, e
		}
		target := *uri
		target.Scheme = "relay"
		inv, err = client.GetPrivateInvitationFromRelay(ctx, &target, d.tlsCfg.Certificates, token, 10*time.Second)
	} else {
		inv, err = client.GetInvitationFromRelay(ctx, uri, id, d.tlsCfg.Certificates, 10*time.Second)
	}
	if err != nil {
		return internalConn{}, err
	}

	conn, err := client.JoinSession(ctx, inv)
	if err != nil {
		return internalConn{}, err
	}

	err = dialer.SetTCPOptions(conn)
	if err != nil {
		conn.Close()
		return internalConn{}, err
	}

	err = dialer.SetTrafficClass(conn, d.trafficClass)
	if err != nil {
		l.Debugln("Dial (BEP/relay): setting traffic class:", err)
	}

	var tc *tls.Conn
	if inv.ServerSocket {
		tc = tls.Server(conn, d.tlsCfg)
	} else {
		tc = tls.Client(conn, d.tlsCfg)
	}

	handshakeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	err = tc.HandshakeContext(handshakeCtx)
	cancel()
	if err != nil {
		tc.Close()
		return internalConn{}, err
	}

	return newInternalConn(tc, connTypeRelayClient, false, d.wanPriority), nil
}

func (d *relayDialer) Priority(_ string) int {
	return d.wanPriority
}

type relayDialerFactory struct{}

func (relayDialerFactory) New(opts config.OptionsConfiguration, tlsCfg *tls.Config, _ *registry.Registry, _ *lanChecker) genericDialer {
	return newRelayDialer(opts, tlsCfg)
}

func newRelayDialer(opts config.OptionsConfiguration, tlsCfg *tls.Config) *relayDialer {
	return &relayDialer{commonDialer: commonDialer{
		trafficClass:      opts.TrafficClass,
		reconnectInterval: time.Duration(opts.RelayReconnectIntervalM) * time.Minute,
		tlsCfg:            tlsCfg,
		wanPriority:       opts.ConnectionPriorityRelay,
		lanPriority:       opts.ConnectionPriorityRelay,
	}}
}

func (relayDialerFactory) AlwaysWAN() bool {
	return true
}

func (relayDialerFactory) Valid(cfg config.Configuration) error {
	if !cfg.Options.RelaysEnabled {
		return errDisabled
	}
	return nil
}

func (relayDialerFactory) String() string {
	return "Relay Dialer"
}

type privateRelayDialerFactory struct{ relayDialerFactory }

func (privateRelayDialerFactory) Valid(config.Configuration) error { return nil }
func (privateRelayDialerFactory) New(opts config.OptionsConfiguration, tlsCfg *tls.Config, _ *registry.Registry, checker *lanChecker) genericDialer {
	d := newRelayDialer(opts, tlsCfg)
	// Short grants must not inherit the public relay reconnect delay in minutes.
	d.reconnectInterval = 30 * time.Second
	if checker != nil {
		d.privateConnect = checker.privateConnect
	}
	return d
}
