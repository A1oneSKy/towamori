//go:build !linux

package collect

func statfs(string) (fsStats, error) {
	return fsStats{}, ErrUnsupported
}
