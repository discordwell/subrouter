//go:build !windows

package main

import (
	"cmp"
	"io"
	"os"
	"syscall"
	"unsafe"
)

type terminalWinsize struct {
	Row    uint16
	Col    uint16
	Xpixel uint16
	Ypixel uint16
}

// terminalSize is $COLUMNS and $LINES, each else that axis of out's terminal,
// else 0 for unknown.
func terminalSize(out io.Writer) (cols, lines int) {
	cols, lines = positiveEnv("COLUMNS"), positiveEnv("LINES")
	if cols > 0 && lines > 0 {
		return cols, lines
	}
	file, ok := out.(*os.File)
	if !ok {
		return cols, lines
	}
	var size terminalWinsize
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, file.Fd(), uintptr(syscall.TIOCGWINSZ), uintptr(unsafe.Pointer(&size)))
	if errno != 0 {
		return cols, lines
	}
	return cmp.Or(cols, int(size.Col)), cmp.Or(lines, int(size.Row))
}
