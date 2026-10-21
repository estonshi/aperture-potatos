//go:build unix

package jsonl

import (
	"os"
	"syscall"
)

// lock takes an flock on the whole file, which other processes honor too, so two writers never interleave lines.
func lock(f *os.File, exclusive bool) error {
	how := syscall.LOCK_SH
	if exclusive {
		how = syscall.LOCK_EX
	}
	for {
		err := syscall.Flock(int(f.Fd()), how)
		if err != syscall.EINTR {
			return err
		}
	}
}

func unlock(f *os.File) { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }
