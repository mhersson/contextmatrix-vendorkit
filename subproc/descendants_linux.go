//go:build linux

package subproc

import (
	"bytes"
	"os"
	"strconv"
	"syscall"
)

// killDescendants SIGKILLs the leader's descendants and the process groups
// they lead: a setsid child (Claude Code's Bash tool) has left the leader's
// group, so the group kill misses it. A child spawned after the snapshot
// escapes. ESRCH and EPERM are expected and ignored.
func killDescendants(leader int) {
	procs := descendants(leader)

	inTree := map[int]bool{leader: true}
	for _, d := range procs {
		inTree[d.pid] = true
	}

	self := syscall.Getpgrp()

	for _, d := range procs {
		// Only a group a descendant leads: one it joined could hold the caller.
		if d.pgrp != leader && d.pgrp != self && inTree[d.pgrp] {
			_ = syscall.Kill(-d.pgrp, syscall.SIGKILL)
		}

		_ = syscall.Kill(d.pid, syscall.SIGKILL)
	}
}

type procInfo struct{ pid, pgrp int }

// descendants walks /proc ppid links from leader.
func descendants(leader int) []procInfo {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}

	children := map[int][]procInfo{}

	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}

		if ppid, pgrp, ok := readStat(pid); ok {
			children[ppid] = append(children[ppid], procInfo{pid: pid, pgrp: pgrp})
		}
	}

	var out []procInfo

	queue := []int{leader}
	for len(queue) > 0 {
		pid := queue[0]
		queue = queue[1:]

		for _, c := range children[pid] {
			out = append(out, c)
			queue = append(queue, c.pid)
		}
	}

	return out
}

// readStat returns the ppid and pgrp of /proc/<pid>/stat; the comm field may
// hold spaces and parentheses, so fields count from the last ')'.
func readStat(pid int) (ppid, pgrp int, ok bool) {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, 0, false
	}

	i := bytes.LastIndexByte(data, ')')
	if i < 0 {
		return 0, 0, false
	}

	fields := bytes.Fields(data[i+1:])
	if len(fields) < 3 {
		return 0, 0, false
	}

	ppid, err1 := strconv.Atoi(string(fields[1]))
	pgrp, err2 := strconv.Atoi(string(fields[2]))

	return ppid, pgrp, err1 == nil && err2 == nil
}
