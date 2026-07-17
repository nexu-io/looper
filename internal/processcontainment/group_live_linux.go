//go:build linux

package processcontainment

import (
	"bytes"
	"os"
	"strconv"
)

// groupHasNonZombieMember reports whether pgid still has a non-zombie member.
// ok is false when /proc cannot be scanned (caller should fall back).
//
// kill(-pgid, 0) returns success while only zombies remain in the group; those
// are not runnable and will be reaped by init after reparent, so they must not
// block confirmed-dead drain.
func groupHasNonZombieMember(pgid int) (hasLive bool, ok bool) {
	if pgid <= 0 {
		return false, true
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return false, false
	}
	for _, ent := range entries {
		if !ent.IsDir() {
			continue
		}
		name := ent.Name()
		if !isAllDigits(name) {
			continue
		}
		data, err := os.ReadFile("/proc/" + name + "/stat")
		if err != nil {
			continue
		}
		state, procPgrp, parsed := parseProcStatStatePgrp(data)
		if !parsed || procPgrp != pgid {
			continue
		}
		// Z = zombie; X/x = dead (kernel). Neither is runnable.
		if state == 'Z' || state == 'X' || state == 'x' {
			continue
		}
		return true, true
	}
	return false, true
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// parseProcStatStatePgrp reads state and pgrp from /proc/pid/stat.
// Format: pid (comm) state ppid pgrp ...
// comm may contain spaces and parentheses; state follows the final ") ".
func parseProcStatStatePgrp(stat []byte) (state byte, pgrp int, ok bool) {
	i := bytes.LastIndexByte(stat, ')')
	if i < 0 || i+2 >= len(stat) {
		return 0, 0, false
	}
	rest := stat[i+2:]
	fields := bytes.Fields(rest)
	// fields[0]=state, [1]=ppid, [2]=pgrp
	if len(fields) < 3 || len(fields[0]) == 0 {
		return 0, 0, false
	}
	state = fields[0][0]
	pgrp64, err := strconv.ParseInt(string(fields[2]), 10, 0)
	if err != nil {
		return 0, 0, false
	}
	return state, int(pgrp64), true
}
