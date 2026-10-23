// Copyright (C) 2026 The Syncthing Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package client

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	native "github.com/syncthing/syncthing/lib/protocol"
	"github.com/syncthing/syncthing/lib/relay/protocol"
	"math/big"
	"net"
	"net/url"
	"testing"
	"time"
)

func TestPrivateRegistrationCancelAndIdentityPin(t *testing.T) {
	cert := func() tls.Certificate {
		pub, key, _ := ed25519.GenerateKey(rand.Reader)
		template := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature}
		raw, err := x509.CreateCertificate(rand.Reader, template, template, pub, key)
		if err != nil {
			t.Fatal(err)
		}
		return tls.Certificate{Certificate: [][]byte{raw}, PrivateKey: key}
	}
	serverCert, clientCert := cert(), cert()
	for _, mode := range []string{"cancel-drain", "wrong-pin", "renewal-failure", "renewal-rejected", "automatic-renewal", "cancel-renewal", "renewal-deadline"} {
		badPin := mode == "wrong-pin"
		t.Run(mode, func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			ready := make(chan struct{})
			renewed := make(chan struct{})
			issuerEntered := make(chan struct{})
			serverDone := make(chan error, 1)
			go func() {
				conn, err := listener.Accept()
				if err != nil {
					serverDone <- err
					return
				}
				defer conn.Close()
				tc := tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{serverCert}, ClientAuth: tls.RequireAnyClientCert, NextProtos: []string{protocol.ControlledProtocolName}, MinVersion: tls.VersionTLS12})
				tc.SetDeadline(time.Now().Add(5 * time.Second))
				msg, err := protocol.ReadMessage(tc)
				if err != nil {
					serverDone <- err
					return
				}
				join, ok := msg.(protocol.PrivateJoinRequest)
				if !ok || len(join.Permit) == 0 {
					serverDone <- protocol.ErrPermit
					return
				}
				if err = protocol.WriteMessage(tc, protocol.ResponseSuccess); err != nil {
					serverDone <- err
					return
				}
				close(ready)
				if mode == "automatic-renewal" {
					for range 3 {
						expiry, e := privatePermitExpiry(join.Permit, time.Now())
						if e != nil {
							serverDone <- e
							return
						}
						// A renewal must arrive before the currently accepted permit
						// expires, including after the issuer changes its lifetime.
						tc.SetReadDeadline(expiry)
						message, e := protocol.ReadMessage(tc)
						if e != nil {
							serverDone <- e
							return
						}
						var ok bool
						join, ok = message.(protocol.PrivateJoinRequest)
						if !ok {
							serverDone <- protocol.ErrPermit
							return
						}
						if e := protocol.WriteMessage(tc, protocol.ResponseSuccess); e != nil {
							serverDone <- e
							return
						}
					}
					close(renewed)
				}
				if mode == "renewal-rejected" {
					msg, e := protocol.ReadMessage(tc)
					if e != nil {
						serverDone <- e
						return
					}
					if _, ok := msg.(protocol.PrivateJoinRequest); !ok {
						serverDone <- protocol.ErrPermit
						return
					}
					if e := protocol.WriteMessage(tc, protocol.ResponseNotFound); e != nil {
						serverDone <- e
						return
					}
				}
				_, err = protocol.ReadMessage(tc)
				serverDone <- err
			}()
			pin := native.NewDeviceID(serverCert.Certificate[0])
			if badPin {
				pin = native.NewDeviceID([]byte("wrong"))
			}
			uri, _ := url.Parse("relay://" + listener.Addr().String() + "?id=" + pin.String())
			calls := 0
			cl, err := NewPrivateClient(uri, []tls.Certificate{clientCert}, time.Second, func(permitCtx context.Context) (protocol.PermitToken, error) {
				calls++
				if mode == "renewal-failure" && calls > 1 {
					return "", errors.New("injected issuer failure")
				}
				if (mode == "cancel-renewal" || mode == "renewal-deadline") && calls > 1 {
					close(issuerEntered)
					<-permitCtx.Done()
					return "", permitCtx.Err()
				}
				ttl := 60 * time.Second
				if mode == "renewal-deadline" {
					ttl = 2 * time.Second
				}
				if mode == "automatic-renewal" {
					ttl = []time.Duration{2 * time.Second, 4 * time.Second, time.Second, 60 * time.Second}[calls-1]
				}
				return testJoinPermit(time.Now(), ttl), nil
			})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			renew := make(chan time.Time, 1)
			if mode == "automatic-renewal" {
				renew = nil
			}
			go func() { done <- cl.(*staticClient).serveWithRenewal(ctx, renew) }()
			if !badPin {
				select {
				case <-ready:
				case <-time.After(3 * time.Second):
					t.Fatal("registration not acknowledged")
				}
				switch mode {
				case "cancel-drain":
					cancel()
				case "automatic-renewal":
					select {
					case <-renewed:
					case err := <-serverDone:
						t.Fatalf("renewal did not precede expiry: %v", err)
					case <-time.After(6 * time.Second):
						t.Fatal("automatic renewal stalled")
					}
					cancel()
				case "cancel-renewal":
					renew <- time.Now()
					select {
					case <-issuerEntered:
					case <-time.After(time.Second):
						t.Fatal("issuer not called")
					}
					cancel()
				default:
					renew <- time.Now()
				}
			}
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("expected stopped or rejected client")
				}
				if mode == "renewal-deadline" && !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("issuer was not bounded by permit expiry: %v", err)
				}
				if mode == "cancel-renewal" && !errors.Is(err, context.Canceled) {
					t.Fatalf("issuer cancellation: %v", err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("client did not drain")
			}
			if cl.URI() != nil {
				t.Fatal("failed registration kept publishing relay")
			}
			select {
			case err := <-serverDone:
				if err == nil {
					t.Fatal("connection remained open")
				}
			case <-time.After(3 * time.Second):
				t.Fatal("server reader did not drain")
			}
		})
	}
}

func testJoinPermit(now time.Time, ttl time.Duration) protocol.PermitToken {
	raw, _ := json.Marshal(protocol.Permit{Purpose: "join", IssuedAt: now.Unix(), ExpiresAt: now.Add(ttl).Unix()})
	return protocol.PermitToken(base64.RawURLEncoding.EncodeToString(raw) + ".test-signature")
}

func TestPrivatePermitRenewalSchedule(t *testing.T) {
	now := time.Unix(1000, 0)
	for _, ttl := range []time.Duration{time.Second, 30 * time.Second, 2 * time.Minute, 5 * time.Minute} {
		expiry, err := privatePermitExpiry(testJoinPermit(now, ttl), now)
		if err != nil {
			t.Fatal(err)
		}
		delay, err := permitRenewalDelay(expiry, now)
		if err != nil || delay <= 0 || delay >= ttl {
			t.Fatalf("ttl=%v delay=%v err=%v", ttl, delay, err)
		}
		want := ttl - min(ttl/2, 30*time.Second)
		if delay != want {
			t.Fatalf("got %v want %v", delay, want)
		}
	}
	for _, token := range []protocol.PermitToken{"", "malformed", testJoinPermit(now, -time.Second), testJoinPermit(now, 301*time.Second)} {
		if _, err := privatePermitExpiry(token, now); !errors.Is(err, protocol.ErrPermit) {
			t.Fatalf("accepted invalid expiry: %v", err)
		}
	}
	if _, err := permitRenewalDelay(now, now); !errors.Is(err, protocol.ErrPermit) {
		t.Fatal("accepted expired permit")
	}
}
