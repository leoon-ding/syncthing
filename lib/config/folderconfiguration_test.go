// Copyright (C) 2026 The Syncthing Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package config

import (
	"errors"
	"os"
	"testing"

	"github.com/syncthing/syncthing/lib/fs"
)

func TestCreateMarkerClosesDirectory(t *testing.T) {
	for _, tc := range []struct {
		name     string
		openErr  error
		syncErr  error
		closeErr error
	}{
		{name: "sync succeeds"},
		{name: "sync fails", syncErr: errors.New("sync failed")},
		{name: "close fails", closeErr: errors.New("close failed")},
		{name: "open fails", openErr: errors.New("open failed")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			tracked := &markerDirectoryFilesystem{
				Filesystem: fs.NewFilesystem(fs.FilesystemTypeBasic, root),
				openErr:    tc.openErr, syncErr: tc.syncErr, closeErr: tc.closeErr,
			}
			// Keep the opened file reachable until cleanup so GC cannot hide a
			// missing Close. Also release it if the regression assertion fails.
			t.Cleanup(func() {
				if tracked.directory != nil && tracked.directory.closes == 0 {
					_ = tracked.directory.File.Close()
				}
			})
			kind := fs.FilesystemType(t.Name())
			fs.RegisterFilesystemType(kind, func(string, ...fs.Option) (fs.Filesystem, error) {
				return tracked, nil
			})
			cfg := FolderConfiguration{
				ID: "marker-close", Path: root, FilesystemType: FilesystemType(kind),
				MarkerName: DefaultMarkerName, CaseSensitiveFS: true,
			}
			// Directory sync/close remain best effort, as marker creation was
			// successful. The original fsync error must not skip Close.
			if err := cfg.CreateMarker(); err != nil {
				t.Fatal(err)
			}
			if err := cfg.CheckPath(); err != nil {
				t.Fatal(err)
			}
			if tracked.opens != 1 {
				t.Fatalf("directory opens = %d, want 1", tracked.opens)
			}
			if tc.openErr == nil {
				if tracked.directory.syncs != 1 || tracked.directory.closes != 1 {
					t.Errorf("directory syncs/closes = %d/%d, want 1/1", tracked.directory.syncs, tracked.directory.closes)
				}
			} else if tracked.directory != nil {
				t.Error("failed open returned a directory")
			}
			// In particular, Windows must allow removal before GC or cleanup.
			if err := os.RemoveAll(root); err != nil {
				t.Errorf("remove folder immediately after CreateMarker: %v", err)
			}
		})
	}
}

type markerDirectoryFilesystem struct {
	fs.Filesystem
	openErr, syncErr, closeErr error
	opens                      int
	directory                  *markerDirectoryFile
}

func (f *markerDirectoryFilesystem) Open(name string) (fs.File, error) {
	f.opens++
	if f.openErr != nil {
		return nil, f.openErr
	}
	file, err := f.Filesystem.Open(name)
	if err != nil {
		return nil, err
	}
	f.directory = &markerDirectoryFile{File: file, syncErr: f.syncErr, closeErr: f.closeErr}
	return f.directory, nil
}

type markerDirectoryFile struct {
	fs.File
	syncErr, closeErr error
	syncs, closes     int
}

func (f *markerDirectoryFile) Sync() error {
	f.syncs++
	return f.syncErr
}

func (f *markerDirectoryFile) Close() error {
	f.closes++
	return errors.Join(f.File.Close(), f.closeErr)
}
