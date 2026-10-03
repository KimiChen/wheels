//go:build !linux && !darwin

package migrationbundle

type directory struct{}

func createDirectory(string) (*directory, error)    { return nil, ErrBundle }
func openDirectory(string) (*directory, error)      { return nil, ErrBundle }
func (*directory) write(string, []byte) error       { return ErrBundle }
func (*directory) read(string, int) ([]byte, error) { return nil, ErrBundle }
func (*directory) only(map[string]bool) error       { return ErrBundle }
func (*directory) sync() error                      { return ErrBundle }
func (*directory) check() error                     { return ErrBundle }
func (*directory) close()                           {}
