//go:build linux

package userstats

import (
	"bytes"
	"context"
	"io"
	"net"
	"os"
	"testing"
	"time"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing/common/bufio"
	M "github.com/sagernet/sing/common/metadata"
	"golang.org/x/sys/unix"
)

// TestSplicePartialWriteBilling preserves the successful-forwarding contract:
// an interrupted destination write must not bill bytes stranded in the splice pipe.
// Two real socket pairs and a write deadline force the kernel partial-write path.
func TestSplicePartialWriteBilling(t *testing.T) {
	testSpliceWriteBilling(t, false)
}

func TestSpliceFailedWriteBilling(t *testing.T) {
	testSpliceWriteBilling(t, true)
}

func testSpliceWriteBilling(t *testing.T, closedDestination bool) {
	t.Helper()
	var connections [4]*net.UnixConn
	for i := range 2 {
		pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
		if err != nil {
			t.Fatal(err)
		}
		for j, descriptor := range pair {
			file := os.NewFile(uintptr(descriptor), "billing-splice")
			conn, connErr := net.FileConn(file)
			file.Close()
			if connErr != nil {
				t.Fatal(connErr)
			}
			t.Cleanup(func() { conn.Close() })
			if err = conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
				t.Fatal(err)
			}
			connections[i*2+j] = conn.(*net.UnixConn)
		}
	}
	input, source, destination, output := connections[0], connections[1], connections[2], connections[3]
	payload := bytes.Repeat([]byte{0x42}, 128*1024)
	if err := input.SetWriteBuffer(len(payload) * 2); err != nil {
		t.Fatal(err)
	}
	if _, err := input.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := input.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	if err := destination.SetWriteBuffer(1024); err != nil {
		t.Fatal(err)
	}
	if err := destination.SetWriteDeadline(time.Now().Add(100 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}

	registry := testRegistry(t)
	tracker := NewTracker(registry, log.NewNOPFactory().Logger())
	tracked := tracker.RoutedConnection(context.Background(), source, adapter.InboundContext{
		Inbound: "in", User: "u1", Destination: M.ParseSocksaddr("127.0.0.1:80"),
	}, nil, nil)
	defer tracked.Close()
	if closedDestination {
		if err := destination.Close(); err != nil {
			t.Fatal(err)
		}
	}
	written, copyErr := bufio.Copy(destination, tracked)
	if copyErr == nil {
		t.Fatal("expected forced partial-write error")
	}
	if !closedDestination {
		if err := destination.CloseWrite(); err != nil {
			t.Fatal(err)
		}
	}
	received, err := io.ReadAll(output)
	if err != nil {
		t.Fatal(err)
	}
	remaining, err := io.ReadAll(source)
	if err != nil {
		t.Fatal(err)
	}
	consumed := len(payload) - len(remaining)
	if (!closedDestination && len(received) == 0) || consumed <= len(received) {
		t.Fatalf("partial write not exercised: consumed=%d received=%d", consumed, len(received))
	}
	if written != int64(len(received)) {
		t.Errorf("copy result=%d received=%d", written, len(received))
	}
	snapshot, err := registry.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	user := userOf(t, snapshot, "in", "u1")
	if user.TCPUplinkBytes != uint64(len(received)) {
		t.Fatalf("forwarding billing mismatch: consumed=%d received=%d billed=%d", consumed, len(received), user.TCPUplinkBytes)
	}
}
