//go:build unix

package server

import (
	"fmt"
	"os"
	"syscall"
)

// validateHermesObserverOwnership closes the easy deployment mistake where the
// Hermes-login account owns pending/ and can forge or replace receipts. The
// supported observer deployment runs ARTEX as the same privileged container UID
// that owns the root-provisioned mount, while the Hermes user receives only
// group read/search access. POSIX ACLs are deliberately not treated as a source
// of write permission: deployments must not grant the sender write ACLs.
func validateHermesObserverOwnership(root, pending os.FileInfo) error {
	rootStat, ok := root.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("observer directory ownership could not be verified")
	}
	pendingStat, ok := pending.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("observer pending directory ownership could not be verified")
	}
	uid := uint32(os.Geteuid())
	if rootStat.Uid != uid || pendingStat.Uid != uid {
		return fmt.Errorf("observer directory and pending directory must be owned by the ARTEX runtime user")
	}
	if rootStat.Gid != pendingStat.Gid {
		return fmt.Errorf("observer directory and pending directory must use the same dedicated group")
	}
	return nil
}
