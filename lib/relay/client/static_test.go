// Copyright (C) 2026 The Syncthing Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package client

import (
	"bytes"
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
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/url"
	"strings"
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
	for _, mode := range []string{"cancel-drain", "issuer-clock-ahead", "issuer-clock-ahead-renewal", "wrong-pin", "renewal-failure", "renewal-rejected", "automatic-renewal", "cancel-renewal", "renewal-deadline"} {
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
				if mode == "issuer-clock-ahead-renewal" {
					msg, e := protocol.ReadMessage(tc)
					if e != nil {
						serverDone <- e
						return
					}
					if _, ok := msg.(protocol.PrivateJoinRequest); !ok {
						serverDone <- protocol.ErrPermit
						return
					}
					if e := protocol.WriteMessage(tc, protocol.ResponseSuccess); e != nil {
						serverDone <- e
						return
					}
					close(renewed)
				}
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
				issued := time.Now()
				if strings.HasPrefix(mode, "issuer-clock-ahead") {
					issued = issued.Add(time.Hour)
				}
				return testJoinPermit(issued, ttl), nil
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
				case "cancel-drain", "issuer-clock-ahead":
					cancel()
				case "issuer-clock-ahead-renewal":
					renew <- time.Now()
					select {
					case <-renewed:
					case err := <-serverDone:
						t.Fatalf("future-issued renewal failed: %v", err)
					case <-time.After(3 * time.Second):
						t.Fatal("future-issued renewal stalled")
					}
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

func TestPrivatePermitRenewalScheduleIssuerClock(t *testing.T) {
	now := time.Unix(1000, 250000000)
	for _, tc := range []struct {
		name   string
		issued time.Time
		ttl    time.Duration
		want   time.Time
	}{
		{"issuer ahead two seconds", now.Add(2 * time.Second), 5 * time.Minute, now.Add(5 * time.Minute)},
		{"issuer far ahead", now.Add(time.Hour), time.Minute, now.Add(time.Minute)},
		{"short future permit", now.Add(2 * time.Second), time.Second, now.Add(time.Second)},
		{"same clock", now, time.Minute, time.Unix(1060, 0)},
		{"aged receipt", now.Add(-40 * time.Second), time.Minute, time.Unix(1020, 0)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			expires, err := privatePermitExpiry(testJoinPermit(tc.issued, tc.ttl), now)
			if err != nil {
				t.Fatalf("valid issuer-clock permit rejected before relay validation: %v", err)
			}
			if !expires.Equal(tc.want) {
				t.Fatalf("expiry = %v, want %v", expires, tc.want)
			}
			delay, err := permitRenewalDelay(expires, now)
			if err != nil || delay <= 0 || delay >= expires.Sub(now) || expires.Sub(now) > tc.ttl {
				t.Fatalf("renewal must stay inside bounded lifetime: expiry=%v delay=%v err=%v", expires, delay, err)
			}
		})
	}
	for _, token := range []protocol.PermitToken{
		testJoinPermit(now.Add(-time.Minute), time.Minute),
		testJoinPermit(now.Add(time.Hour), 301*time.Second),
		testJoinPermit(now.Add(time.Hour), 0),
	} {
		if _, err := privatePermitExpiry(token, now); !errors.Is(err, protocol.ErrPermit) {
			t.Fatalf("accepted invalid or already expired lifetime: %v", err)
		}
	}
}

func TestPrivateSetupFailureDiagnostics(t *testing.T) {
	var output bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&output, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	c := &staticClient{permit: func(context.Context) (protocol.PermitToken, error) { return "", nil }}
	for _, tc := range []struct {
		err    error
		reason string
	}{
		{protocol.ErrPermit, "permit_invalid"},
		{&incorrectResponseCodeErr{code: 1, msg: "SECRET_SERVER_RESPONSE"}, "relay_rejected"},
		{context.DeadlineExceeded, "timeout"},
		{io.EOF, "connection_closed"},
		{errors.New("SECRET_SIGNED_PERMIT"), "transport_or_protocol"},
	} {
		output.Reset()
		c.logPrivateSetupFailure(context.Background(), "join", tc.err)
		var entry map[string]any
		if err := json.Unmarshal(output.Bytes(), &entry); err != nil {
			t.Fatal(err)
		}
		if entry["stage"] != "join" || entry["reason"] != tc.reason || strings.Contains(output.String(), "SECRET") {
			t.Fatalf("missing classification or leaked error content: %s", output.String())
		}
	}
	output.Reset()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c.logPrivateSetupFailure(ctx, "connect", context.Canceled)
	c.permit = nil
	c.logPrivateSetupFailure(context.Background(), "connect", errors.New("legacy failure"))
	if output.Len() != 0 {
		t.Fatalf("normal cancellation or legacy client emitted private failure: %s", output.String())
	}
}
