//go:build !unix && !windows

package service

import "os"

// Non-Unix filesystems do not have Unix FIFOs; openRegular still verifies the
// opened file's identity, type and permissions before reading any content.
func openLocalFile(path string) (*os.File, error) { return os.Open(path) }
