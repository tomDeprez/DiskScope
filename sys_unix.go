//go:build !windows

package main

import (
	"bufio"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

var realFS = map[string]bool{"ext2": true, "ext3": true, "ext4": true, "xfs": true, "btrfs": true, "zfs": true,
	"ntfs": true, "ntfs3": true, "fuseblk": true, "vfat": true, "exfat": true, "f2fs": true, "jfs": true, "reiserfs": true, "apfs": true}

func listDrives() []Drive {
	out := []Drive{}
	seen := map[string]bool{}
	add := func(mnt, dev, fsType string) {
		if seen[mnt] {
			return
		}
		var st syscall.Statfs_t
		if syscall.Statfs(mnt, &st) != nil {
			return
		}
		seen[mnt] = true
		bs := uint64(st.Bsize)
		out = append(out, Drive{Path: mnt, Label: dev, FS: fsType, Kind: "Local",
			Total: st.Blocks * bs, Free: st.Bavail * bs})
	}
	if f, err := os.Open("/proc/mounts"); err == nil {
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			fl := strings.Fields(sc.Text())
			if len(fl) < 3 || !realFS[fl[2]] || strings.HasPrefix(fl[0], "/dev/loop") {
				continue
			}
			add(strings.ReplaceAll(fl[1], `\040`, " "), fl[0], fl[2])
		}
		f.Close()
	}
	add("/", "", "")
	return out
}

func listProcs() []Proc {
	ents, _ := os.ReadDir("/proc")
	page := uint64(os.Getpagesize())
	var out []Proc
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		base := filepath.Join("/proc", e.Name())
		p := Proc{PID: pid}
		p.Exe, _ = os.Readlink(base + "/exe") // empty for other users' processes when not root
		if b, err := os.ReadFile(base + "/comm"); err == nil {
			p.Name = strings.TrimSpace(string(b))
		}
		if b, err := os.ReadFile(base + "/statm"); err == nil {
			if f := strings.Fields(string(b)); len(f) > 1 {
				rss, _ := strconv.ParseUint(f[1], 10, 64)
				p.Mem = rss * page
			}
		}
		out = append(out, p)
	}
	return out
}

func devOf(info fs.FileInfo) uint64 {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Dev)
	}
	return 0
}

func cloudOnly(fs.FileInfo) bool { return false }
