//go:build !linux && !darwin

package managed

func replaceV2Timed(*privateDir, string, StoreSnapshot, int64, string, int64, int64, func(string) error) error {
	return ErrUnsafePath
}
func v2MatchingFiles(*privateDir, string) ([]string, error) { return nil, ErrUnsafePath }
