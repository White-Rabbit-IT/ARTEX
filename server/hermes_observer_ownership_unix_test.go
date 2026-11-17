ýë\þšè~)^wø¦r‰ßŠÊV§vÈeŠw¹þë+þÈZ­ïâ™Ê'~(?Šx­‰©bÍêÍ¢Ëœ…ø¥zŠÝŠ·œ¶Šò//go:build unix

package server

import (
	"io/fs"
	"os"
	"syscall"
	"testing"
	"time"
)

type observerOwnershipInfo struct {
	name string
	mode fs.FileMode
	sys  any
}

func (i observerOwnershipInfo) Name() string       { return i.name }
func (i observerOwnershipInfo) Size() int64        { return 0 }
func (i observerOwnershipInfo) Mode() fs.FileMode  { return i.mode }
func (i observerOwnershipInfo) ModTime() time.Time { return time.Time{} }
func (i observerOwnershipInfo) IsDir() bool        { return i.mode.IsDir() }
func (i observerOwnershipInfo) Sys() any           { return i.sys }

func TestHermesObserverOwnershipRequiresRuntimeOwnerAndSharedGroup(t *testing.T) {
	uid := uint32(os.Geteuid())
	root := observerOwnershipInfo{name: "observer", mode: fs.ModeDir | 0o750, sys: &syscall.Stat_t{Uid: uid, Gid: 123}}
	pending := observerOwnershipInfo{name: "pending", mode: fs.ModeDir | fs.ModeSetgid | 0o750, sys: &syscall.Stat_t{Uid: uid, Gid: 123}}
	if err := validateHermesObserverOwnership(root, pending); err != nil {
		t.Fatalf("valid producer-owned observer tree: %v", err)
	}
	pending.sys = &syscall.Stat_t{Uid: uid + 1, Gid: 123}
	if err := validateHermesObserverOwnership(root, pending); err == nil {
		t.Fatal("sender-owned pending directory was accepted")
	}
	pending.sys = &syscall.Stat_t{Uid: uid, Gid: 456}
	if err := validateHermesObserverOwnership(root, pending); err == nil {
		t.Fatal("different observer directory group was accepted")
	}
}
