package provider

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// claudeDesktopProcessManager performs the one-way handoff from Claude
// Desktop's internal Code CLI to the standalone stream-json CLI. Implementors
// must only signal processes proven to own the requested Desktop session.
type claudeDesktopProcessManager interface {
	StopSession(aliases []string, grace time.Duration) (bool, error)
}

type claudeProcess struct {
	PID     int
	PPID    int
	Command string
}

type systemClaudeDesktopProcessManager struct {
	listProcesses func() ([]claudeProcess, error)
	openFiles     func(int) ([]string, error)
	signal        func(int, syscall.Signal) error
	alive         func(int) bool
	sleep         func(time.Duration)
}

func newSystemClaudeDesktopProcessManager() *systemClaudeDesktopProcessManager {
	return &systemClaudeDesktopProcessManager{
		listProcesses: listClaudeProcesses,
		openFiles:     claudeProcessOpenFiles,
		signal: func(pid int, sig syscall.Signal) error {
			return syscall.Kill(pid, sig)
		},
		alive: func(pid int) bool {
			err := syscall.Kill(pid, 0)
			return err == nil || errors.Is(err, syscall.EPERM)
		},
		sleep: time.Sleep,
	}
}

func listClaudeProcesses() ([]claudeProcess, error) {
	out, err := exec.Command("ps", "-ww", "-axo", "pid=,ppid=,command=").Output()
	if err != nil {
		return nil, fmt.Errorf("inspect Claude Desktop processes: %w", err)
	}
	return parseClaudeProcesses(string(out)), nil
}

func parseClaudeProcesses(out string) []claudeProcess {
	rows := []claudeProcess{}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		pid, err1 := strconv.Atoi(fields[0])
		ppid, err2 := strconv.Atoi(fields[1])
		if err1 != nil || err2 != nil || pid <= 1 {
			continue
		}
		rows = append(rows, claudeProcess{PID: pid, PPID: ppid, Command: strings.Join(fields[2:], " ")})
	}
	return rows
}

func claudeProcessOpenFiles(pid int) ([]string, error) {
	out, err := exec.Command("lsof", "-Fn", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		// lsof exits non-zero when the process disappears during inspection. That
		// is already a safe outcome for ownership handoff.
		if !processAlive(pid) {
			return nil, nil
		}
		return nil, fmt.Errorf("inspect Claude process %d open files: %w", pid, err)
	}
	files := []string{}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "n") {
			files = append(files, strings.TrimPrefix(line, "n"))
		}
	}
	return files, nil
}

func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func isClaudeDesktopInternalProcess(command string) bool {
	return strings.Contains(command, "/Library/Application Support/Claude/claude-code/") &&
		(strings.Contains(command, "/claude.app/Contents/MacOS/claude") ||
			strings.Contains(command, "/claude.remote-agent-real.app/Contents/MacOS/claude") ||
			strings.Contains(command, "/claude.remote-coding-real.app/Contents/MacOS/claude"))
}

func commandHasSessionAlias(command string, aliases map[string]bool) bool {
	for _, token := range strings.Fields(command) {
		token = strings.Trim(token, "'\"")
		if aliases[token] || (strings.HasPrefix(token, "--resume=") && aliases[strings.TrimPrefix(token, "--resume=")]) ||
			(strings.HasPrefix(token, "--session-id=") && aliases[strings.TrimPrefix(token, "--session-id=")]) {
			return true
		}
	}
	return false
}

func filesHaveSessionTranscript(files []string, aliases map[string]bool) bool {
	for _, path := range files {
		if !strings.Contains(path, "/.claude/projects/") {
			continue
		}
		name := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
		if strings.HasSuffix(path, ".jsonl") && aliases[name] {
			return true
		}
	}
	return false
}

func (m *systemClaudeDesktopProcessManager) StopSession(aliasList []string, grace time.Duration) (bool, error) {
	aliases := map[string]bool{}
	for _, alias := range aliasList {
		if alias != "" {
			aliases[alias] = true
		}
	}
	if len(aliases) == 0 {
		return false, nil
	}
	processes, err := m.listProcesses()
	if err != nil {
		return false, err
	}
	byPID := map[int]claudeProcess{}
	children := map[int][]int{}
	for _, proc := range processes {
		if !isClaudeDesktopInternalProcess(proc.Command) {
			continue
		}
		byPID[proc.PID] = proc
		children[proc.PPID] = append(children[proc.PPID], proc.PID)
	}
	matched := map[int]bool{}
	for pid, proc := range byPID {
		owned := commandHasSessionAlias(proc.Command, aliases)
		if !owned {
			files, err := m.openFiles(pid)
			if err != nil {
				return false, err
			}
			owned = filesHaveSessionTranscript(files, aliases)
		}
		if owned {
			matched[pid] = true
		}
	}
	if len(matched) == 0 {
		return false, nil
	}

	// Desktop launches a small parent/child CLI family per Code session. Once
	// one member is proven to own the transcript, include only its internal-CLI
	// ancestors and descendants so the parent cannot respawn the child.
	owners := map[int]bool{}
	var addDescendants func(int)
	addDescendants = func(pid int) {
		if owners[pid] {
			return
		}
		owners[pid] = true
		for _, child := range children[pid] {
			addDescendants(child)
		}
	}
	for pid := range matched {
		root := pid
		for {
			parent, ok := byPID[byPID[root].PPID]
			if !ok {
				break
			}
			root = parent.PID
		}
		addDescendants(root)
	}
	pids := make([]int, 0, len(owners))
	for pid := range owners {
		if pid != os.Getpid() {
			pids = append(pids, pid)
		}
	}
	sort.Ints(pids)
	for _, pid := range pids {
		if err := m.signal(pid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
			return false, fmt.Errorf("stop Claude Desktop session process %d: %w", pid, err)
		}
	}
	if waitClaudeProcessesExit(pids, grace, m.alive, m.sleep) {
		return true, nil
	}
	for _, pid := range pids {
		if m.alive(pid) {
			if err := m.signal(pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
				return false, fmt.Errorf("force-stop Claude Desktop session process %d: %w", pid, err)
			}
		}
	}
	if waitClaudeProcessesExit(pids, time.Second, m.alive, m.sleep) {
		return true, nil
	}
	return false, errors.New("Claude Desktop session process did not exit; refusing competing CLI owner")
}

func waitClaudeProcessesExit(pids []int, timeout time.Duration, alive func(int) bool, sleep func(time.Duration)) bool {
	deadline := time.Now().Add(timeout)
	for {
		allExited := true
		for _, pid := range pids {
			if alive(pid) {
				allExited = false
				break
			}
		}
		if allExited {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		sleep(100 * time.Millisecond)
	}
}
