package harness

import (
	"errors"

	"golang.org/x/sys/unix"
)

func waitProviderExit(pid int) error {
	queue, err := unix.Kqueue()
	if err != nil {
		return err
	}
	defer unix.Close(queue)
	changes := []unix.Kevent_t{{Ident: uint64(pid), Filter: unix.EVFILT_PROC, Flags: unix.EV_ADD | unix.EV_ONESHOT, Fflags: unix.NOTE_EXIT}}
	var events [1]unix.Kevent_t
	for {
		_, err := unix.Kevent(queue, changes, events[:], nil)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return err
		}
		if events[0].Flags&unix.EV_ERROR != 0 {
			err = unix.Errno(events[0].Data)
			if errors.Is(err, unix.ESRCH) {
				return nil
			}
			return err
		}
		return nil
	}
}
