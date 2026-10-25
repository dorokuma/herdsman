//go:build !linux

package shared

import "fmt"

// syscallPeerCred is unsupported off Linux, mirroring the TS guard that returns
// undefined when process.platform !== "linux".
func syscallPeerCred(_ int) (peerCred, error) {
	return peerCred{}, fmt.Errorf("%w: SO_PEERCRED is Linux-only", errPeerCredUnavailable)
}
