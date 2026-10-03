// Copyright (C) 2026 The Syncthing Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package connections

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/url"
	"runtime"
	"syscall"
	"testing"
	"time"

	"github.com/syncthing/syncthing/lib/config"
	"github.com/syncthing/syncthing/lib/connections/registry"
	"github.com/syncthing/syncthing/lib/events"
	"github.com/syncthing/syncthing/lib/nat"
	"github.com/syncthing/syncthing/lib/protocol"
	"github.com/syncthing/syncthing/lib/tlsutil"
)

func TestTCPListenerConnectionHandoff(t *testing.T) {
	for _, deliver := range []bool{false, true} {
		name := "cancel"
		if deliver {
			name = "deliver"
		}
		t.Run(name, func(t *testing.T) {
			cert := mustGetCert(t)
			id := protocol.NewDeviceID(cert.Certificate[0])
			tlsCfg := tlsutil.SecureDefaultTLS13()
			tlsCfg.Certificates = []tls.Certificate{cert}
			tlsCfg.InsecureSkipVerify = true
			cfg := config.Wrap("", config.New(id), id, events.NoopLogger)
			reserved, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			addr := reserved.Addr().String()
			if err := reserved.Close(); err != nil {
				t.Fatal(err)
			}
			uri, _ := url.Parse("tcp://" + addr)
			conns := make(chan internalConn)
			listener := (&tcpListenerFactory{}).New(uri, cfg, tlsCfg, conns, nat.NewService(id, cfg), registry.New(), &lanChecker{cfg}).(*tcpListener)
			addresses := make(chan string, 1)
			listener.OnAddressesChanged(func(a ListenerAddresses) {
				if len(a.LANAddresses) > 0 {
					select {
					case addresses <- a.LANAddresses[0].Host:
					default:
					}
				}
			})
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- listener.Serve(ctx) }()
			joined := false
			defer func() {
				cancel()
				if joined {
					return
				}
				timer := time.NewTimer(3 * time.Second)
				defer timer.Stop()
				for {
					select {
					case c := <-conns:
						c.Close()
					case <-done:
						return
					case <-timer.C:
						t.Error("listener cleanup did not finish")
						return
					}
				}
			}()
			select {
			case <-addresses:
			case <-time.After(time.Second):
				t.Fatal("listener did not start")
			}
			client, err := tls.DialWithDialer(&net.Dialer{Timeout: time.Second}, "tcp", addr, tlsCfg)
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			if deliver {
				var server internalConn
				select {
				case server = <-conns:
				case <-time.After(time.Second):
					t.Fatal("connection not delivered")
				}
				defer server.Close()
				cancel()
				select {
				case err := <-done:
					joined = true
					if err != nil && !errors.Is(err, context.Canceled) {
						t.Fatal(err)
					}
				case <-time.After(3 * time.Second):
					t.Fatal("listener did not stop")
				}
				// Successful delivery transfers ownership; stopping the listener must
				// leave this connection usable by its receiver.
				sent := make(chan error, 1)
				go func() { _, err := server.Write([]byte{42}); sent <- err }()
				_ = client.SetReadDeadline(time.Now().Add(time.Second))
				b := make([]byte, 1)
				if _, err := io.ReadFull(client, b); err != nil || b[0] != 42 {
					t.Fatalf("delivered connection unusable: %v", err)
				}
				if err := <-sent; err != nil {
					t.Fatal(err)
				}
			} else {
				cancel()
				select {
				case err := <-done:
					joined = true
					if err != nil && !errors.Is(err, context.Canceled) {
						t.Fatal(err)
					}
				case <-time.After(3 * time.Second):
					t.Fatal("handoff ignored cancellation")
				}
				_ = client.SetReadDeadline(time.Now().Add(time.Second))
				_, err := client.Read(make([]byte, 1))
				// Windows may report WSAECONNRESET instead of EOF when the peer closes.
				reset := runtime.GOOS == "windows" && errors.Is(err, syscall.Errno(10054))
				if !errors.Is(err, io.EOF) && !reset {
					t.Fatalf("unclaimed connection not closed: %v", err)
				}
			}
		})
	}
}
