//go:build windows

package main

import (
	"io/fs"
	"syscall"
	"unsafe"
)

var (
	k32                     = syscall.NewLazyDLL("kernel32.dll")
	pGetLogicalDrives       = k32.NewProc("GetLogicalDrives")
	pGetDriveType           = k32.NewProc("GetDriveTypeW")
	pGetDiskFreeSpaceEx     = k32.NewProc("GetDiskFreeSpaceExW")
	pGetVolumeInformation   = k32.NewProc("GetVolumeInformationW")
	pQueryFullProcImageName = k32.NewProc("QueryFullProcessImageNameW")
	pGetProcessMemoryInfo   = k32.NewProc("K32GetProcessMemoryInfo")
)

var driveKinds = map[uintptr]string{2: "Amovible", 3: "Local", 4: "Réseau", 5: "CD/DVD", 6: "RAM"}

func listDrives() []Drive {
	mask, _, _ := pGetLogicalDrives.Call()
	out := []Drive{}
	for i := 0; i < 26; i++ {
		if mask&(1<<i) == 0 {
			continue
		}
		root := string(rune('A'+i)) + `:\`
		rp, _ := syscall.UTF16PtrFromString(root)
		var free, total, totalFree uint64
		ok, _, _ := pGetDiskFreeSpaceEx.Call(uintptr(unsafe.Pointer(rp)),
			uintptr(unsafe.Pointer(&free)), uintptr(unsafe.Pointer(&total)), uintptr(unsafe.Pointer(&totalFree)))
		if ok == 0 { // no media (empty card reader, CD…)
			continue
		}
		t, _, _ := pGetDriveType.Call(uintptr(unsafe.Pointer(rp)))
		label, fsName := make([]uint16, 261), make([]uint16, 261)
		pGetVolumeInformation.Call(uintptr(unsafe.Pointer(rp)), uintptr(unsafe.Pointer(&label[0])), 261,
			0, 0, 0, uintptr(unsafe.Pointer(&fsName[0])), 261)
		out = append(out, Drive{
			Path: root, Label: syscall.UTF16ToString(label), FS: syscall.UTF16ToString(fsName),
			Kind: driveKinds[t], Total: total, Free: totalFree,
		})
	}
	return out
}

type memCounters struct {
	cb                                                 uint32
	PageFaultCount                                     uint32
	PeakWorkingSetSize, WorkingSetSize                 uintptr
	QuotaPeakPagedPoolUsage, QuotaPagedPoolUsage       uintptr
	QuotaPeakNonPagedPoolUsage, QuotaNonPagedPoolUsage uintptr
	PagefileUsage, PeakPagefileUsage                   uintptr
}

func listProcs() []Proc {
	snap, err := syscall.CreateToolhelp32Snapshot(syscall.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil
	}
	defer syscall.CloseHandle(snap)
	var e syscall.ProcessEntry32
	e.Size = uint32(unsafe.Sizeof(e))
	var out []Proc
	for err = syscall.Process32First(snap, &e); err == nil; err = syscall.Process32Next(snap, &e) {
		p := Proc{PID: int(e.ProcessID), Name: syscall.UTF16ToString(e.ExeFile[:])}
		// PROCESS_QUERY_LIMITED_INFORMATION | PROCESS_VM_READ, then limited only.
		h, err := syscall.OpenProcess(0x1000|0x0010, false, e.ProcessID)
		if err != nil {
			h, err = syscall.OpenProcess(0x1000, false, e.ProcessID)
		}
		if err == nil {
			buf := make([]uint16, 1024)
			n := uint32(len(buf))
			if r, _, _ := pQueryFullProcImageName.Call(uintptr(h), 0, uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&n))); r != 0 {
				p.Exe = syscall.UTF16ToString(buf[:n])
			}
			var mc memCounters
			mc.cb = uint32(unsafe.Sizeof(mc))
			if r, _, _ := pGetProcessMemoryInfo.Call(uintptr(h), uintptr(unsafe.Pointer(&mc)), uintptr(mc.cb)); r != 0 {
				p.Mem = uint64(mc.WorkingSetSize)
			}
			syscall.CloseHandle(h)
		}
		out = append(out, p)
	}
	return out
}

func devOf(fs.FileInfo) uint64 { return 0 }

// cloudOnly: OneDrive "online-only" placeholders report a size but use no disk.
func cloudOnly(info fs.FileInfo) bool {
	if a, ok := info.Sys().(*syscall.Win32FileAttributeData); ok {
		const offline, recallOnOpen, recallOnAccess = 0x1000, 0x40000, 0x400000
		return a.FileAttributes&(offline|recallOnOpen|recallOnAccess) != 0
	}
	return false
}
