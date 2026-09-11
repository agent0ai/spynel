package updater

/*
#include <libproc.h>
*/
import "C"

import (
	"bytes"
	"errors"
	"path/filepath"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

func installationWritable(path string) bool { return syscall.Access(path, 2) == nil }

func processImagePath(_ int, path string) string { return path }

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
	if count, err := C.proc_pidpath(C.int(pid), unsafe.Pointer(&path[0]), C.uint32_t(len(path))); count <= 0 {
		// npm unlinks the old image while it is still running. macOS retains
		// its launch-time executable path separately from argv[0] in procargs2.
		// Never expose the remaining argument/environment bytes.
		if data, argsErr := unix.SysctlRaw("kern.procargs2", pid); argsErr == nil && len(data) > 4 && len(data) <= 1<<20 {
			if end := bytes.IndexByte(data[4:], 0); end > 0 && end < len(path) {
				executable := string(data[4 : 4+end])
				if filepath.IsAbs(executable) {
					if resolved, resolveErr := filepath.EvalSymlinks(executable); resolveErr == nil {
						executable = resolved
					}
					return executable, nil
				}
			}
		}
		if err == nil {
			err = errors.New("process executable unavailable")
		}
		return "", err
	}
	return C.GoString((*C.char)(unsafe.Pointer(&path[0]))), nil
}
