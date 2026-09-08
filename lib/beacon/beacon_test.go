// Copyright (C) 2026 The Syncthing Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package beacon

import (
	"bytes"
	"testing"
)

func TestCastSendCopiesData(t *testing.T) {
	c := &cast{
		inbox:   make(chan []byte),
		stopped: make(chan struct{}),
	}
	inspect := make(chan struct{})
	received := make(chan []byte, 1)
	go func() {
		data := <-c.inbox
		<-inspect
		received <- data
	}()

	sent := []byte("announcement")
	c.Send(sent)
	sent[0] = 'X'
	close(inspect)

	if got := <-received; !bytes.Equal(got, []byte("announcement")) {
		t.Fatalf("Send retained caller-owned storage: got %q", got)
	}
}
