//go:build linux

package collect

import "syscall"

func statfs(path string) (fsStats, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return fsStats{}, err
	}
	// Block counts are in units of the fragment size, as df(1) uses.
	bs := uint64(st.Frsize)
	if bs == 0 {
		bs = uint64(st.Bsize)
	}
	return fsStats{
		total:     st.Blocks * bs,
		free:      st.Bfree * bs,
		avail:     st.Bavail * bs,
		files:     st.Files,
		filesFree: st.Ffree,
	}, nil
}
