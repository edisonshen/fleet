package rc

import (
	"os"
	"syscall"
)

// flockExNB / flockUn are tiny test helpers used by
// TestUp_ContestedOnConcurrentLock to seed the lock from outside
// withLock. Kept in a _test.go file so they don't leak into the
// production surface.
func flockExNB(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
}

func flockUn(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
