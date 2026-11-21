package main

import (
	"io"
	"os"
	"strconv"
)

func terminalColumns(out io.Writer) int {
	cols, _ := terminalSize(out)
	return cols
}

// positiveEnv is the variable as a positive integer, or 0.
func positiveEnv(name string) int {
	value, err := strconv.Atoi(os.Getenv(name))
	if err != nil || value <= 0 {
		return 0
	}
	return value
}
