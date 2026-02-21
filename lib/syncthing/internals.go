// Copyright (C) 2024 The Syncthing Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package syncthing

import (
	"context"
	"iter"
	"strings"
	"time"

	"github.com/syncthing/syncthing/internal/db"
	"github.com/syncthing/syncthing/lib/model"
	"github.com/syncthing/syncthing/lib/protocol"
	"github.com/syncthing/syncthing/lib/stats"
)

// Internals allows access to a subset of functionality in model.Model. While intended for use from applications that import
// the package, it is not intended as a stable API at this time. It does however provide a boundary between the more
// volatile Model interface and upstream users (one of which is an iOS app).
type Internals struct {
	model model.Model
}

type Counts = db.Counts

// SnapshotCompat provides a compatibility layer for callers previously using
// the old DB snapshot API from v1.x.
type SnapshotCompat struct {
	model  model.Model
	folder string
}

func newInternals(model model.Model) *Internals {
	return &Internals{
		model: model,
	}
}

func (m *Internals) FolderState(folderID string) (string, time.Time, error) {
	return m.model.State(folderID)
}

func (m *Internals) LoadIgnores(folderID string) ([]string, []string, error) {
	return m.model.LoadIgnores(folderID)
}

func (m *Internals) Ignores(folderID string) ([]string, []string, error) {
	return m.model.CurrentIgnores(folderID)
}

func (m *Internals) SetIgnores(folderID string, content []string) error {
	return m.model.SetIgnores(folderID, content)
}

func (m *Internals) DownloadBlock(ctx context.Context, deviceID protocol.DeviceID, folderID string, path string, blockNumber int, blockInfo protocol.BlockInfo, allowFromTemporary bool) ([]byte, error) {
	return m.model.RequestGlobal(ctx, deviceID, folderID, path, blockNumber, blockInfo.Offset, blockInfo.Size, blockInfo.Hash, allowFromTemporary)
}

func (m *Internals) BlockAvailability(folderID string, file protocol.FileInfo, block protocol.BlockInfo) ([]model.Availability, error) {
	return m.model.Availability(folderID, file, block)
}

func (m *Internals) GlobalFileInfo(folderID, path string) (protocol.FileInfo, bool, error) {
	return m.model.CurrentGlobalFile(folderID, path)
}

func (m *Internals) LocalFileInfo(folderID, path string) (protocol.FileInfo, bool, error) {
	return m.model.CurrentFolderFile(folderID, path)
}

func (m *Internals) GlobalTree(folderID string, prefix string, levels int, returnOnlyDirectories bool) ([]*model.TreeEntry, error) {
	return m.model.GlobalDirectoryTree(folderID, prefix, levels, returnOnlyDirectories)
}

func (m *Internals) IsConnectedTo(deviceID protocol.DeviceID) bool {
	return m.model.ConnectedTo(deviceID)
}

func (m *Internals) ScanFolders() map[string]error {
	return m.model.ScanFolders()
}

func (m *Internals) Completion(deviceID protocol.DeviceID, folderID string) (model.FolderCompletion, error) {
	return m.model.Completion(deviceID, folderID)
}

func (m *Internals) DeviceStatistics() (map[protocol.DeviceID]stats.DeviceStatistics, error) {
	return m.model.DeviceStatistics()
}

func (m *Internals) PendingFolders(deviceID protocol.DeviceID) (map[string]db.PendingFolder, error) {
	return m.model.PendingFolders(deviceID)
}

func (m *Internals) ScanFolderSubdirs(folderID string, paths []string) error {
	return m.model.ScanFolderSubdirs(folderID, paths)
}

func (m *Internals) DBSnapshot(folderID string) (*SnapshotCompat, error) {
	if _, err := m.model.GlobalSize(folderID); err != nil {
		return nil, err
	}
	return &SnapshotCompat{
		model:  m.model,
		folder: folderID,
	}, nil
}

func (m *Internals) GlobalSize(folder string) (Counts, error) {
	counts, err := m.model.GlobalSize(folder)
	if err != nil {
		return Counts{}, err
	}
	return counts, nil
}

func (m *Internals) LocalSize(folder string) (Counts, error) {
	counts, err := m.model.LocalSize(folder, protocol.LocalDeviceID)
	if err != nil {
		return Counts{}, err
	}
	return counts, nil
}

func (m *Internals) NeedSize(folder string, device protocol.DeviceID) (Counts, error) {
	counts, err := m.model.NeedSize(folder, device)
	if err != nil {
		return Counts{}, err
	}
	return counts, nil
}

func (m *Internals) AllGlobalFiles(folder string) (iter.Seq[db.FileMetadata], func() error) {
	return m.model.AllGlobalFiles(folder)
}

func (m *Internals) FolderProgressBytesCompleted(folder string) int64 {
	return m.model.FolderProgressBytesCompleted(folder)
}

// NeedFolderFiles returns paginated list of currently needed files in
// progress, queued, and to be queued on next puller iteration.
func (m *Internals) NeedFolderFiles(folder string, page, perpage int) ([]protocol.FileInfo, []protocol.FileInfo, []protocol.FileInfo, error) {
	return m.model.NeedFolderFiles(folder, page, perpage)
}

func (m *Internals) RemoteNeedFolderFiles(folder string, device protocol.DeviceID, page, perpage int) ([]protocol.FileInfo, error) {
	return m.model.RemoteNeedFolderFiles(folder, device, page, perpage)
}

func (m *Internals) LocalChangedFolderFiles(folder string, page, perpage int) ([]protocol.FileInfo, error) {
	return m.model.LocalChangedFolderFiles(folder, page, perpage)
}

func (m *Internals) ScanFolder(folderID string) error {
	return m.model.ScanFolder(folderID)
}

func (m *Internals) Override(folderID string) {
	m.model.Override(folderID)
}

func (m *Internals) Revert(folderID string) {
	m.model.Revert(folderID)
}

func (m *Internals) ResetFolder(folderID string) error {
	return m.model.ResetFolder(folderID)
}

func (m *Internals) FolderErrors(folderID string) ([]model.FileError, error) {
	return m.model.FolderErrors(folderID)
}

func (s *SnapshotCompat) Release() {}

func (s *SnapshotCompat) WithGlobalTruncated(fn func(protocol.FileInfo) bool) {
	seq, done := s.model.AllGlobalFiles(s.folder)
	defer func() {
		if done != nil {
			_ = done()
		}
	}()
	for md := range seq {
		fi, ok, err := s.model.CurrentGlobalFile(s.folder, md.Name)
		if err != nil || !ok {
			continue
		}
		if !fn(fi) {
			return
		}
	}
}

func (s *SnapshotCompat) WithPrefixedGlobalTruncated(prefix string, fn func(protocol.FileInfo) bool) {
	seq, done := s.model.AllGlobalFiles(s.folder)
	defer func() {
		if done != nil {
			_ = done()
		}
	}()
	for md := range seq {
		if prefix != "" && !strings.HasPrefix(md.Name, prefix) {
			continue
		}
		fi, ok, err := s.model.CurrentGlobalFile(s.folder, md.Name)
		if err != nil || !ok {
			continue
		}
		if !fn(fi) {
			return
		}
	}
}
