// Copyright (C) 2026 The Syncthing Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package main

import (
	"net"
	"testing"
	"time"

	native "github.com/syncthing/syncthing/lib/protocol"
)

func TestSessionAcceptsJoinsBeforeServe(t *testing.T) {
	s := newSession(native.NewDeviceID([]byte("a")), native.NewDeviceID([]byte("b")), 0, nil)
	if s == nil {
		t.Fatal("session creation failed")
	}
	s.onDone = func() {} // Private sessions close both directions on termination.
	a, ap := net.Pipe()
	b, bp := net.Pipe()
	extra, ep := net.Pipe()
	defer ap.Close()
	defer bp.Close()
	defer extra.Close()
	defer ep.Close()
	// No receiver is running: this deterministically exercises the old default
	// branch, without relying on a scheduler race or sleeps.
	defer func() { s.abort(); go s.Serve(); <-s.done }()
	if !s.AddConnection(a) {
		a.Close()
		t.Fatal("first join rejected before Serve")
	}
	if !s.AddConnection(b) {
		b.Close()
		t.Fatal("second join rejected before Serve")
	}
	if s.AddConnection(extra) {
		t.Fatal("third join accepted")
	}
}

func TestSessionQueuedJoinsClosedOnStopOrExpiry(t *testing.T) {
	for _, mode := range []string{"abort", "close", "expiry"} {
		t.Run(mode, func(t *testing.T) {
			s := newSession(native.NewDeviceID([]byte("a")), native.NewDeviceID([]byte("b")), 0, nil)
			if s == nil {
				t.Fatal("session creation failed")
			}
			a, peer := net.Pipe()
			defer peer.Close()
			if !s.AddConnection(a) {
				a.Close()
				t.Fatal("join rejected")
			}
			switch mode {
			case "abort":
				s.abort()
			case "close":
				s.CloseConns()
			case "expiry":
				s.joinDeadline = time.Now().Add(-time.Second)
			}
			extra, other := net.Pipe()
			defer extra.Close()
			defer other.Close()
			if s.AddConnection(extra) {
				t.Fatal("join accepted after sealing or expiry")
			}
			go s.Serve()
			defer func() { s.abort(); <-s.done }()
			// CloseConns seals admission but Serve still needs its normal stop signal.
			if mode == "close" {
				s.abort()
			}
			select {
			case <-s.done:
			case <-time.After(time.Second):
				t.Fatal("session did not drain")
			}
			peer.SetReadDeadline(time.Now().Add(time.Second))
			if _, err := peer.Read(make([]byte, 1)); err == nil {
				t.Fatal("accepted socket not closed")
			} else if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
				t.Fatal("accepted socket leaked", err)
			}
			if findSession(string(s.clientkey)) != nil || findSession(string(s.serverkey)) != nil {
				t.Fatal("pending keys survived drain")
			}
		})
	}
}
