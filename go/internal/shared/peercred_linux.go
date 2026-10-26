//go:build linux

package shared

import "syscall"

// syscallPeerCred issues SO_PEERCRED and returns the kernel's struct ucred.
func syscallPeerCred(fd int) (peerCred, error) {
	cred, err := syscall.GetsockoptUcred(fd, solSocket, soPeerCred)
	if err != nil {
		return peerCred{}, err
	}
	return peerCred{Pid: cred.Pid, Uid: cred.Uid, Gid: cred.Gid}, nil
}
