package gittrailer

import (
	"bytes"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

func processIdentity(pid uint32) (string, error) {
	data, _, err := readRegular(fmt.Sprintf("/proc/%d/stat", pid), 4096)
	if err != nil {
		return "", err
	}
	_, rest, found := bytes.CutLast(data, []byte(")"))
	if !found {
		return "", errors.New("invalid process metadata")
	}
	// After the parenthesized command, field 3 starts at index 0;
	// field 22 is the process's monotonic start time.
	fields := strings.Fields(string(rest))
	if len(fields) <= 19 {
		return "", errors.New("incomplete process metadata")
	}
	if _, err := strconv.ParseUint(fields[19], 10, 64); err != nil {
		return "", err
	}
	boot, _, err := readRegular("/proc/sys/kernel/random/boot_id", 64)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s/%d/%s", strings.TrimSpace(string(boot)), pid, fields[19]), nil
}
