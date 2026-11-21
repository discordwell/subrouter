//go:build windows

package main

import "io"

// terminalSize reads only $COLUMNS and $LINES here. 0 is unknown.
func terminalSize(_ io.Writer) (cols, lines int) {
	return positiveEnv("COLUMNS"), positiveEnv("LINES")
}
