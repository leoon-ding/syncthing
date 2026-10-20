package client

import (
	"context"
	"io"
	"net"
	"testing"
	"time"

	"github.com/syncthing/syncthing/lib/relay/protocol"
)

func TestJoinSessionFailureClosesSocket(t *testing.T) {
	for _, mode := range []string{"cancel", "reject", "malformed"} {
		t.Run(mode, func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			observed := make(chan error, 1)
			go func() {
				conn, err := listener.Accept()
				if err != nil {
					observed <- err
					return
				}
				defer conn.Close()
				conn.SetDeadline(time.Now().Add(2 * time.Second))
				if _, err = protocol.ReadMessage(conn); err != nil {
					observed <- err
					return
				}
				switch mode {
				case "cancel":
					cancel()
				case "reject":
					err = protocol.WriteMessage(conn, protocol.Response{Code: 1})
				case "malformed":
					err = protocol.WriteMessage(conn, protocol.Ping{})
				}
				if err != nil {
					observed <- err
					return
				}
				var b [1]byte
				_, err = conn.Read(b[:])
				observed <- err
			}()
			addr := listener.Addr().(*net.TCPAddr)
			conn, err := JoinSession(ctx, protocol.SessionInvitation{Address: addr.IP, Port: uint16(addr.Port), Key: []byte("test")})
			if err == nil || conn != nil {
				if conn != nil {
					conn.Close()
				}
				t.Fatal("unexpected success")
			}
			if err := <-observed; err != io.EOF {
				t.Fatalf("failed join did not release socket: %v", err)
			}
		})
	}
}
