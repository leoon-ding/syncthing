// Copyright (C) 2014 The Syncthing Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package protocol

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	lz4 "github.com/pierrec/lz4/v4"
	"google.golang.org/protobuf/proto"

	"github.com/syncthing/syncthing/internal/gen/bep"
	"github.com/syncthing/syncthing/lib/rand"
	"github.com/syncthing/syncthing/lib/testutil"
)

var (
	c0ID = NewDeviceID([]byte{1})
	c1ID = NewDeviceID([]byte{2})
)

func TestPing(t *testing.T) {
	ar, aw := io.Pipe()
	br, bw := io.Pipe()

	c0 := getRawConnection(NewConnection(c0ID, ar, bw, testutil.NoopCloser{}, newTestModel(), new(mockedConnectionInfo), CompressionAlways, testKeyGen))
	c0.Start()
	defer closeAndWait(c0, ar, bw)
	c1 := getRawConnection(NewConnection(c1ID, br, aw, testutil.NoopCloser{}, newTestModel(), new(mockedConnectionInfo), CompressionAlways, testKeyGen))
	c1.Start()
	defer closeAndWait(c1, ar, bw)
	c0.ClusterConfig(&ClusterConfig{}, nil)
	c1.ClusterConfig(&ClusterConfig{}, nil)

	if ok := c0.ping(); !ok {
		t.Error("c0 ping failed")
	}
	if ok := c1.ping(); !ok {
		t.Error("c1 ping failed")
	}
}

var errManual = errors.New("manual close")

func TestClose(t *testing.T) {
	m0 := newTestModel()
	m1 := newTestModel()

	ar, aw := io.Pipe()
	br, bw := io.Pipe()

	c0 := getRawConnection(NewConnection(c0ID, ar, bw, testutil.NoopCloser{}, m0, new(mockedConnectionInfo), CompressionAlways, testKeyGen))
	c0.Start()
	defer closeAndWait(c0, ar, bw)
	c1 := NewConnection(c1ID, br, aw, testutil.NoopCloser{}, m1, new(mockedConnectionInfo), CompressionAlways, testKeyGen)
	c1.Start()
	defer closeAndWait(c1, ar, bw)
	c0.ClusterConfig(&ClusterConfig{}, nil)
	c1.ClusterConfig(&ClusterConfig{}, nil)

	c0.internalClose(errManual)

	<-c0.closed
	if err := m0.closedError(); err != errManual {
		t.Fatal("Connection should be closed")
	}

	// None of these should panic, some should return an error

	if c0.ping() {
		t.Error("Ping should not return true")
	}

	ctx := context.Background()

	c0.Index(ctx, &Index{Folder: "default"})
	c0.Index(ctx, &Index{Folder: "default"})

	if _, err := c0.Request(ctx, &Request{Folder: "default", Name: "foo"}); err == nil {
		t.Error("Request should return an error")
	}
}

// TestCloseOnBlockingSend checks that the connection does not deadlock when
// Close is called while the underlying connection is broken (send blocks).
// https://github.com/syncthing/syncthing/pull/5442
func TestCloseOnBlockingSend(t *testing.T) {
	oldCloseTimeout := CloseTimeout
	CloseTimeout = 100 * time.Millisecond
	defer func() {
		CloseTimeout = oldCloseTimeout
	}()

	m := newTestModel()

	rw := testutil.NewBlockingRW()
	c := getRawConnection(NewConnection(c0ID, rw, rw, testutil.NoopCloser{}, m, new(mockedConnectionInfo), CompressionAlways, testKeyGen))
	c.Start()
	defer closeAndWait(c, rw)

	wg := sync.WaitGroup{}

	wg.Add(1)
	go func() {
		c.ClusterConfig(&ClusterConfig{}, nil)
		wg.Done()
	}()

	wg.Add(1)
	go func() {
		c.Close(errManual)
		wg.Done()
	}()

	// This simulates an error from ping timeout
	wg.Add(1)
	go func() {
		c.internalClose(ErrTimeout)
		wg.Done()
	}()

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("timed out before all functions returned")
	}
}

func TestCloseRace(t *testing.T) {
	indexReceived := make(chan struct{})
	unblockIndex := make(chan struct{})
	m0 := newTestModel()
	m0.indexFn = func(string, []FileInfo) {
		close(indexReceived)
		<-unblockIndex
	}
	m1 := newTestModel()

	ar, aw := io.Pipe()
	br, bw := io.Pipe()

	c0 := getRawConnection(NewConnection(c0ID, ar, bw, testutil.NoopCloser{}, m0, new(mockedConnectionInfo), CompressionNever, testKeyGen))
	c0.Start()
	defer closeAndWait(c0, ar, bw)
	c1 := NewConnection(c1ID, br, aw, testutil.NoopCloser{}, m1, new(mockedConnectionInfo), CompressionNever, testKeyGen)
	c1.Start()
	defer closeAndWait(c1, ar, bw)
	c0.ClusterConfig(&ClusterConfig{}, nil)
	c1.ClusterConfig(&ClusterConfig{}, nil)

	c1.Index(context.Background(), &Index{Folder: "default"})
	select {
	case <-indexReceived:
	case <-time.After(time.Second):
		t.Fatal("timed out before receiving index")
	}

	go c0.internalClose(errManual)
	select {
	case <-c0.closed:
	case <-time.After(time.Second):
		t.Fatal("timed out before c0.closed was closed")
	}

	select {
	case <-m0.closedCh:
		t.Errorf("receiver.Closed called before receiver.Index")
	default:
	}

	close(unblockIndex)

	if err := m0.closedError(); err != errManual {
		t.Fatal("Connection should be closed")
	}
}

func TestClusterConfigFirst(t *testing.T) {
	m := newTestModel()

	rw := testutil.NewBlockingRW()
	c := getRawConnection(NewConnection(c0ID, rw, &testutil.NoopRW{}, testutil.NoopCloser{}, m, new(mockedConnectionInfo), CompressionAlways, testKeyGen))
	c.Start()
	defer closeAndWait(c, rw)

	select {
	case c.outbox <- asyncMessage{msg: &bep.Ping{}}:
		t.Fatal("able to send ping before cluster config")
	case <-time.After(100 * time.Millisecond):
		// Allow some time for c.writerLoop to set up after c.Start
	}

	c.ClusterConfig(&ClusterConfig{}, nil)

	done := make(chan struct{})
	if ok := c.send(context.Background(), &bep.Ping{}, done); !ok {
		t.Fatal("send ping after cluster config returned false")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("timed out before ping was sent")
	}

	done = make(chan struct{})
	go func() {
		c.internalClose(errManual)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close didn't return before timeout")
	}

	if err := m.closedError(); err != errManual {
		t.Fatal("Connection should be closed")
	}
}

// TestCloseTimeout checks that calling Close times out and proceeds, if sending
// the close message does not succeed.
func TestCloseTimeout(t *testing.T) {
	oldCloseTimeout := CloseTimeout
	CloseTimeout = 100 * time.Millisecond
	defer func() {
		CloseTimeout = oldCloseTimeout
	}()

	m := newTestModel()

	rw := testutil.NewBlockingRW()
	c := getRawConnection(NewConnection(c0ID, rw, rw, testutil.NoopCloser{}, m, new(mockedConnectionInfo), CompressionAlways, testKeyGen))
	c.Start()
	defer closeAndWait(c, rw)

	done := make(chan struct{})
	go func() {
		c.Close(errManual)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * CloseTimeout):
		t.Fatal("timed out before Close returned")
	}
}

func TestUnmarshalFDPUv16v17(t *testing.T) {
	var fdpu bep.FileDownloadProgressUpdate

	m0, _ := hex.DecodeString("08cda1e2e3011278f3918787f3b89b8af2958887f0aa9389f3a08588f3aa8f96f39aa8a5f48b9188f19286a0f3848da4f3aba799f3beb489f0a285b9f487b684f2a3bda2f48598b4f2938a89f2a28badf187a0a2f2aebdbdf4849494f4808fbbf2b3a2adf2bb95bff0a6ada4f198ab9af29a9c8bf1abb793f3baabb2f188a6ba1a0020bb9390f60220f6d9e42220b0c7e2b2fdffffffff0120fdb2dfcdfbffffffff0120cedab1d50120bd8784c0feffffffff0120ace99591fdffffffff0120eed7d09af9ffffffff01")
	if err := proto.Unmarshal(m0, &fdpu); err != nil {
		t.Fatal("Unmarshalling message from v0.14.16:", err)
	}

	m1, _ := hex.DecodeString("0880f1969905128401f099b192f0abb1b9f3b280aff19e9aa2f3b89e84f484b39df1a7a6b0f1aea4b1f0adac94f3b39caaf1939281f1928a8af0abb1b0f0a8b3b3f3a88e94f2bd85acf29c97a9f2969da6f0b7a188f1908ea2f09a9c9bf19d86a6f29aada8f389bb95f0bf9d88f1a09d89f1b1a4b5f29b9eabf298a59df1b2a589f2979ebdf0b69880f18986b21a440a1508c7d8fb8897ca93d90910e8c4d8e8f2f8f0ccee010a1508afa8ffd8c085b393c50110e5bdedc3bddefe9b0b0a1408a1bedddba4cac5da3c10b8e5d9958ca7e3ec19225ae2f88cb2f8ffffffff018ceda99cfbffffffff01b9c298a407e295e8e9fcffffffff01f3b9ade5fcffffffff01c08bfea9fdffffffff01a2c2e5e1ffffffffff0186dcc5dafdffffffff01e9ffc7e507c9d89db8fdffffffff01")
	if err := proto.Unmarshal(m1, &fdpu); err != nil {
		t.Fatal("Unmarshalling message from v0.14.16:", err)
	}
}

func TestWriteCompressed(t *testing.T) {
	for _, random := range []bool{false, true} {
		buf := new(bytes.Buffer)
		c := &rawConnection{
			cr:          &countingReader{Reader: buf},
			cw:          &countingWriter{Writer: buf},
			compression: CompressionAlways,
		}

		msg := (&Response{Data: make([]byte, 10240)}).toWire()
		if random {
			// This should make the message incompressible.
			rand.Read(msg.Data)
		}

		if err := c.writeMessage(msg); err != nil {
			t.Fatal(err)
		}
		got, err := c.readMessage(make([]byte, 4))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got.(*bep.Response).Data, msg.Data) {
			t.Error("received the wrong message")
		}

		hdr := &bep.Header{Type: typeOf(msg)}
		size := int64(2 + proto.Size(hdr) + 4 + proto.Size(msg))
		if c.cr.Tot() > size {
			t.Errorf("compression enlarged message from %d to %d",
				size, c.cr.Tot())
		}
	}
}

func TestLZ4Compression(t *testing.T) {
	for i := 0; i < 10; i++ {
		dataLen := 150 + rand.Intn(150)
		data := make([]byte, dataLen)
		_, err := io.ReadFull(rand.Reader, data[100:])
		if err != nil {
			t.Fatal(err)
		}

		comp := make([]byte, lz4.CompressBlockBound(dataLen))
		compLen, err := lz4Compress(data, comp)
		if err != nil {
			t.Errorf("compressing %d bytes: %v", dataLen, err)
			continue
		}

		res, err := lz4Decompress(comp[:compLen])
		if err != nil {
			t.Errorf("decompressing %d bytes to %d: %v", len(comp), dataLen, err)
			continue
		}
		if len(res) != len(data) {
			t.Errorf("Incorrect len %d != expected %d", len(res), len(data))
		}
		if !bytes.Equal(data, res) {
			t.Error("Incorrect decompressed data")
		}
		t.Logf("OK #%d, %d -> %d -> %d", i, dataLen, len(comp), dataLen)
	}
}

func TestLZ4CompressionUpdate(t *testing.T) {
	uncompressed := []byte("this is some arbitrary yet fairly compressible data")

	// Compressed, as created by the LZ4 implementation in Syncthing 1.18.6 and earlier.
	oldCompressed, _ := hex.DecodeString("00000033f0247468697320697320736f6d65206172626974726172792079657420666169726c7920636f6d707265737369626c652064617461")

	// Verify that we can decompress

	res, err := lz4Decompress(oldCompressed)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(uncompressed, res) {
		t.Fatal("result does not match")
	}

	// Verify that our current compression is equivalent

	buf := make([]byte, 128)
	n, err := lz4Compress(uncompressed, buf)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(oldCompressed, buf[:n]) {
		t.Logf("%x", oldCompressed)
		t.Logf("%x", buf[:n])
		t.Fatal("compression does not match")
	}
}

func TestCheckFilename(t *testing.T) {
	cases := []struct {
		name string
		ok   bool
	}{
		// Valid filenames
		{"foo", true},
		{"foo/bar/baz", true},
		{"foo/bar:baz", true}, // colon is ok in general, will be filtered on windows
		{`\`, true},           // path separator on the wire is forward slash, so as above
		{`\.`, true},
		{`\..`, true},
		{".foo", true},
		{"foo..", true},

		// Invalid filenames
		{"foo/..", false},
		{"foo/../bar", false},
		{"../foo/../bar", false},
		{"", false},
		{".", false},
		{"..", false},
		{"/", false},
		{"/.", false},
		{"/..", false},
		{"/foo", false},
		{"./foo", false},
		{"foo./", false},
		{"foo/.", false},
		{"foo/", false},
	}

	for _, tc := range cases {
		err := checkFilename(tc.name)
		if (err == nil) != tc.ok {
			t.Errorf("Unexpected result for checkFilename(%q): %v", tc.name, err)
		}
	}
}

func TestCheckConsistency(t *testing.T) {
	cases := []struct {
		fi FileInfo
		ok bool
	}{
		{
			// valid
			fi: FileInfo{
				Name:   "foo",
				Type:   FileInfoTypeFile,
				Blocks: []BlockInfo{{Size: 1234, Offset: 0, Hash: []byte{1, 2, 3, 4}}},
			},
			ok: true,
		},
		{
			// deleted with blocks
			fi: FileInfo{
				Name:    "foo",
				Deleted: true,
				Type:    FileInfoTypeFile,
				Blocks:  []BlockInfo{{Size: 1234, Offset: 0, Hash: []byte{1, 2, 3, 4}}},
			},
			ok: false,
		},
		{
			// no blocks
			fi: FileInfo{
				Name: "foo",
				Type: FileInfoTypeFile,
			},
			ok: false,
		},
		{
			// directory with blocks
			fi: FileInfo{
				Name:   "foo",
				Type:   FileInfoTypeDirectory,
				Blocks: []BlockInfo{{Size: 1234, Offset: 0, Hash: []byte{1, 2, 3, 4}}},
			},
			ok: false,
		},
	}

	for _, tc := range cases {
		err := checkFileInfoConsistency(tc.fi)
		if tc.ok && err != nil {
			t.Errorf("Unexpected error %v (want nil) for %v", err, tc.fi)
		}
		if !tc.ok && err == nil {
			t.Errorf("Unexpected nil error for %v", tc.fi)
		}
	}
}

func TestBlockSize(t *testing.T) {
	cases := []struct {
		fileSize  int64
		blockSize int
	}{
		{1 << KiB, 128 << KiB},
		{1 << MiB, 128 << KiB},
		{499 << MiB, 256 << KiB},
		{500 << MiB, 512 << KiB},
		{501 << MiB, 512 << KiB},
		{1 << GiB, 1 << MiB},
		{2 << GiB, 2 << MiB},
		{3 << GiB, 2 << MiB},
		{500 << GiB, 16 << MiB},
		{50000 << GiB, 16 << MiB},
	}

	for _, tc := range cases {
		size := BlockSize(tc.fileSize)
		if size != tc.blockSize {
			t.Errorf("BlockSize(%d), size=%d, expected %d", tc.fileSize, size, tc.blockSize)
		}
	}
}

var blockSize int

func BenchmarkBlockSize(b *testing.B) {
	for i := 0; i < b.N; i++ {
		blockSize = BlockSize(16 << 30)
	}
}

// TestClusterConfigAfterClose checks that ClusterConfig does not deadlock when
// ClusterConfig is called on a closed connection.
func TestClusterConfigAfterClose(t *testing.T) {
	m := newTestModel()

	rw := testutil.NewBlockingRW()
	c := getRawConnection(NewConnection(c0ID, rw, rw, testutil.NoopCloser{}, m, new(mockedConnectionInfo), CompressionAlways, testKeyGen))
	c.Start()
	defer closeAndWait(c, rw)

	c.internalClose(errManual)

	done := make(chan struct{})
	go func() {
		c.ClusterConfig(&ClusterConfig{}, nil)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("timed out before Cluster Config returned")
	}
}

func TestDispatcherToCloseDeadlock(t *testing.T) {
	// Verify that we don't deadlock when calling Close() from within one of
	// the model callbacks (ClusterConfig).
	m := newTestModel()
	rw := testutil.NewBlockingRW()
	c := getRawConnection(NewConnection(c0ID, rw, &testutil.NoopRW{}, testutil.NoopCloser{}, m, new(mockedConnectionInfo), CompressionAlways, testKeyGen))
	m.ccFn = func(*ClusterConfig) {
		c.Close(errManual)
	}
	c.Start()
	defer closeAndWait(c, rw)

	c.inbox <- &bep.ClusterConfig{}

	select {
	case <-c.dispatcherLoopStopped:
	case <-time.After(time.Second):
		t.Fatal("timed out before dispatcher loop terminated")
	}
}

func TestIndexIDString(t *testing.T) {
	// Index ID is a 64 bit, zero padded hex integer.
	var i IndexID = 42
	if i.String() != "0x000000000000002A" {
		t.Error(i.String())
	}
}

func closeAndWait(c interface{}, closers ...io.Closer) {
	for _, closer := range closers {
		closer.Close()
	}
	var raw *rawConnection
	switch i := c.(type) {
	case *rawConnection:
		raw = i
	default:
		raw = getRawConnection(c.(Connection))
	}
	raw.internalClose(ErrClosed)
	raw.loopWG.Wait()
}

func getRawConnection(c Connection) *rawConnection {
	var raw *rawConnection
	switch i := c.(type) {
	case wireFormatConnection:
		raw = i.Connection.(encryptedConnection).conn
	case encryptedConnection:
		raw = i.conn
	}
	return raw
}

func TestCloseBeforeStart(t *testing.T) {
	m := newTestModel()
	rw := testutil.NewBlockingRW()
	c := getRawConnection(NewConnection(c0ID, rw, rw, testutil.NoopCloser{}, m, new(mockedConnectionInfo), CompressionNever, testKeyGen))
	defer closeAndWait(c, rw)
	done := make(chan struct{})
	go func() { c.Close(errManual); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Close waited for an unstarted writer")
	}
	select {
	case <-m.closedCh:
	case <-time.After(time.Second):
		t.Fatal("cleanup did not notify model")
	}
	c.Start()
	select {
	case <-c.started:
		t.Fatal("closed connection started")
	default:
	}
	c.Close(errManual)
	if m.closedErr != errManual {
		t.Fatalf("close error = %v", m.closedErr)
	}
}

func TestConcurrentStartAndClose(t *testing.T) {
	for range 50 {
		m := newTestModel()
		rw := testutil.NewBlockingRW()
		c := getRawConnection(NewConnection(c0ID, rw, rw, rw, m, new(mockedConnectionInfo), CompressionNever, testKeyGen))
		ready := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); <-ready; c.Start() }()
		go func() { defer wg.Done(); <-ready; c.Close(errManual) }()
		close(ready)
		// Release I/O and complete cleanup for either ordering.
		rw.Close()
		done := make(chan struct{})
		go func() { wg.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("Start/Close deadlocked")
		}
		c.loopWG.Wait()
		select {
		case <-m.closedCh:
		case <-time.After(time.Second):
			t.Fatal("missing close callback")
		}
	}
}

func TestFolderCancellationSkipsQueuedBytesAndPreservesControl(t *testing.T) {
	var output bytes.Buffer
	c := newRawConnection(c0ID, bytes.NewReader(nil), &output, testutil.NoopCloser{}, nil, new(mockedConnectionInfo), CompressionNever)
	c.outbox = make(chan asyncMessage, 2)
	c.indexBox = make(chan asyncMessage, 1)
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	responseDone, indexDone, pingDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
	c.outbox <- asyncMessage{ctx: canceled, msg: (&Response{ID: 7, Data: []byte("revoked data")}).toWire(), done: responseDone}
	c.indexBox <- asyncMessage{ctx: canceled, msg: (&Index{Folder: "revoked"}).toWire(), done: indexDone}
	c.outbox <- asyncMessage{msg: &bep.Ping{}, done: pingDone}
	exited := make(chan struct{})
	go func() { defer close(exited); c.writerLoop() }()
	ccDone := make(chan struct{})
	c.clusterConfigBox <- clusterConfigMessage{config: &ClusterConfig{}, done: ccDone}
	<-ccDone
	<-responseDone
	<-indexDone
	<-pingDone
	close(c.closed)
	<-exited
	reader := newRawConnection(c1ID, bytes.NewReader(output.Bytes()), io.Discard, testutil.NoopCloser{}, nil, new(mockedConnectionInfo), CompressionNever)
	if _, err := reader.readMessage(make([]byte, 4)); err != nil {
		t.Fatal(err)
	}
	msg, err := reader.readMessage(make([]byte, 4))
	if err != nil {
		t.Fatal(err)
	}
	response, ok := msg.(*bep.Response)
	if !ok || len(response.Data) != 0 || response.Code == bep.ErrorCode_ERROR_CODE_NO_ERROR {
		t.Fatalf("revoked response: %v", msg)
	}
	msg, err = reader.readMessage(make([]byte, 4))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := msg.(*bep.Ping); !ok {
		t.Fatalf("shared control damaged: %v", msg)
	}
}

type heldFolderWriter struct{ entered, resume chan struct{} }

func (w heldFolderWriter) Write(p []byte) (int, error) {
	select {
	case w.entered <- struct{}{}:
	default:
	}
	<-w.resume
	return len(p), nil
}
func TestFolderIndexBorrowEndsAfterActualWrite(t *testing.T) {
	c := newRawConnection(c0ID, bytes.NewReader(nil), io.Discard, testutil.NoopCloser{}, nil, new(mockedConnectionInfo), CompressionNever)
	scope, cancel := context.WithCancel(context.Background())
	defer cancel()
	released := make(chan struct{})
	c.folderAdmit = func(context.Context, string) (context.Context, func(), error) {
		return scope, func() { close(released) }, nil
	}
	exited := make(chan struct{})
	go func() { defer close(exited); c.writerLoop() }()
	ccDone := make(chan struct{})
	c.clusterConfigBox <- clusterConfigMessage{config: &ClusterConfig{Folders: []Folder{{ID: "one"}}}, done: ccDone}
	<-ccDone
	// No write is active between the initial config and Index submission.
	writer := heldFolderWriter{make(chan struct{}, 1), make(chan struct{})}
	c.cw = &countingWriter{Writer: writer}
	done := make(chan error, 1)
	go func() { done <- c.Index(context.Background(), &Index{Folder: "one"}) }()
	<-writer.entered
	cancel()
	select {
	case <-released:
		t.Fatal("borrow released during write")
	default:
	}
	close(writer.resume)
	<-done
	<-released
	close(c.closed)
	<-exited
}

func TestFolderIndexCancellationDoesNotWaitForOtherFolderWrite(t *testing.T) {
	for _, update := range []bool{false, true} {
		for _, disconnect := range []bool{false, true} {
			t.Run(fmt.Sprintf("update=%v/disconnect=%v", update, disconnect), func(t *testing.T) {
				c := newRawConnection(c0ID, bytes.NewReader(nil), io.Discard, testutil.NoopCloser{}, nil, new(mockedConnectionInfo), CompressionNever)
				bCtx, cancelB := context.WithCancel(context.Background())
				defer cancelB()
				aReleased, bReleased, bAdmitted := make(chan struct{}), make(chan struct{}), make(chan struct{})
				c.folderAdmit = func(_ context.Context, folder string) (context.Context, func(), error) {
					switch folder {
					case "A":
						return context.Background(), func() { close(aReleased) }, nil
					case "B":
						close(bAdmitted)
						return bCtx, func() { close(bReleased) }, nil
					default:
						return context.Background(), func() {}, nil
					}
				}
				exited := make(chan struct{})
				go func() { defer close(exited); c.writerLoop() }()
				ccDone := make(chan struct{})
				c.clusterConfigBox <- clusterConfigMessage{config: &ClusterConfig{Folders: []Folder{{ID: "A"}, {ID: "B"}, {ID: "C"}}}, done: ccDone}
				<-ccDone
				writer := heldFolderWriter{make(chan struct{}, 1), make(chan struct{})}
				c.cw = &countingWriter{Writer: writer}
				var resumeOnce, closeOnce sync.Once
				resume := func() { resumeOnce.Do(func() { close(writer.resume) }) }
				shutdown := func() { closeOnce.Do(func() { close(c.closed) }) }
				aDone, bDone := make(chan error, 1), make(chan error, 1)
				aFinished, bFinished := make(chan struct{}), make(chan struct{})
				t.Cleanup(func() {
					cancelB()
					resume()
					shutdown()
					<-aFinished
					<-bFinished
					<-exited
				})
				go func() { defer close(aFinished); aDone <- c.Index(context.Background(), &Index{Folder: "A"}) }()
				<-writer.entered // A owns the serial slot and is inside the actual write.
				go func() {
					defer close(bFinished)
					if update {
						bDone <- c.IndexUpdate(context.Background(), &IndexUpdate{Folder: "B"})
					} else {
						bDone <- c.Index(context.Background(), &Index{Folder: "B"})
					}
				}()
				<-bAdmitted
				want := context.Canceled
				if disconnect {
					shutdown()
					want = ErrClosed
				} else {
					cancelB()
				}
				select {
				case err := <-bDone:
					if !errors.Is(err, want) {
						t.Fatalf("B: %v, want %v", err, want)
					}
				case <-time.After(time.Second):
					t.Fatal("B cancellation waited for A's blocked write")
				}
				select {
				case <-bReleased:
				default:
					t.Fatal("B retained its folder borrow")
				}
				select {
				case <-aReleased:
					t.Fatal("A released before actual write completion")
				default:
				}
				resume()
				if err := <-aDone; err != nil {
					t.Fatal(err)
				}
				<-aReleased
				if !disconnect {
					// B cancellation must neither close the shared connection nor
					// consume its serialization slot permanently.
					if err := c.IndexUpdate(context.Background(), &IndexUpdate{Folder: "C"}); err != nil {
						t.Fatal(err)
					}
				}
			})
		}
	}
}

func TestClusterConfigFenceRejectsWholeSnapshotAndReleases(t *testing.T) {
	c := newRawConnection(c0ID, bytes.NewReader(nil), io.Discard, testutil.NoopCloser{}, nil, new(mockedConnectionInfo), CompressionNever)
	releases := 0
	c.folderAdmit = func(ctx context.Context, folder string) (context.Context, func(), error) {
		if folder == "closed" {
			return nil, nil, context.Canceled
		}
		return ctx, func() { releases++ }, nil
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.ClusterConfig(&ClusterConfig{Folders: []Folder{{ID: "open"}, {ID: "closed"}}}, nil)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("partial snapshot queued")
	}
	if releases != 1 {
		t.Fatal("earlier borrow leaked", releases)
	}
}

func TestCanceledFirstClusterConfigWaitsForFreshSnapshot(t *testing.T) {
	var output bytes.Buffer
	c := newRawConnection(c0ID, bytes.NewReader(nil), &output, testutil.NoopCloser{}, nil, new(mockedConnectionInfo), CompressionNever)
	c.outbox = make(chan asyncMessage, 1)
	pingDone := make(chan struct{})
	c.outbox <- asyncMessage{msg: &bep.Ping{}, done: pingDone}
	exited := make(chan struct{})
	go func() { defer close(exited); c.writerLoop() }()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	staleDone := make(chan struct{})
	c.clusterConfigBox <- clusterConfigMessage{config: &ClusterConfig{Folders: []Folder{{ID: "stale"}}}, scopes: []context.Context{ctx}, done: staleDone}
	<-staleDone
	freshDone := make(chan struct{})
	c.clusterConfigBox <- clusterConfigMessage{config: &ClusterConfig{Folders: []Folder{{ID: "fresh"}}}, done: freshDone}
	<-freshDone
	<-pingDone
	close(c.closed)
	<-exited
	reader := newRawConnection(c1ID, bytes.NewReader(output.Bytes()), io.Discard, testutil.NoopCloser{}, nil, new(mockedConnectionInfo), CompressionNever)
	msg, err := reader.readMessage(make([]byte, 4))
	if err != nil {
		t.Fatal(err)
	}
	cc, ok := msg.(*bep.ClusterConfig)
	if !ok || len(cc.Folders) != 1 || cc.Folders[0].Id != "fresh" {
		t.Fatalf("first wire message: %v", msg)
	}
	msg, err = reader.readMessage(make([]byte, 4))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := msg.(*bep.Ping); !ok {
		t.Fatalf("control after fresh config: %v", msg)
	}
}

func TestPausedFolderAllowsSiblingUpdateAndReconnect(t *testing.T) {
	for _, reconnect := range []bool{false, true} {
		t.Run(fmt.Sprint(reconnect), func(t *testing.T) {
			var output bytes.Buffer
			c := newRawConnection(c0ID, bytes.NewReader(nil), &output, testutil.NoopCloser{}, nil, new(mockedConnectionInfo), CompressionNever)
			c.folderAdmit = func(context.Context, string) (context.Context, func(), error) {
				t.Error("announcement used data admission")
				return nil, nil, context.Canceled
			}
			c.announcementAdmit = func(ctx context.Context, id string, paused bool) (context.Context, func(), error) {
				if id == "a" && !paused {
					return nil, nil, context.Canceled
				}
				return ctx, func() {}, nil
			}
			exited := make(chan struct{})
			go func() { defer close(exited); c.writerLoop() }()
			if !reconnect {
				c.ClusterConfig(&ClusterConfig{}, nil)
			}
			c.ClusterConfig(&ClusterConfig{Folders: []Folder{{ID: "a", StopReason: FolderStopReasonPaused}, {ID: "b", Label: "updated"}}}, nil)
			close(c.closed)
			<-exited
			reader := newRawConnection(c1ID, bytes.NewReader(output.Bytes()), io.Discard, testutil.NoopCloser{}, nil, new(mockedConnectionInfo), CompressionNever)
			if !reconnect {
				if _, err := reader.readMessage(make([]byte, 4)); err != nil {
					t.Fatal(err)
				}
			}
			msg, err := reader.readMessage(make([]byte, 4))
			if err != nil {
				t.Fatal(err)
			}
			cc, ok := msg.(*bep.ClusterConfig)
			if !ok || len(cc.Folders) != 2 || cc.Folders[1].Label != "updated" {
				t.Fatalf("incomplete config: %v", msg)
			}
		})
	}
}

func TestDownloadProgressAdmissionAndQueuedRevocation(t *testing.T) {
	var output bytes.Buffer
	c := newRawConnection(c0ID, bytes.NewReader(nil), &output, testutil.NoopCloser{}, nil, new(mockedConnectionInfo), CompressionNever)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	released := make(chan struct{})
	c.folderAdmit = func(_ context.Context, folder string) (context.Context, func(), error) {
		if folder == "denied" {
			return nil, nil, context.Canceled
		}
		if folder == "revoked" {
			return ctx, func() { close(released) }, nil
		}
		return context.Background(), func() {}, nil
	}
	c.DownloadProgress(context.Background(), &DownloadProgress{Folder: "denied"})
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.DownloadProgress(context.Background(), &DownloadProgress{Folder: "revoked"})
	}()
	// Hold the message after enqueue, before the writer can serialize it.
	msg := <-c.outbox
	cancel()
	select {
	case <-released:
		t.Fatal("released before queued work completed")
	default:
	}
	c.outbox = make(chan asyncMessage, 1)
	c.outbox <- msg
	exited := make(chan struct{})
	go func() { defer close(exited); c.writerLoop() }()
	ccDone := make(chan struct{})
	c.clusterConfigBox <- clusterConfigMessage{config: &ClusterConfig{}, done: ccDone}
	<-ccDone
	<-done
	<-released
	c.DownloadProgress(context.Background(), &DownloadProgress{Folder: "other"})
	close(c.closed)
	<-exited
	reader := newRawConnection(c1ID, bytes.NewReader(output.Bytes()), io.Discard, testutil.NoopCloser{}, nil, new(mockedConnectionInfo), CompressionNever)
	if _, err := reader.readMessage(make([]byte, 4)); err != nil {
		t.Fatal(err)
	}
	message, err := reader.readMessage(make([]byte, 4))
	if err != nil {
		t.Fatal(err)
	}
	progress, ok := message.(*bep.DownloadProgress)
	if !ok || progress.Folder != "other" {
		t.Fatalf("revoked metadata or sibling damage: %v", message)
	}
	if _, err := reader.readMessage(make([]byte, 4)); !errors.Is(err, io.EOF) {
		t.Fatalf("extra wire message: %v", err)
	}
}

func TestDownloadProgressBorrowEndsAfterWrite(t *testing.T) {
	c := newRawConnection(c0ID, bytes.NewReader(nil), io.Discard, testutil.NoopCloser{}, nil, new(mockedConnectionInfo), CompressionNever)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	released := make(chan struct{})
	c.folderAdmit = func(context.Context, string) (context.Context, func(), error) {
		return ctx, func() { close(released) }, nil
	}
	exited := make(chan struct{})
	go func() { defer close(exited); c.writerLoop() }()
	ccDone := make(chan struct{})
	c.clusterConfigBox <- clusterConfigMessage{config: &ClusterConfig{}, done: ccDone}
	<-ccDone
	writer := heldFolderWriter{make(chan struct{}, 1), make(chan struct{})}
	c.cw = &countingWriter{Writer: writer}
	done := make(chan struct{})
	go func() { defer close(done); c.DownloadProgress(context.Background(), &DownloadProgress{Folder: "one"}) }()
	<-writer.entered
	cancel()
	select {
	case <-released:
		t.Error("borrow released during active write")
	default:
	}
	close(writer.resume)
	<-done
	<-released
	close(c.closed)
	<-exited
}

func TestClusterConfigUnsentCancellationReleasesWholeSnapshot(t *testing.T) {
	for _, canceledFolder := range []string{"A", "B"} {
		t.Run(canceledFolder, func(t *testing.T) {
			c := newRawConnection(c0ID, bytes.NewReader(nil), io.Discard, testutil.NoopCloser{}, nil, new(mockedConnectionInfo), CompressionNever)
			defer close(c.closed)
			scopes := make(map[string]context.Context)
			cancels := make(map[string]context.CancelFunc)
			released := make(map[string]chan struct{})
			for _, id := range []string{"A", "B"} {
				scopes[id], cancels[id] = context.WithCancel(context.Background())
				defer cancels[id]()
				released[id] = make(chan struct{})
			}
			admitted := make(chan struct{})
			c.announcementAdmit = func(_ context.Context, id string, _ bool) (context.Context, func(), error) {
				if id == "B" {
					close(admitted)
				}
				return scopes[id], func() { close(released[id]) }, nil
			}
			done := make(chan struct{})
			go func() {
				defer close(done)
				c.ClusterConfig(&ClusterConfig{Folders: []Folder{{ID: "A"}, {ID: "B"}}}, nil)
			}()
			<-admitted // No writer consumes clusterConfigBox: the snapshot cannot be enqueued.
			cancels[canceledFolder]()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("unsent snapshot did not cancel")
			}
			for _, ch := range released {
				select {
				case <-ch:
				default:
					t.Fatal("snapshot borrow not released")
				}
			}
			other := "A"
			if canceledFolder == "A" {
				other = "B"
			}
			if scopes[other].Err() != nil {
				t.Fatal("sibling scope canceled")
			}
			select {
			case <-c.clusterConfigBox:
				t.Fatal("partial or stale snapshot sent")
			default:
			}
		})
	}
}

func TestClusterConfigAcceptedCancellationWaitsForCompletion(t *testing.T) {
	c := newRawConnection(c0ID, bytes.NewReader(nil), io.Discard, testutil.NoopCloser{}, nil, new(mockedConnectionInfo), CompressionNever)
	defer close(c.closed)
	scope, cancel := context.WithCancel(context.Background())
	defer cancel()
	released := make(chan struct{})
	c.folderAdmit = func(context.Context, string) (context.Context, func(), error) {
		return scope, func() { close(released) }, nil
	}
	done := make(chan struct{})
	go func() { defer close(done); c.ClusterConfig(&ClusterConfig{Folders: []Folder{{ID: "A"}}}, nil) }()
	message := <-c.clusterConfigBox
	cancel()
	select {
	case <-released:
		t.Fatal("released before writer completion")
	default:
	}
	if written, err := c.writeClusterConfig(message); err != nil || written {
		t.Fatalf("canceled snapshot written: %v %v", written, err)
	}
	<-done
	<-released
	// A fresh sibling snapshot still completes on the same connection.
	c.folderAdmit = func(ctx context.Context, _ string) (context.Context, func(), error) { return ctx, func() {}, nil }
	doneFresh := make(chan struct{})
	go func() { c.ClusterConfig(&ClusterConfig{Folders: []Folder{{ID: "B"}}}, nil); close(doneFresh) }()
	fresh := <-c.clusterConfigBox
	if written, err := c.writeClusterConfig(fresh); err != nil || !written {
		t.Fatalf("fresh sibling failed: %v %v", written, err)
	}
	<-doneFresh
}

func TestClusterConfigWritingCancellationRetainsBorrow(t *testing.T) {
	writer := heldFolderWriter{make(chan struct{}, 1), make(chan struct{})}
	c := newRawConnection(c0ID, bytes.NewReader(nil), writer, testutil.NoopCloser{}, nil, new(mockedConnectionInfo), CompressionNever)
	scope, cancel := context.WithCancel(context.Background())
	defer cancel()
	released := make(chan struct{})
	c.folderAdmit = func(context.Context, string) (context.Context, func(), error) {
		return scope, func() { close(released) }, nil
	}
	exited := make(chan struct{})
	go func() { defer close(exited); c.writerLoop() }()
	var resume sync.Once
	defer func() { resume.Do(func() { close(writer.resume) }); close(c.closed); <-exited }()
	done := make(chan struct{})
	go func() { defer close(done); c.ClusterConfig(&ClusterConfig{Folders: []Folder{{ID: "A"}}}, nil) }()
	<-writer.entered
	cancel()
	select {
	case <-released:
		t.Fatal("borrow released during active write")
	default:
	}
	resume.Do(func() { close(writer.resume) })
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("write did not complete")
	}
	<-released
}

// Pause send's context evaluation after registration, without a timing poll.
type requestEnqueueContext struct {
	context.Context
	entered, resume chan struct{}
	reads           int
}

func (c *requestEnqueueContext) Done() <-chan struct{} {
	c.reads++
	if c.reads == 2 {
		close(c.entered)
		<-c.resume
	}
	return c.Context.Done()
}

func TestRequestFailedEnqueueRetiresAwaiting(t *testing.T) {
	for _, disconnect := range []bool{false, true} {
		t.Run(fmt.Sprint(disconnect), func(t *testing.T) {
			c := getRawConnection(NewConnection(c0ID, bytes.NewReader(nil), io.Discard, testutil.NoopCloser{}, newTestModel(), new(mockedConnectionInfo), CompressionNever, testKeyGen))
			base, cancel := context.WithCancel(context.Background())
			defer cancel()
			ctx := &requestEnqueueContext{Context: base, entered: make(chan struct{}), resume: make(chan struct{})}
			done := make(chan error, 1)
			go func() { _, err := c.Request(ctx, &Request{Folder: "A", Name: "file"}); done <- err }()
			<-ctx.entered
			if disconnect {
				c.internalClose(ErrClosed)
			} else {
				cancel()
			}
			close(ctx.resume)
			if err := <-done; !errors.Is(err, ErrClosed) {
				t.Fatal("enqueue error changed", err)
			}
			c.awaitingMut.Lock()
			n := len(c.awaiting)
			c.awaitingMut.Unlock()
			if n != 0 {
				t.Fatalf("awaiting retained: %d", n)
			}
		})
	}
}

func TestRequestCancelIgnoresLateResponse(t *testing.T) {
	c := newRawConnection(c0ID, bytes.NewReader(nil), io.Discard, testutil.NoopCloser{}, nil, new(mockedConnectionInfo), CompressionNever)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := c.Request(ctx, &Request{Folder: "A", Name: "old"}); done <- err }()
	old := (<-c.outbox).msg.(*bep.Request)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	c.awaitingMut.Lock()
	n := len(c.awaiting)
	c.awaitingMut.Unlock()
	if n != 0 {
		t.Fatalf("canceled request retained: %d", n)
	}
	result := make(chan asyncResult, 1)
	go func() {
		data, err := c.Request(context.Background(), &Request{Folder: "B", Name: "new"})
		result <- asyncResult{data, err}
	}()
	next := (<-c.outbox).msg.(*bep.Request)
	c.handleResponse(&Response{ID: int(old.Id), Data: []byte("late")})
	select {
	case <-result:
		t.Fatal("late response completed new request")
	default:
	}
	c.handleResponse(&Response{ID: int(next.Id), Data: []byte("current")})
	got := <-result
	if got.err != nil || string(got.val) != "current" {
		t.Fatalf("new request: %q %v", got.val, got.err)
	}
	c.awaitingMut.Lock()
	n = len(c.awaiting)
	c.awaitingMut.Unlock()
	if n != 0 {
		t.Fatalf("completed request retained: %d", n)
	}
}

func TestRequestCancelResponseCloseRace(t *testing.T) {
	for i := 0; i < 50; i++ {
		c := getRawConnection(NewConnection(c0ID, bytes.NewReader(nil), io.Discard, testutil.NoopCloser{}, newTestModel(), new(mockedConnectionInfo), CompressionNever, testKeyGen))
		ctx, cancel := context.WithCancel(context.Background())
		result := make(chan asyncResult, 1)
		go func() {
			data, err := c.Request(ctx, &Request{Folder: "A", Name: "file"})
			result <- asyncResult{data, err}
		}()
		req := (<-c.outbox).msg.(*bep.Request)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for _, action := range []func(){cancel, func() { c.handleResponse(&Response{ID: int(req.Id), Data: []byte("data")}) }, func() { c.internalClose(ErrClosed) }} {
			wg.Add(1)
			go func(f func()) { defer wg.Done(); <-start; f() }(action)
		}
		close(start)
		wg.Wait()
		got := <-result
		if got.err == nil {
			if string(got.val) != "data" {
				t.Fatal("wrong response")
			}
		} else if !errors.Is(got.err, context.Canceled) && !errors.Is(got.err, ErrClosed) {
			t.Fatal(got.err)
		}
		c.handleResponse(&Response{ID: int(req.Id), Data: []byte("duplicate")})
		c.awaitingMut.Lock()
		n := len(c.awaiting)
		c.awaitingMut.Unlock()
		if n != 0 {
			t.Fatalf("awaiting after race: %d", n)
		}
	}
}

// Acceptance into the writer is not proof that bytes were written. Cancellation
// before write must be distinguishable from cancellation during a full write.
func TestFolderIndexWriteOutcome(t *testing.T) {
	for _, update := range []bool{false, true} {
		for _, mode := range []string{"skipped", "written", "failed"} {
			t.Run(fmt.Sprintf("update=%v/%s", update, mode), func(t *testing.T) {
				c := getRawConnection(NewConnection(c0ID, bytes.NewReader(nil), io.Discard, testutil.NoopCloser{}, newTestModel(), new(mockedConnectionInfo), CompressionNever, testKeyGen))
				scope, cancel := context.WithCancelCause(context.Background())
				defer cancel(nil)
				released := make(chan struct{})
				c.folderAdmit = func(context.Context, string) (context.Context, func(), error) {
					return scope, func() { close(released) }, nil
				}
				c.recordWrittenAnnouncement(&ClusterConfig{Folders: []Folder{{ID: "folder"}}})
				result := make(chan error, 1)
				indexDone := make(chan struct{})
				go func() {
					defer close(indexDone)
					if update {
						result <- c.IndexUpdate(context.Background(), &IndexUpdate{Folder: "folder"})
					} else {
						result <- c.Index(context.Background(), &Index{Folder: "folder"})
					}
				}()
				// Intercept the unbuffered handoff to deterministically order cancellation
				// before the writer examines this exact queued message.
				msg := <-c.indexBox
				if mode == "skipped" {
					cancel(ErrFolderAccessClosed)
				}
				exited := make(chan struct{})
				go func() { defer close(exited); c.writerLoop() }()
				ccDone := make(chan struct{})
				c.clusterConfigBox <- clusterConfigMessage{config: &ClusterConfig{Folders: []Folder{{ID: "folder"}}}, done: ccDone}
				<-ccDone
				writer := &indexOutcomeWriter{entered: make(chan struct{}), resume: make(chan struct{}), fail: mode == "failed"}
				c.cw = &countingWriter{Writer: writer}
				var resumeOnce sync.Once
				resume := func() { resumeOnce.Do(func() { close(writer.resume) }) }
				defer func() { cancel(nil); resume(); c.internalClose(errManual); <-exited; <-indexDone }()
				c.indexBox <- msg
				if mode != "skipped" {
					<-writer.entered
					cancel(ErrFolderAccessClosed)
					select {
					case <-released:
						t.Fatal("released before actual write completion")
					default:
					}
					resume()
				}
				err := <-result
				switch mode {
				case "skipped":
					if !errors.Is(err, ErrFolderAccessClosed) {
						t.Fatalf("skipped: %v", err)
					}
				case "written":
					if err != nil {
						t.Fatalf("complete write misreported as cancellation: %v", err)
					}
				case "failed":
					if !errors.Is(err, io.ErrClosedPipe) {
						t.Fatalf("write error lost: %v", err)
					}
				}
				<-released
				if mode == "failed" {
					<-c.Closed()
				} else {
					c.internalClose(errManual)
				}
				<-exited
			})
		}
	}
}

type indexOutcomeWriter struct {
	entered, resume chan struct{}
	once            sync.Once
	fail            bool
}

func (w *indexOutcomeWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.entered) })
	<-w.resume
	if w.fail {
		return 0, io.ErrClosedPipe
	}
	return len(p), nil
}

// A reopened data fence is not evidence that the peer has received our new
// folder announcement. Exercise the real writer's ordering on an existing BEP.
func TestFolderIndexWaitsForWrittenAnnouncement(t *testing.T) {
	for _, update := range []bool{false, true} {
		t.Run(fmt.Sprint(update), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var output bytes.Buffer
				c := newRawConnection(c0ID, bytes.NewReader(nil), &output, testutil.NoopCloser{}, nil, new(mockedConnectionInfo), CompressionNever)
				c.folderAdmit = func(ctx context.Context, _ string) (context.Context, func(), error) { return ctx, func() {}, nil }
				exited := make(chan struct{})
				go func() { defer close(exited); c.writerLoop() }()
				defer func() { close(c.closed); <-exited }()
				c.ClusterConfig(&ClusterConfig{Folders: []Folder{{ID: "existing"}}}, nil)
				done := make(chan error, 1)
				go func() {
					if update {
						done <- c.IndexUpdate(context.Background(), &IndexUpdate{Folder: "reverse"})
					} else {
						done <- c.Index(context.Background(), &Index{Folder: "reverse"})
					}
				}()
				synctest.Wait()
				select {
				case err := <-done:
					t.Fatalf("index passed its withheld announcement: %v", err)
				default:
				}
				if err := c.Index(context.Background(), &Index{Folder: "existing"}); err != nil {
					t.Fatal(err)
				}
				c.ClusterConfig(&ClusterConfig{Folders: []Folder{{ID: "existing"}, {ID: "reverse"}}}, nil)
				if err := <-done; err != nil {
					t.Fatal(err)
				}
				reader := newRawConnection(c1ID, bytes.NewReader(output.Bytes()), io.Discard, testutil.NoopCloser{}, nil, new(mockedConnectionInfo), CompressionNever)
				for i := 0; i < 4; i++ {
					msg, err := reader.readMessage(make([]byte, 4))
					if err != nil {
						t.Fatal(err)
					}
					if i == 2 {
						if cc, ok := msg.(*bep.ClusterConfig); !ok || len(cc.Folders) != 2 {
							t.Fatalf("announcement must precede new index: %T", msg)
						}
					}
					if i == 3 {
						switch m := msg.(type) {
						case *bep.Index:
							if m.Folder != "reverse" {
								t.Fatal(m)
							}
						case *bep.IndexUpdate:
							if m.Folder != "reverse" {
								t.Fatal(m)
							}
						default:
							t.Fatalf("last message: %T", msg)
						}
					}
				}
			})
		})
	}
}

func TestFolderIndexAnnouncementWaitCancellationAndWithdrawal(t *testing.T) {
	for _, mode := range []string{"cancel", "close", "withdraw", "pause", "canceled-announcement", "failed-announcement"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				c := newRawConnection(c0ID, bytes.NewReader(nil), io.Discard, testutil.NoopCloser{}, nil, new(mockedConnectionInfo), CompressionNever)
				c.folderAdmit = func(ctx context.Context, _ string) (context.Context, func(), error) { return ctx, func() {}, nil }
				exited := make(chan struct{})
				go func() { defer close(exited); c.writerLoop() }()
				var once sync.Once
				shutdown := func() { once.Do(func() { close(c.closed) }) }
				defer func() { shutdown(); <-exited }()
				cc := &ClusterConfig{Folders: []Folder{{ID: "existing"}, {ID: "reverse"}}}
				if mode == "withdraw" || mode == "pause" {
					c.ClusterConfig(cc, nil)
					c.idxGate <- struct{}{} // Let the sender observe readiness, but delay its handoff.
				} else {
					c.ClusterConfig(&ClusterConfig{Folders: []Folder{{ID: "existing"}}}, nil)
				}
				ctx, cancel := context.WithCancelCause(context.Background())
				defer cancel(nil)
				done := make(chan error, 1)
				go func() { done <- c.Index(ctx, &Index{Folder: "reverse"}) }()
				synctest.Wait()
				if mode == "withdraw" || mode == "pause" {
					next := &ClusterConfig{Folders: []Folder{{ID: "existing"}}}
					if mode == "pause" {
						next.Folders = append(next.Folders, Folder{ID: "reverse", StopReason: FolderStopReasonPaused})
					}
					c.ClusterConfig(next, nil)
					<-c.idxGate
				}
				if mode == "canceled-announcement" || mode == "failed-announcement" {
					scoped, abort := context.WithCancel(context.Background())
					abort()
					message := clusterConfigMessage{config: cc, scopes: []context.Context{scoped}, done: make(chan struct{})}
					if mode == "failed-announcement" {
						message.scopes = nil
						c.cw = &countingWriter{Writer: announcementErrorWriter{}}
					}
					// No writer operation is active: test the exact completion helper.
					_, err := c.writeClusterConfig(message)
					if mode == "failed-announcement" {
						if !errors.Is(err, io.ErrClosedPipe) {
							t.Fatal(err)
						}
						c.cw = &countingWriter{Writer: io.Discard}
					}
				}
				synctest.Wait()
				select {
				case err := <-done:
					t.Fatalf("index passed missing/withdrawn announcement: %v", err)
				default:
				}
				if err := c.Index(context.Background(), &Index{Folder: "existing"}); err != nil {
					t.Fatal(err)
				}
				if mode == "close" {
					shutdown()
					if err := <-done; !errors.Is(err, ErrClosed) {
						t.Fatal(err)
					}
				} else if mode == "cancel" {
					cancel(ErrFolderAccessClosed)
					if err := <-done; !errors.Is(err, ErrFolderAccessClosed) {
						t.Fatal(err)
					}
				} else {
					c.ClusterConfig(cc, nil)
					if err := <-done; err != nil {
						t.Fatal(err)
					}
				}
			})
		})
	}
}

type announcementErrorWriter struct{}

func (announcementErrorWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestFolderIndexSecondaryPromotionRestoresAnnouncementGate(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := newRawConnection(c0ID, bytes.NewReader(nil), io.Discard, testutil.NoopCloser{}, nil, new(mockedConnectionInfo), CompressionNever)
		c.folderAdmit = func(ctx context.Context, _ string) (context.Context, func(), error) { return ctx, func() {}, nil }
		exited := make(chan struct{})
		go func() { defer close(exited); c.writerLoop() }()
		defer func() { close(c.closed); <-exited }()
		c.ClusterConfig(&ClusterConfig{Secondary: true}, nil)
		if err := c.Index(context.Background(), &Index{Folder: "reverse"}); err != nil {
			t.Fatal(err)
		}
		// Promotion makes this connection responsible for the complete metadata.
		c.ClusterConfig(&ClusterConfig{Folders: []Folder{{ID: "existing"}}}, nil)
		done := make(chan error, 1)
		go func() { done <- c.Index(context.Background(), &Index{Folder: "reverse"}) }()
		synctest.Wait()
		select {
		case err := <-done:
			t.Fatalf("secondary readiness survived promotion: %v", err)
		default:
		}
		c.ClusterConfig(&ClusterConfig{Folders: []Folder{{ID: "existing"}, {ID: "reverse"}}}, nil)
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	})
}

// Observe real IndexUpdate -> index lane -> writer boundaries, holding each stage
// independently. Virtual time measures queue and write delays without sleeps
// depending on scheduler speed or network performance.
func TestIndexWireDiagnostics(t *testing.T) {
	for _, mode := range []string{"written", "failed", "queue_canceled", "canceled_before_write"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var output indexTraceBuffer
				old := slog.Default()
				slog.SetDefault(slog.New(slog.NewJSONHandler(&output, nil)))
				defer slog.SetDefault(old)
				info := new(mockedConnectionInfo)
				info.ConnectionIDReturns("diagnostic-connection")
				c := getRawConnection(NewConnection(c0ID, bytes.NewReader(nil), io.Discard, testutil.NoopCloser{}, newTestModel(), info, CompressionNever, testKeyGen))
				ctx, cancel := context.WithCancelCause(context.Background())
				defer cancel(nil)
				c.folderAdmit = func(context.Context, string) (context.Context, func(), error) { return ctx, func() {}, nil }
				c.recordWrittenAnnouncement(&ClusterConfig{Folders: []Folder{{ID: "folder"}}})
				result := make(chan error, 1)
				go func() {
					result <- c.IndexUpdate(context.Background(), &IndexUpdate{Folder: "folder", LastSequence: 17, Files: []FileInfo{{Name: "never-log-filename"}}})
				}()
				synctest.Wait()
				first := output.events(t)
				if len(first) != 1 || first[0]["phase"] != "queued" {
					t.Fatalf("missing queued boundary: %v", first)
				}
				time.Sleep(3 * time.Second)
				if mode == "queue_canceled" {
					cancel(ErrFolderAccessClosed)
					if err := <-result; !errors.Is(err, ErrFolderAccessClosed) {
						t.Fatal(err)
					}
					events := output.events(t)
					if len(events) != 2 || events[1]["phase"] != "queue_failed" || events[1]["since_queue"] != float64(3*time.Second) {
						t.Fatal(events)
					}
					c.internalClose(errManual)
					return
				}
				msg := <-c.indexBox
				if mode == "canceled_before_write" {
					cancel(ErrFolderAccessClosed)
				}
				exited := make(chan struct{})
				go func() { defer close(exited); c.writerLoop() }()
				ccDone := make(chan struct{})
				c.clusterConfigBox <- clusterConfigMessage{config: &ClusterConfig{Folders: []Folder{{ID: "folder"}}}, done: ccDone}
				<-ccDone
				writer := &indexOutcomeWriter{entered: make(chan struct{}), resume: make(chan struct{}), fail: mode == "failed"}
				c.cw = &countingWriter{Writer: writer}
				var once sync.Once
				resume := func() { once.Do(func() { close(writer.resume) }) }
				defer func() { resume(); c.internalClose(errManual); <-exited }()
				c.indexBox <- msg
				if mode != "canceled_before_write" {
					<-writer.entered
					events := output.events(t)
					if len(events) != 2 || events[1]["phase"] != "write_started" || events[1]["since_queue"] != float64(3*time.Second) {
						t.Fatal(events)
					}
					time.Sleep(2 * time.Second)
					resume()
				}
				err := <-result
				if (mode == "written") != (err == nil) {
					t.Fatalf("outcome %s: %v", mode, err)
				}
				events := output.events(t)
				phase := "write_completed"
				if mode == "failed" {
					phase = "write_failed"
				}
				if mode == "canceled_before_write" {
					phase = "canceled_before_write"
				}
				last := events[len(events)-1]
				if last["phase"] != phase {
					t.Fatal(events)
				}
				if mode != "canceled_before_write" && (last["since_queue"] != float64(5*time.Second) || last["write_elapsed"] != float64(2*time.Second)) {
					t.Fatal(last)
				}
				for _, ev := range events {
					if ev["folder"] != "folder" || ev["sequence"] != float64(17) || ev["items"] != float64(1) || ev["connection_id"] != "diagnostic-connection" {
						t.Fatal(ev)
					}
					if _, err := time.Parse(time.RFC3339Nano, ev["event_time"].(string)); err != nil {
						t.Fatal(err)
					}
				}
				if strings.Contains(output.text(), "never-log-filename") {
					t.Fatal("index payload leaked")
				}
			})
		})
	}
}

type indexTraceBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *indexTraceBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}
func (b *indexTraceBuffer) text() string { b.mu.Lock(); defer b.mu.Unlock(); return b.buf.String() }
func (b *indexTraceBuffer) events(t *testing.T) []map[string]any {
	t.Helper()
	var result []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(b.text()), "\n") {
		var ev map[string]any
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatal(err)
		}
		if ev["msg"] == "Index wire" {
			result = append(result, ev)
		}
	}
	return result
}

// steppedProtocolWriter holds each complete frame until the test releases it.
// Both producers can reach their real unbuffered handoff before the next choice.
type steppedProtocolWriter struct {
	frames chan []byte
	resume chan error
	closed chan struct{}
	once   sync.Once
}

func (w *steppedProtocolWriter) Write(p []byte) (int, error) {
	select {
	case w.frames <- append([]byte(nil), p...):
	case <-w.closed:
		return 0, io.ErrClosedPipe
	}
	select {
	case err := <-w.resume:
		if err != nil {
			return 0, err
		}
		return len(p), nil
	case <-w.closed:
		return 0, io.ErrClosedPipe
	}
}
func (w *steppedProtocolWriter) Close() error {
	w.once.Do(func() { close(w.closed) })
	return nil
}
func (w *steppedProtocolWriter) next(t *testing.T) proto.Message {
	t.Helper()
	synctest.Wait()
	select {
	case frame := <-w.frames:
		reader := newRawConnection(c1ID, bytes.NewReader(frame), io.Discard, testutil.NoopCloser{}, nil, new(mockedConnectionInfo), CompressionNever)
		msg, err := reader.readMessage(make([]byte, 4))
		if err != nil {
			t.Fatal(err)
		}
		return msg
	default:
		t.Fatal("writer did not produce the next frame")
		return nil
	}
}
func newSteppedProtocolConnection(t *testing.T, admitted bool) (*rawConnection, *steppedProtocolWriter) {
	t.Helper()
	w := &steppedProtocolWriter{frames: make(chan []byte), resume: make(chan error), closed: make(chan struct{})}
	c := getRawConnection(NewConnection(c0ID, bytes.NewReader(nil), w, w, newTestModel(), new(mockedConnectionInfo), CompressionNever, testKeyGen))
	if admitted {
		c.folderAdmit = func(ctx context.Context, _ string) (context.Context, func(), error) { return ctx, func() {}, nil }
	}
	exited := make(chan struct{})
	go func() { defer close(exited); c.writerLoop() }()
	t.Cleanup(func() { c.internalClose(errManual); <-exited })
	ccDone := make(chan struct{})
	c.clusterConfigBox <- clusterConfigMessage{config: &ClusterConfig{Folders: []Folder{{ID: "folder"}}}, done: ccDone}
	if _, ok := w.next(t).(*bep.ClusterConfig); !ok {
		t.Fatal("missing initial configuration")
	}
	w.resume <- nil
	<-ccDone
	return c, w
}

func TestWriterIndexPriorityAndFairData(t *testing.T) {
	for _, admitted := range []bool{false, true} {
		for _, counts := range [][2]int{{8, 8}, {8, 0}, {0, 8}} {
			t.Run(fmt.Sprintf("admitted=%v/data=%d/index=%d", admitted, counts[0], counts[1]), func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					c, w := newSteppedProtocolConnection(t, admitted)
					var workers sync.WaitGroup
					defer func() { c.internalClose(errManual); workers.Wait() }()
					data := bytes.Repeat([]byte{0xab}, 64<<10)
					workers.Add(1)
					go func() {
						defer workers.Done()

						done := make(chan struct{})
						c.send(context.Background(), (&Response{ID: 0, Data: data}).toWire(), done)
						<-done
					}()
					if msg, ok := w.next(t).(*bep.Response); !ok || msg.Id != 0 {
						t.Fatal("missing leading block", msg)
					}
					// Queue bulk data first, then the new index while the first
					// block is in flight. FIFO sharing would put data ahead of it.
					workers.Add(1)
					go func() {
						defer workers.Done()

						for i := 1; i <= counts[0]; i++ {
							done := make(chan struct{})
							if !c.send(context.Background(), (&Response{ID: i, Data: data}).toWire(), done) {
								return
							}
							<-done
						}
					}()
					synctest.Wait()
					indexResult := make(chan error, 1)
					workers.Add(1)
					go func() {
						defer workers.Done()

						for i := 1; i <= counts[1]; i++ {
							var err error
							if i == 1 {
								err = c.Index(context.Background(), &Index{Folder: "folder", LastSequence: int64(i)})
							} else {
								err = c.IndexUpdate(context.Background(), &IndexUpdate{Folder: "folder", LastSequence: int64(i)})
							}
							if err != nil {
								indexResult <- err
								return
							}
						}
						indexResult <- nil
					}()
					synctest.Wait()
					w.resume <- nil
					for i := 1; i <= max(counts[0], counts[1]); i++ {
						if i <= counts[1] {
							msg := w.next(t)
							var sequence int64
							switch m := msg.(type) {
							case *bep.Index:
								sequence = m.LastSequence
							case *bep.IndexUpdate:
								sequence = m.LastSequence
							default:
								t.Fatalf("index %d delayed behind %T", i, msg)
							}
							if sequence != int64(i) {
								t.Fatal("index reordered", sequence, i)
							}
							w.resume <- nil
						}
						if i <= counts[0] {
							msg, ok := w.next(t).(*bep.Response)
							if !ok || msg.Id != int32(i) || !bytes.Equal(msg.Data, data) {
								t.Fatal("data starved, reordered or corrupted", i)
							}
							w.resume <- nil
						}
					}
					if err := <-indexResult; err != nil {
						t.Fatal(err)
					}
					workers.Wait()
				})
			})
		}
	}
}

func TestWriterPriorityKeepsAnnouncementAndCancellation(t *testing.T) {
	for _, mode := range []string{"withdraw", "cancel", "close", "write_failure"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				c, w := newSteppedProtocolConnection(t, true)
				var workers sync.WaitGroup
				defer func() { c.internalClose(errManual); workers.Wait() }()
				ctx, cancel := context.WithCancelCause(context.Background())
				defer cancel(nil)
				workers.Add(1)
				go func() {
					defer workers.Done()

					done := make(chan struct{})
					c.send(context.Background(), (&Response{ID: 1}).toWire(), done)
					<-done
				}()
				if _, ok := w.next(t).(*bep.Response); !ok {
					t.Fatal("missing in-flight data")
				}
				result := make(chan error, 1)
				workers.Add(1)
				go func() {
					defer workers.Done()
					result <- c.IndexUpdate(ctx, &IndexUpdate{Folder: "folder", LastSequence: 7})
				}()
				workers.Add(1)
				go func() {
					defer workers.Done()

					done := make(chan struct{})
					c.send(context.Background(), (&Response{ID: 2}).toWire(), done)
					<-done
				}()
				synctest.Wait()
				switch mode {
				case "withdraw":
					workers.Add(1)
					go func() {
						defer workers.Done()

						c.ClusterConfig(&ClusterConfig{Folders: []Folder{{ID: "folder", StopReason: FolderStopReasonPaused}}}, nil)
					}()
				case "cancel":
					cancel(ErrFolderAccessClosed)
					if err := <-result; !errors.Is(err, ErrFolderAccessClosed) {
						t.Fatal(err)
					}
				case "close":
					workers.Add(1)
					go func() {
						defer workers.Done()

						done := make(chan struct{})
						select {
						case c.closeBox <- asyncMessage{msg: &bep.Close{Reason: "test"}, done: done}:
							<-done
						case <-c.closed:
						}
					}()
				}
				synctest.Wait()
				w.resume <- nil
				switch mode {
				case "withdraw":
					if _, ok := w.next(t).(*bep.ClusterConfig); !ok {
						t.Fatal("withdrawal was overtaken")
					}
					w.resume <- nil
					if m, ok := w.next(t).(*bep.Response); !ok || m.Id != 2 {
						t.Fatal("withdrawn index was written or data blocked")
					}
					synctest.Wait()
					select {
					case err := <-result:
						t.Fatal("withdrawn index completed", err)
					default:
					}
					workers.Add(1)
					go func() {
						defer workers.Done()
						c.ClusterConfig(&ClusterConfig{Folders: []Folder{{ID: "folder"}}}, nil)
					}()
					synctest.Wait()
					w.resume <- nil
					if _, ok := w.next(t).(*bep.ClusterConfig); !ok {
						t.Fatal("missing resume announcement")
					}
					w.resume <- nil
					if m, ok := w.next(t).(*bep.IndexUpdate); !ok || m.LastSequence != 7 {
						t.Fatal("index did not resume")
					}
					w.resume <- nil
					if err := <-result; err != nil {
						t.Fatal(err)
					}
				case "cancel":
					if m, ok := w.next(t).(*bep.Response); !ok || m.Id != 2 {
						t.Fatal("canceled index wrote bytes")
					}
					w.resume <- nil
				case "close":
					if _, ok := w.next(t).(*bep.Close); !ok {
						t.Fatal("close lost priority")
					}
					w.resume <- nil
					c.internalClose(errManual)
					if err := <-result; !errors.Is(err, ErrClosed) {
						t.Fatal(err)
					}
				case "write_failure":
					if _, ok := w.next(t).(*bep.IndexUpdate); !ok {
						t.Fatal("index lost priority")
					}
					w.resume <- io.ErrClosedPipe
					if err := <-result; !errors.Is(err, io.ErrClosedPipe) {
						t.Fatal("write error lost", err)
					}
					<-c.closed
				}
				workers.Wait()
			})
		})
	}
}
