//go:build unix

package mixnet

import (
	"os"
	"syscall"
)

// socketpair returns two connected stream sockets. Neither leaks into other
// child processes: they are marked close-on-exec under ForkLock, as the net
// package does, and exec.Cmd hands "theirs" to the child explicitly.
func socketpair() (ours, theirs *os.File, err error) {
	syscall.ForkLock.RLock()
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err == nil {
		syscall.CloseOnExec(fds[0])
		syscall.CloseOnExec(fds[1])
	}
	syscall.ForkLock.RUnlock()
	if err != nil {
		return nil, nil, os.NewSyscallError("socketpair", err)
	}
	return os.NewFile(uintptr(fds[0]), "nymdial-stream"), os.NewFile(uintptr(fds[1]), "nymdial-child"), nil
}
