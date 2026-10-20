package client

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
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
	for _, mode := range []string{"cancel-drain", "wrong-pin", "renewal-failure", "renewal-rejected"} {
		badPin := mode == "wrong-pin"
		t.Run(mode, func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			ready := make(chan struct{})
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
				if !ok || join.Permit != "test-permit" {
					serverDone <- protocol.ErrPermit
					return
				}
				if err = protocol.WriteMessage(tc, protocol.ResponseSuccess); err != nil {
					serverDone <- err
					return
				}
				close(ready)
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
			cl, err := NewPrivateClient(uri, []tls.Certificate{clientCert}, time.Second, func(context.Context) (protocol.PermitToken, error) {
				calls++
				if mode == "renewal-failure" && calls > 1 {
					return "", errors.New("injected issuer failure")
				}
				return "test-permit", nil
			})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			renew := make(chan time.Time, 1)
			go func() { done <- cl.(*staticClient).serveWithRenewal(ctx, renew) }()
			if !badPin {
				select {
				case <-ready:
				case <-time.After(3 * time.Second):
					t.Fatal("registration not acknowledged")
				}
				if mode == "cancel-drain" {
					cancel()
				} else {
					renew <- time.Now()
				}
			}
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("expected stopped or rejected client")
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
