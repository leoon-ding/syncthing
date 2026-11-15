// Copyright (C) 2026 The Syncthing Authors.
// Licensed under the Mozilla Public License, v. 2.0.

//go:build !noquic

package connections

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/syncthing/syncthing/lib/protocol"
	relayprotocol "github.com/syncthing/syncthing/lib/relay/protocol"
	"github.com/syncthing/syncthing/lib/tlsutil"
)

func TestSTUNTransportCancellationPreservesQUICSocket(t *testing.T) {
	socket, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer socket.Close()
	transport := &quic.Transport{Conn: socket}
	defer transport.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	conn := &transportPacketConn{tran: transport, ctx: ctx}
	_ = conn.SetReadDeadline(time.Now().Add(time.Hour))
	done := make(chan error, 1)
	go func() { _, _, err := conn.ReadFrom(make([]byte, 1024)); done <- err }()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("STUN read did not stop")
	}
	if _, err := conn.WriteTo([]byte("canceled"), socket.LocalAddr()); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	// The STUN lifetime must not own or close the shared QUIC transport.
	if _, err := transport.WriteTo([]byte("still open"), socket.LocalAddr()); err != nil {
		t.Fatal(err)
	}
}

func privateQUICCheckerForTest() *lanChecker {
	return &lanChecker{privateConnect: func(context.Context, protocol.DeviceID, *url.URL) (relayprotocol.PermitToken, error) {
		return relayprotocol.PermitToken(""), nil
	}}
}

func TestQUICPrivatePacketPolicyIsolation(t *testing.T) {
	for _, checker := range []*lanChecker{nil, {}, {stunServers: func() []string { return nil }}} {
		cfg := quicConfigForRuntime(checker)
		if cfg.DisablePathMTUDiscovery || cfg.InitialPacketSize != 0 {
			t.Fatal("standalone MTU discovery changed")
		}
	}
	first := quicConfigForRuntime(privateQUICCheckerForTest())
	if !first.DisablePathMTUDiscovery || first.InitialPacketSize != 1200 {
		t.Fatal("private path must remain at the base packet size")
	}
	if first.MaxIdleTimeout != quicConfig.MaxIdleTimeout || first.KeepAlivePeriod != quicConfig.KeepAlivePeriod {
		t.Fatal("private packet policy changed connection lifetime")
	}
	first.InitialPacketSize = 1400
	if quicConfigForRuntime(privateQUICCheckerForTest()).InitialPacketSize != 1200 || quicConfig.InitialPacketSize != 0 {
		t.Fatal("private config mutation escaped its connection")
	}
}

// baseSizePacketConn deterministically models a path that discards datagrams
// above QUIC's base size. The counter catches probes even if data still succeeds.
type baseSizePacketConn struct {
	net.PacketConn
	oversized atomic.Int32
}

func (c *baseSizePacketConn) WriteTo(p []byte, addr net.Addr) (int, error) {
	if len(p) > 1200 {
		c.oversized.Add(1)
		return len(p), nil
	}
	return c.PacketConn.WriteTo(p, addr)
}

func TestQUICPrivateBidirectionalTransferOnBaseSizePath(t *testing.T) {
	cert, err := tlsutil.NewCertificateInMemory("test", 1)
	if err != nil {
		t.Fatal(err)
	}
	newSocket := func() *baseSizePacketConn {
		pc, err := net.ListenPacket("udp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = pc.Close() })
		return &baseSizePacketConn{PacketConn: pc}
	}
	serverSocket, clientSocket := newSocket(), newSocket()
	serverTransport := &quic.Transport{Conn: serverSocket}
	defer serverTransport.Close()
	clientTransport := &quic.Transport{Conn: clientSocket}
	defer clientTransport.Close()
	listener, err := serverTransport.Listen(&tls.Config{Certificates: []tls.Certificate{cert}, NextProtos: []string{"p9-size-test"}}, quicConfigForRuntime(privateQUICCheckerForTest()))
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := clientTransport.Dial(ctx, listener.Addr(), &tls.Config{InsecureSkipVerify: true, NextProtos: []string{"p9-size-test"}}, quicConfigForRuntime(privateQUICCheckerForTest())) // test-only certificate
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseWithError(0, "test complete")
	peer, err := listener.Accept(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer peer.CloseWithError(0, "test complete")
	payload := bytes.Repeat([]byte("base-size-path"), 160000)
	done := make(chan error, 1)
	go func() {
		stream, err := peer.AcceptStream(ctx)
		if err != nil {
			done <- err
			return
		}
		deadline, _ := ctx.Deadline()
		_ = stream.SetDeadline(deadline)
		got := make([]byte, len(payload))
		if _, err = io.ReadFull(stream, got); err == nil && !bytes.Equal(got, payload) {
			err = errors.New("server payload mismatch")
		}
		if err == nil {
			_, err = stream.Write(got)
		}
		done <- err
	}()
	stream, err := conn.OpenStreamSync(ctx)
	if err != nil {
		cancel()
		_ = peer.CloseWithError(0, "test failure")
		<-done
		t.Fatal(err)
	}
	deadline, _ := ctx.Deadline()
	_ = stream.SetDeadline(deadline)
	_, writeErr := stream.Write(payload)
	got := make([]byte, len(payload))
	_, readErr := io.ReadFull(stream, got)
	if writeErr != nil || readErr != nil {
		cancel()
		_ = peer.CloseWithError(0, "test failure")
	}
	serverErr := <-done
	if writeErr != nil || readErr != nil || serverErr != nil {
		t.Fatalf("write=%v read=%v server=%v", writeErr, readErr, serverErr)
	}
	if !bytes.Equal(got, payload) {
		t.Fatal("client payload mismatch")
	}
	if serverSocket.oversized.Load() != 0 || clientSocket.oversized.Load() != 0 {
		t.Fatal("private path emitted oversized datagrams")
	}
}
