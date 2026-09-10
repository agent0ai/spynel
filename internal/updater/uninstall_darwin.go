package updater

/*
#include <libproc.h>
*/
import "C"

import (
	"errors"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

func installationWritable(path string) bool { return syscall.Access(path, 2) == nil }

func installationProcessIDs() ([]int, error) {
	processes, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return nil, err
	}
	ids := make([]int, 0, len(processes))
	for _, process := range processes {
		ids = append(ids, int(process.Proc.P_pid))
	}
	return ids, nil
}

func installationProcessPath(pid int) (string, error) {
	var path [C.PROC_PIDPATHINFO_MAXSIZE]byte
	if C.proc_pidpath(C.int(pid), unsafe.Pointer(&path[0]), C.uint32_t(len(path))) <= 0 {
		return "", errors.New("process executable unavailable")
	}
	return C.GoString((*C.char)(unsafe.Pointer(&path[0]))), nil
}
