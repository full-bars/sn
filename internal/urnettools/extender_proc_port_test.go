package urnettools

import "os"

// procPortStartReader serves one value for the sysctl port threshold and
// reports every other file as missing, so a test never reads the real host.
func procPortStartReader(value string, err error) func(string) ([]byte, error) {
	return func(path string) ([]byte, error) {
		if path != extenderSysctlPortStartPath {
			return nil, os.ErrNotExist
		}
		if err != nil {
			return nil, err
		}
		return []byte(value), nil
	}
}
