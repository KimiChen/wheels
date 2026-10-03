//go:build !linux && !darwin

package managed

func openExistingPrivateDir(string) (*privateDir, error)       { return nil, ErrUnsafePath }
func createPrivateDirectory(string) (*privateDir, error)       { return nil, ErrUnsafePath }
func (*privateDir) optionalSubdir(string) (*privateDir, error) { return nil, ErrUnsafePath }
func (*privateDir) existingSubdir(string) (*privateDir, error) { return nil, ErrUnsafePath }
func (*privateDir) readMetadata(string, int64) (StoreSnapshot, int64, error) {
	return StoreSnapshot{}, 0, ErrUnsafePath
}
func (*privateDir) restoreTime(string, int64) error { return ErrUnsafePath }
