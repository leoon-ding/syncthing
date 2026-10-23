// Copyright (C) 2026 The Syncthing Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package model

import (
	"bytes"
	"context"
	"errors"
	"io"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/syncthing/syncthing/internal/db"
	"github.com/syncthing/syncthing/lib/events"
	"github.com/syncthing/syncthing/lib/protocol"
	"github.com/syncthing/syncthing/lib/semaphore"
	"github.com/syncthing/syncthing/lib/testutil"
)

func TestFolderAccessSealDrainIsolationAndRecovery(t *testing.T) {
	g := new(FolderAccess)
	peer := protocol.NewDeviceID([]byte("peer"))
	old, release, err := g.Admit(context.Background(), peer, "one")
	if err != nil {
		t.Fatal(err)
	}
	other, releaseOther, err := g.Admit(context.Background(), peer, "two")
	if err != nil {
		t.Fatal(err)
	}
	defer releaseOther()
	if err := g.Open(peer, "one"); err != nil {
		t.Fatal("open scope rejected existing work", err)
	}
	g.Seal(peer, "one")
	g.Seal(peer, "one")
	if old.Err() == nil || other.Err() != nil {
		t.Fatal("scope cancellation leaked")
	}
	if _, _, err := g.Admit(context.Background(), peer, "one"); !errors.Is(err, ErrFolderAccessClosed) {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := g.Drain(canceled, peer, "one"); !errors.Is(err, context.Canceled) {
		t.Fatal("unfinished drain", err)
	}
	if err := g.Open(peer, "one"); err == nil {
		t.Fatal("reopened before release")
	}
	release()
	release()
	if err := g.Drain(context.Background(), peer, "one"); err != nil {
		t.Fatal(err)
	}
	if err := g.Open(peer, "one"); err != nil {
		t.Fatal(err)
	}
	next, done, err := g.Admit(context.Background(), peer, "one")
	if err != nil {
		t.Fatal(err)
	}
	defer done()
	release()
	if next.Err() != nil {
		t.Fatal("stale release canceled new work")
	}
	g.Seal(protocol.EmptyDeviceID, "")
	if next.Err() == nil || other.Err() == nil {
		t.Fatal("stop missed work")
	}
}
func TestFolderAccessRequestRetainedUntilResponseClose(t *testing.T) {
	cfg, folder := newDefaultCfgWrapper(t)
	m := setupModel(t, cfg, new(FolderAccess))
	defer cleanupModel(m)
	file, err := folder.Filesystem().Create("native.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = file.Write([]byte("private")); err != nil {
		t.Fatal(err)
	}
	file.Close()
	response, err := m.Request(device1Conn, &protocol.Request{Folder: "default", Name: "native.txt", Size: 7})
	if err != nil {
		t.Fatal(err)
	}
	defer response.Close()
	m.folderAccess.Seal(device1, "default")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := m.folderAccess.Drain(ctx, device1, "default"); err == nil {
		t.Fatal("response released before send completion")
	}
	if _, err := m.Request(device1Conn, &protocol.Request{Folder: "default", Name: "native.txt", Size: 7}); err == nil {
		t.Fatal("revoked read accepted")
	}
	if response.(interface{ Context() context.Context }).Context().Err() == nil {
		t.Fatal("response not canceled")
	}
	response.Close()
	if err := m.folderAccess.Drain(context.Background(), device1, "default"); err != nil {
		t.Fatal(err)
	}
	if err := m.folderAccess.Open(device1, "default"); err != nil {
		t.Fatal(err)
	}
	again, err := m.Request(device1Conn, &protocol.Request{Folder: "default", Name: "native.txt", Size: 7})
	if err != nil {
		t.Fatal(err)
	}
	again.Close()
}
func TestFolderAccessCanceledQuotaReturnsEarlierReservation(t *testing.T) {
	first, second := semaphore.New(1), semaphore.New(1)
	second.Take(1)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := newLimitedRequestResponseContext(ctx, 1, first, second); done <- err }()
	deadline := time.After(time.Second)
	for first.Available() != 0 {
		select {
		case <-deadline:
			t.Fatal("first reservation missing")
		default:
			runtime.Gosched()
		}
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("quota cancellation stalled")
	}
	if first.Available() != 1 {
		t.Fatal("earlier reservation leaked")
	}
	second.Give(1)
}
func TestFolderAccessPullSourceRetainedUntilApplyCompletion(t *testing.T) {
	gate := new(FolderAccess)
	state := &sharedPullerState{folder: "one"}
	peer := protocol.NewDeviceID([]byte("source"))
	scope, err := state.admitSource(context.Background(), gate, peer)
	if err != nil {
		t.Fatal(err)
	}
	same, err := state.admitSource(context.Background(), gate, peer)
	if err != nil || same != scope {
		t.Fatal("duplicate source lease")
	}
	gate.Seal(peer, "one")
	if state.sourceError() == nil {
		t.Fatal("late application allowed")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if gate.Drain(ctx, peer, "one") == nil {
		t.Fatal("apply completion not retained")
	}
	state.releaseSources()
	state.releaseSources()
	if gate.Drain(context.Background(), peer, "one") != nil {
		t.Fatal("release failed")
	}
}

func TestFolderAccessCanceledIndexHasNoSideEffects(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// Deliberately omit DB/runner/connection: cancellation must be checked
	// before any of those owners are used.
	h := &indexHandler{}
	if err := h.receive(nil, false, "index", 0, 0, ctx); err != nil {
		t.Fatal(err)
	}
}

type cancelIndexLock struct{ cancel context.CancelFunc }

func (l cancelIndexLock) Lock()   { l.cancel() }
func (l cancelIndexLock) Unlock() {}
func TestFolderAccessIndexRechecksAfterLock(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h := &indexHandler{conn: device1Conn, cond: sync.NewCond(cancelIndexLock{cancel})}
	if err := h.receive(nil, false, "index", 0, 0, ctx); err != nil {
		t.Fatal(err)
	}
}

func TestFolderAccessReopenRequestsFreshAdvertisement(t *testing.T) {
	g := new(FolderAccess)
	events := g.Reopened()
	g.Seal(device1, "one")
	if err := g.Open(device1, "one"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-events:
	default:
		t.Fatal("reopen did not request refresh")
	}
	if err := g.Open(device1, "one"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-events:
		t.Fatal("no-op open refreshed")
	default:
	}
	g.Seal(device1, "one")
	g.Seal(device1, "two")
	if err := g.Open(device1, "one"); err != nil {
		t.Fatal(err)
	}
	if err := g.Open(device1, "two"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-events:
	default:
		t.Fatal("missing coalesced refresh")
	}
	select {
	case <-events:
		t.Fatal("unbounded refresh queue")
	default:
	}
}

func TestPausedAnnouncementPreservesRevocationFence(t *testing.T) {
	g := new(FolderAccess)
	ctx := context.Background()
	old, release, err := g.AdmitAnnouncement(ctx, device1, "a", false)
	if err != nil {
		t.Fatal(err)
	}
	g.Seal(protocol.EmptyDeviceID, "a")
	if old.Err() == nil {
		t.Fatal("old snapshot not canceled")
	}
	release()
	if err := g.CommitPause(protocol.EmptyDeviceID, "a"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := g.Admit(ctx, device1, "a"); err == nil {
		t.Fatal("paused data admitted")
	}
	if _, _, err := g.AdmitAnnouncement(ctx, device1, "a", false); err == nil {
		t.Fatal("old unpaused snapshot admitted")
	}
	paused, release, err := g.AdmitAnnouncement(ctx, device1, "a", true)
	if err != nil {
		t.Fatal(err)
	}
	g.Seal(device1, "a")
	if paused.Err() == nil {
		t.Fatal("revocation did not cancel paused snapshot")
	}
	release()
	if _, _, err := g.AdmitAnnouncement(ctx, device1, "a", true); err == nil {
		t.Fatal("revoked pause admitted")
	}
	if err := g.Open(protocol.EmptyDeviceID, "a"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := g.AdmitAnnouncement(ctx, device1, "a", true); err == nil {
		t.Fatal("broader pause open bypassed peer fence")
	}
}

// The DB barrier seals admission exactly after deleting the old full index.
type revokedIndexDB struct {
	db.DB
	afterDrop func()
	updated   bool
}

func (d *revokedIndexDB) DropAllFiles(string, protocol.DeviceID) error               { d.afterDrop(); return nil }
func (d *revokedIndexDB) GetDeviceSequence(string, protocol.DeviceID) (int64, error) { return 0, nil }
func (d *revokedIndexDB) Update(_ string, _ protocol.DeviceID, _ []protocol.FileInfo) error {
	d.updated = true
	return nil
}

type indexPullRunner struct {
	service
	pulled bool
}

func (r *indexPullRunner) SchedulePull() { r.pulled = true }

func TestFolderAccessFullIndexFinishesAfterSeal(t *testing.T) {
	gate := new(FolderAccess)
	scope, release, err := gate.Admit(context.Background(), device1, "one")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	store := &revokedIndexDB{afterDrop: func() {
		gate.Seal(device1, "one")
		canceled, cancel := context.WithCancel(context.Background())
		cancel()
		if err := gate.Drain(canceled, device1, "one"); !errors.Is(err, context.Canceled) {
			t.Fatalf("drained during replacement: %v", err)
		}
	}}
	runner := &indexPullRunner{}
	h := &indexHandler{conn: device1Conn, downloads: newDeviceDownloadState(), folder: "one", sdb: store, runner: runner, cond: sync.NewCond(new(sync.Mutex)), evLogger: events.NoopLogger}
	if err := h.receive([]protocol.FileInfo{{Name: "fresh"}}, false, "index", 0, 0, scope); err != nil {
		t.Fatal(err)
	}
	if !store.updated || !runner.pulled {
		t.Fatal("full replacement abandoned after deleting old index")
	}
	release()
	if err := gate.Drain(context.Background(), device1, "one"); err != nil {
		t.Fatal(err)
	}
}

func TestFolderAccessUnsentAnnouncementDrainIsolation(t *testing.T) {
	g := new(FolderAccess)
	sibling, releaseSibling, err := g.Admit(context.Background(), device1, "B")
	if err != nil {
		t.Fatal(err)
	}
	defer releaseSibling()
	admitted := make(chan struct{})
	announce := func(ctx context.Context, id string, paused bool) (context.Context, func(), error) {
		scope, release, err := g.AdmitAnnouncement(ctx, device1, id, paused)
		if id == "B" {
			close(admitted)
		}
		return scope, release, err
	}
	// No Start: the writer cannot accept the announcement, modeling a busy writer.
	c := protocol.NewConnectionWithAnnouncement(device1, bytes.NewReader(nil), io.Discard, testutil.NoopCloser{}, nil, nil, protocol.CompressionNever, nil, announce, nil)
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.ClusterConfig(&protocol.ClusterConfig{Folders: []protocol.Folder{{ID: "A"}, {ID: "B"}}}, nil)
	}()
	<-admitted
	g.Seal(device1, "A")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := g.Drain(ctx, device1, "A"); err != nil {
		t.Fatal("revoked folder waits for unsent announcement", err)
	}
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("snapshot retained sibling borrow")
	}
	if sibling.Err() != nil {
		t.Fatal("unrelated B operation canceled")
	}
	canceled, stop := context.WithCancel(context.Background())
	stop()
	if err := g.Drain(canceled, device1, "B"); !errors.Is(err, context.Canceled) {
		t.Fatal("B active borrow released", err)
	}
	releaseSibling()
	if err := g.Drain(ctx, device1, "B"); err != nil {
		t.Fatal(err)
	}
	if err := g.Open(device1, "A"); err != nil {
		t.Fatal(err)
	}
}
