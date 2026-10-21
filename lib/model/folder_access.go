// Copyright (C) 2026 The Syncthing Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package model

import (
	"context"
	"errors"
	"sync"

	"github.com/syncthing/syncthing/lib/protocol"
)

// FolderAccess is a run-local deny fence, not an authorization database.
// The embedding configuration owner seals before changing sharing and opens
// only after a successful authorized configuration commit. Nil preserves the
// standalone engine contract. Existing model sharing checks remain mandatory.
type FolderAccess struct {
	mu       sync.Mutex
	sealed   map[folderScope]bool
	paused   map[folderScope]bool
	work     map[*folderWork]struct{}
	changed  chan struct{}
	reopened chan struct{}
}
type folderScope struct {
	device protocol.DeviceID
	folder string
}
type folderWork struct {
	scope  folderScope
	cancel context.CancelFunc
}

var ErrFolderAccessClosed = errors.New("folder access sealed")

func (g *FolderAccess) initLocked() {
	if g.sealed == nil {
		g.sealed = make(map[folderScope]bool)
		g.paused = make(map[folderScope]bool)
		g.work = make(map[*folderWork]struct{})
		g.changed = make(chan struct{})
		g.reopened = make(chan struct{}, 1)
	}
}
func (s folderScope) matches(v folderScope) bool {
	return (s.device == protocol.EmptyDeviceID || s.device == v.device) && (s.folder == "" || s.folder == v.folder)
}
func (g *FolderAccess) Admit(parent context.Context, device protocol.DeviceID, folder string) (context.Context, func(), error) {
	return g.admit(parent, device, folder, false, false)
}

// AdmitAnnouncement allows committed pause metadata, but never a revocation fence.
func (g *FolderAccess) AdmitAnnouncement(parent context.Context, device protocol.DeviceID, folder string, paused bool) (context.Context, func(), error) {
	return g.admit(parent, device, folder, true, paused)
}

func (g *FolderAccess) admit(parent context.Context, device protocol.DeviceID, folder string, announcement, paused bool) (context.Context, func(), error) {
	if parent.Err() != nil {
		return nil, nil, parent.Err()
	}
	if g == nil {
		return parent, func() {}, nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.initLocked()
	scope := folderScope{device, folder}
	for sealed := range g.sealed {
		if sealed.matches(scope) {
			return nil, nil, ErrFolderAccessClosed
		}
	}
	for p := range g.paused {
		if p.matches(scope) && (!announcement || !paused) {
			return nil, nil, ErrFolderAccessClosed
		}
	}
	ctx, cancel := context.WithCancel(parent)
	w := &folderWork{scope, cancel}
	g.work[w] = struct{}{}
	var once sync.Once
	return ctx, func() {
		once.Do(func() {
			cancel()
			g.mu.Lock()
			delete(g.work, w)
			close(g.changed)
			g.changed = make(chan struct{})
			g.mu.Unlock()
		})
	}, nil
}

// Seal is idempotent and remains effective after Drain times out. Cancellation
// requests do not release work; only each work owner's completion does.
func (g *FolderAccess) Seal(device protocol.DeviceID, folder string) {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.initLocked()
	scope := folderScope{device, folder}
	g.sealed[scope] = true
	for w := range g.work {
		if scope.matches(w.scope) {
			w.cancel()
		}
	}
}
func (g *FolderAccess) Drain(ctx context.Context, device protocol.DeviceID, folder string) error {
	if g == nil {
		return nil
	}
	scope := folderScope{device, folder}
	for {
		g.mu.Lock()
		g.initLocked()
		busy := false
		for w := range g.work {
			if scope.matches(w.scope) {
				busy = true
				break
			}
		}
		changed := g.changed
		g.mu.Unlock()
		if !busy {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
		}
	}
}

// Open cannot supersede unfinished old work. Broader device/run fences remain
// sealed; opening one folder never reopens siblings or the entire device.
func (g *FolderAccess) Open(device protocol.DeviceID, folder string) error {
	if g == nil {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.initLocked()
	scope := folderScope{device, folder}
	if !g.sealed[scope] && !g.paused[scope] {
		return nil
	}
	for w := range g.work {
		if scope.matches(w.scope) {
			return ErrFolderAccessClosed
		}
	}
	delete(g.sealed, scope)
	delete(g.paused, scope)
	select {
	case g.reopened <- struct{}{}:
	default:
	}
	return nil
}

type admittedResponse struct {
	protocol.RequestResponse
	ctx     context.Context
	release func()
	once    sync.Once
}

func (r *admittedResponse) Context() context.Context { return r.ctx }
func (r *admittedResponse) Close()                   { r.once.Do(func() { r.RequestResponse.Close(); r.release() }) }

// Reopened coalesces successful fence reopening so the model can regenerate a
// fresh complete advertisement. It never replays a pre-revocation snapshot.
func (g *FolderAccess) Reopened() <-chan struct{} {
	if g == nil {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.initLocked()
	return g.reopened
}

// CommitPause replaces a drained mutation fence only after paused configuration
// is committed. Data remains denied; fresh complete announcements may proceed.
func (g *FolderAccess) CommitPause(device protocol.DeviceID, folder string) error {
	if g == nil {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.initLocked()
	scope := folderScope{device, folder}
	for w := range g.work {
		if scope.matches(w.scope) {
			return ErrFolderAccessClosed
		}
	}
	g.paused[scope] = true
	delete(g.sealed, scope)
	select {
	case g.reopened <- struct{}{}:
	default:
	}
	return nil
}
