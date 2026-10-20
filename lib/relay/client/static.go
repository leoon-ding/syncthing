// Copyright (C) 2015 Audrius Butkevicius and Contributors (see the CONTRIBUTORS file).

package client

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"sync/atomic"
	"time"

	"github.com/syncthing/syncthing/internal/slogutil"
	"github.com/syncthing/syncthing/lib/dialer"
	"github.com/syncthing/syncthing/lib/osutil"
	syncthingprotocol "github.com/syncthing/syncthing/lib/protocol"
	"github.com/syncthing/syncthing/lib/relay/protocol"
)

type staticClient struct {
	joined atomic.Bool
	commonClient

	uri *url.URL

	config *tls.Config

	messageTimeout time.Duration
	connectTimeout time.Duration

	conn   *tls.Conn
	token  string
	permit func(context.Context) (protocol.PermitToken, error)
}

func newStaticClient(uri *url.URL, certs []tls.Certificate, invitations chan protocol.SessionInvitation, timeout time.Duration) *staticClient {
	c := &staticClient{
		uri: uri,

		config: configForCerts(certs),

		messageTimeout: time.Minute * 2,
		connectTimeout: timeout,

		token: uri.Query().Get("token"),
	}
	c.commonClient = newCommonClient(invitations, c.serve, c.String())
	return c
}

func (c *staticClient) serve(ctx context.Context) error {
	return c.serveWithRenewal(ctx, nil)
}

// A supplied renewal clock permits deterministic lifecycle verification.
func (c *staticClient) serveWithRenewal(ctx context.Context, renew <-chan time.Time) error {
	if err := c.connect(ctx); err != nil {
		l.Debugf("Could not connect to relay %s: %s", c.uri, err)
		return err
	}

	owned := c.conn
	cancelDone := make(chan struct{})
	stopCancel := context.AfterFunc(ctx, func() { owned.Close(); close(cancelDone) })
	defer func() {
		if !stopCancel() {
			<-cancelDone
		}
	}()
	l.Debugln(c, "connected", c.conn.RemoteAddr())
	defer c.disconnect()

	if err := c.join(ctx); err != nil {
		l.Debugf("Could not join relay %s: %s", c.uri, err)
		return err
	}

	c.joined.Store(true)
	defer c.joined.Store(false)
	if err := c.conn.SetDeadline(time.Time{}); err != nil {
		l.Debugln("Relay set deadline:", err)
		return err
	}

	slog.InfoContext(ctx, "Joined relay", slogutil.URI(fmt.Sprintf("%s://%s", c.uri.Scheme, c.uri.Host)))

	messages := make(chan interface{})
	errorsc := make(chan error, 1)

	readerCtx, cancelReader := context.WithCancel(ctx)
	readerDone := make(chan struct{})
	go func() { defer close(readerDone); messageReader(readerCtx, c.conn, messages, errorsc) }()
	defer func() { cancelReader(); c.conn.Close(); <-readerDone }()

	timeout := time.NewTimer(c.messageTimeout)
	defer timeout.Stop()
	if c.permit != nil && renew == nil {
		ticker := time.NewTicker(2 * time.Minute)
		defer ticker.Stop()
		renew = ticker.C
	}

	for {
		select {
		case message := <-messages:
			timeout.Reset(c.messageTimeout)
			l.Debugf("%s received message %T", c, message)

			switch msg := message.(type) {
			case protocol.Ping:
				if err := protocol.WriteMessage(c.conn, protocol.Pong{}); err != nil {
					l.Debugln("Relay write:", err)
					return err
				}
				l.Debugln(c, "sent pong")

			case protocol.SessionInvitation:
				ip := net.IP(msg.Address)
				if len(ip) == 0 || ip.IsUnspecified() {
					msg.Address, _ = osutil.IPFromAddr(c.conn.RemoteAddr())
				}
				select {
				case c.invitations <- msg:
				case <-ctx.Done():
					l.Debugln(c, "stopping")
					return ctx.Err()
				}

			case protocol.Response:
				if c.permit == nil || msg.Code != 0 {
					return errors.New("relay renewal rejected")
				}
			case protocol.RelayFull:
				l.Debugf("Disconnected from relay %s due to it becoming full.", c.uri)
				return errors.New("relay full")

			default:
				l.Debugf("Relay: protocol error: unexpected message %v", msg)
				return fmt.Errorf("protocol error: unexpected message %v", msg)
			}

		case <-renew:
			token, err := c.permit(ctx)
			if err != nil {
				return err
			}
			if err := c.conn.SetWriteDeadline(time.Now().Add(c.connectTimeout)); err != nil {
				return err
			}
			if err := protocol.WriteMessage(c.conn, protocol.PrivateJoinRequest{Permit: token}); err != nil {
				return err
			}
			if err := c.conn.SetWriteDeadline(time.Time{}); err != nil {
				return err
			}
		case <-ctx.Done():
			l.Debugln(c, "stopping")
			return ctx.Err()

		case err := <-errorsc:
			l.Debugf("Disconnecting from relay %s due to error: %s", c.uri, err)
			return err

		case <-timeout.C:
			l.Debugln(c, "timed out")
			return errors.New("timed out")
		}
	}
}

func (c *staticClient) String() string {
	return fmt.Sprintf("StaticClient:%p@%s", c, c.URI())
}

func (c *staticClient) URI() *url.URL {
	if c.permit != nil && !c.joined.Load() {
		return nil
	}
	return c.uri
}

func (c *staticClient) connect(ctx context.Context) error {
	if c.uri.Scheme != "relay" {
		return fmt.Errorf("unsupported relay scheme: %v", c.uri.Scheme)
	}

	timeoutCtx, cancel := context.WithTimeout(ctx, c.connectTimeout)
	defer cancel()
	tcpConn, err := dialer.DialContext(timeoutCtx, "tcp", c.uri.Host)
	if err != nil {
		return err
	}

	stopCancel := context.AfterFunc(timeoutCtx, func() { tcpConn.Close() })
	defer stopCancel()
	// Copy the TLS config and set the server name we're connecting to. In
	// many cases this will be an IP address, in which case it's a no-op. In
	// other cases it will be a hostname, which will cause the TLS stack to
	// send SNI.
	cfg := c.config
	if host, _, err := net.SplitHostPort(c.uri.Host); err == nil {
		cfg = cfg.Clone()
		cfg.ServerName = host
	}

	conn := tls.Client(tcpConn, cfg)

	if err := conn.SetDeadline(time.Now().Add(c.connectTimeout)); err != nil {
		conn.Close()
		return err
	}

	if err := performHandshakeProtocol(conn, c.uri, c.config.NextProtos[0]); err != nil {
		conn.Close()
		return err
	}

	c.conn = conn
	return nil
}

func (c *staticClient) disconnect() {
	l.Debugln(c, "disconnecting")
	c.conn.Close()
}

func (c *staticClient) join(ctx context.Context) error {
	var request interface{} = protocol.JoinRelayRequest{Token: c.token}
	if c.permit != nil {
		token, err := c.permit(ctx)
		if err != nil {
			return err
		}
		request = protocol.PrivateJoinRequest{Permit: token}
	}
	if err := protocol.WriteMessage(c.conn, request); err != nil {
		return err
	}

	message, err := protocol.ReadMessage(c.conn)
	if err != nil {
		return err
	}

	switch msg := message.(type) {
	case protocol.Response:
		if msg.Code != 0 {
			return &incorrectResponseCodeErr{msg.Code, msg.Message}
		}

	case protocol.RelayFull:
		return errors.New("relay full")

	default:
		return fmt.Errorf("protocol error: expecting response got %v", msg)
	}

	return nil
}

func performHandshakeAndValidation(conn *tls.Conn, uri *url.URL) error {
	return performHandshakeProtocol(conn, uri, protocol.ProtocolName)
}
func performHandshakeProtocol(conn *tls.Conn, uri *url.URL, alpn string) error {
	if alpn == protocol.ControlledProtocolName && uri.Query().Get("id") == "" {
		return errors.New("private relay requires identity pin")
	}
	if err := conn.Handshake(); err != nil {
		return err
	}

	cs := conn.ConnectionState()
	if cs.NegotiatedProtocol != alpn {
		return errors.New("protocol negotiation error")
	}

	q := uri.Query()
	relayIDs := q.Get("id")
	if relayIDs != "" {
		relayID, err := syncthingprotocol.DeviceIDFromString(relayIDs)
		if err != nil {
			return fmt.Errorf("relay address contains invalid verification id: %w", err)
		}

		certs := cs.PeerCertificates
		if cl := len(certs); cl != 1 {
			return fmt.Errorf("unexpected certificate count: %d", cl)
		}

		remoteID := syncthingprotocol.NewDeviceID(certs[0].Raw)
		if remoteID != relayID {
			return fmt.Errorf("relay id does not match. Expected %v got %v", relayID, remoteID)
		}
	}

	return nil
}

func messageReader(ctx context.Context, conn net.Conn, messages chan<- interface{}, errors chan<- error) {
	for {
		msg, err := protocol.ReadMessage(conn)
		if err != nil {
			errors <- err
			return
		}
		select {
		case messages <- msg:
		case <-ctx.Done():
			return
		}
	}
}
