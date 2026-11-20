package main

import (
	"errors"
	"os"
	"strconv"
	"strings"
)

// descriptorPath is the path the kernel currently has for an open descriptor.
// A deleted file has no path to rotate beside.
func descriptorPath(fd int) (string, error) {
	path, err := os.Readlink("/proc/self/fd/" + strconv.Itoa(fd))
	if err != nil {
		return "", err
	}
	if strings.HasSuffix(path, " (deleted)") {
		return "", errors.New("the file was deleted")
	}
	return path, nil
}
