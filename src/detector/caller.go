package detector

import (
	"fmt"
	"os"
	"strings"
)

// findCallerByHidraw scans /proc/*/fd/ to find which process (other than ourselves)
// has the given hidraw device path open, then returns its process chain.
// Returns "" if the caller cannot be determined.
func findCallerByHidraw(devicePath string) string {
	selfPID := fmt.Sprintf("%d", os.Getpid())
	comms, ppids := buildProcessMaps()

	for pid := range comms {
		if pid == selfPID {
			continue
		}
		fds, err := os.ReadDir("/proc/" + pid + "/fd")
		if err != nil {
			continue
		}
		for _, fd := range fds {
			target, err := os.Readlink("/proc/" + pid + "/fd/" + fd.Name())
			if err != nil {
				continue
			}
			if target == devicePath {
				return buildProcessChain(pid, comms, ppids)
			}
		}
	}
	return ""
}

// findCallerForGPG heuristically identifies which process triggered a GPG operation
// by scanning for known GPG client programs (git, ssh, pass, gpg, …) and returning
// their process chain walking up the parent tree.
//
// Example output: "ssh → git → github-desktop"
func findCallerForGPG() string {
	comms, ppids := buildProcessMaps()
	selfPID := fmt.Sprintf("%d", os.Getpid())

	// Programs that use GPG/SSH keys — used as starting points for the chain walk.
	gpgClients := map[string]bool{
		"pass": true, "gopass": true,
		"git": true, "ssh": true, "scp": true,
		"gpg": true, "gpg2": true,
		"age": true, "rage": true,
	}

	for pid, comm := range comms {
		if !gpgClients[comm] || pid == selfPID {
			continue
		}
		return buildProcessChain(pid, comms, ppids)
	}
	return ""
}

// buildProcessMaps reads /proc to build two maps: pid→comm and pid→ppid.
func buildProcessMaps() (comms map[string]string, ppids map[string]string) {
	comms = make(map[string]string)
	ppids = make(map[string]string)

	procs, err := os.ReadDir("/proc")
	if err != nil {
		return
	}

	for _, p := range procs {
		if !p.IsDir() || !isPIDNumeric(p.Name()) {
			continue
		}
		pid := p.Name()

		data, err := os.ReadFile("/proc/" + pid + "/comm")
		if err != nil {
			continue
		}
		comms[pid] = strings.TrimSpace(string(data))

		// Parse ppid from /proc/PID/stat: "pid (comm) state ppid ..."
		// comm may contain spaces/parens, so find the last ')' to skip it.
		stat, err := os.ReadFile("/proc/" + pid + "/stat")
		if err != nil {
			continue
		}
		closeIdx := strings.LastIndex(string(stat), ")")
		if closeIdx < 0 {
			continue
		}
		fields := strings.Fields(string(stat)[closeIdx+1:])
		if len(fields) >= 2 {
			ppids[pid] = fields[1] // fields[0]=state, fields[1]=ppid
		}
	}
	return
}

// buildProcessChain walks up the process tree from pid and returns a " → " separated
// chain of process names up to 6 levels, stopping at PID 1 or unresolvable parents.
// Example: "ssh → git → github-desktop"
func buildProcessChain(pid string, comms, ppids map[string]string) string {
	var chain []string
	seen := make(map[string]bool)
	current := pid

	for i := 0; i < 6; i++ {
		comm, ok := comms[current]
		if !ok || seen[current] {
			break
		}
		seen[current] = true
		chain = append(chain, comm)

		parent := ppids[current]
		if parent == "" || parent == "0" || parent == "1" {
			break
		}
		current = parent
	}

	return strings.Join(chain, " → ")
}

func isPIDNumeric(s string) bool {
	if len(s) == 0 {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}
