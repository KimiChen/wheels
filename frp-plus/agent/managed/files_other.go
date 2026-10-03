//go:build !linux && !darwin

package managed

import "os"

// The managed writer is deliberately unavailable where its private descriptor
// and process-lock guarantees have not been implemented and tested.
type privateDir struct{}

func openPrivateDir(string) (*privateDir, error)              { return nil, ErrUnsafePath }
func (*privateDir) subdir(string) (*privateDir, error)        { return nil, ErrUnsafePath }
func (*privateDir) read(string, int64) (StoreSnapshot, error) { return StoreSnapshot{}, ErrUnsafePath }
func (*privateDir) replace(string, StoreSnapshot, string, int64, func(string) error) error {
	return ErrUnsafePath
}
func (*privateDir) lock() (*os.File, error)  { return nil, ErrUnsafePath }
func (*privateDir) names() ([]string, error) { return nil, ErrUnsafePath }
func (*privateDir) close()                   {}
