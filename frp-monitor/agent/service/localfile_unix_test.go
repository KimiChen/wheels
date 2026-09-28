//go:build unix

package service

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestOpenRegularRejectsFIFOReplacementWithoutBlocking(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credential")
	if err := os.WriteFile(path, []byte("token"), 0600); err != nil {
		t.Fatal(err)
	}
	before, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path, path+".old"); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		f, err := openRegular(path, before, 512, true)
		if f != nil {
			f.Close()
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("accepted FIFO replacement")
		}
	case <-time.After(time.Second):
		// Unblock a regressed blocking opener so the test does not leak it.
		if fd, err := unix.Open(path, unix.O_RDWR|unix.O_NONBLOCK, 0); err == nil {
			unix.Close(fd)
		}
		t.Fatal("opening a FIFO replacement blocked")
	}
}
