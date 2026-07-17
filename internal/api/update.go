package api

import (
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/psyche08/remote-agent/internal/autoupdate"
)

func (s *Server) update(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, s.updateSnapshot())
	case http.MethodPost:
		started, err := s.startAutoUpdate("web")
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		status := http.StatusAccepted
		if !started {
			status = http.StatusOK
		}
		resp := s.updateSnapshot()
		resp["started"] = started
		writeJSON(w, status, resp)
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (s *Server) autoUpdateLoop() {
	if os.Getenv("RC_AUTO_UPDATE") == "0" {
		return
	}
	timer := time.NewTimer(45 * time.Second)
	defer timer.Stop()
	for {
		select {
		case <-s.pushStop:
			return
		case <-timer.C:
			_, _ = s.startAutoUpdate("auto")
			timer.Reset(5 * time.Minute)
		}
	}
}

func (s *Server) updateSnapshot() map[string]any {
	s.updateMu.Lock()
	running := s.updateRunning
	s.updateMu.Unlock()
	st, _ := autoupdate.LoadState(s.autoUpdateStatePath())
	return map[string]any{
		"ok":      true,
		"running": running,
		"state":   st,
	}
}

func (s *Server) startAutoUpdate(reason string) (bool, error) {
	if os.Getenv("RC_AUTO_UPDATE") == "0" {
		return false, nil
	}
	relayURL := strings.TrimSpace(os.Getenv("RC_UPDATE_RELAY_URL"))
	if relayURL == "" {
		return false, nil
	}
	s.updateMu.Lock()
	if s.updateRunning {
		s.updateMu.Unlock()
		return false, nil
	}
	s.updateRunning = true
	s.updateMu.Unlock()

	exe, err := os.Executable()
	if err != nil {
		s.setUpdateRunning(false)
		return false, err
	}
	args := []string{
		"update", "apply",
		"--device", s.cfg.DeviceID,
		"--target", exe,
		"--state", s.autoUpdateStatePath(),
		"--staging", filepath.Join(s.store.DataDir(), "update-staging"),
		"--min-interval", autoupdate.DefaultMinInterval.String(),
		"--health-url", "http://" + s.cfg.Host + ":" + strconv.Itoa(s.cfg.Port) + "/healthz",
		"--reason", reason,
		"--relay-url", relayURL,
	}
	if certDir := os.Getenv("RC_UPDATE_CERT_DIR"); certDir != "" {
		args = append(args, "--cert-dir", certDir)
	}
	if s.cfg.UDS != "" {
		args = append(args, "--health-uds", s.cfg.UDS)
	}
	cmd := exec.Command(exe, args...)
	workerLog, err := attachAutoUpdateWorkerLog(cmd, s.store.DataDir())
	if err != nil {
		s.setUpdateRunning(false)
		return false, err
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		_ = workerLog.Close()
		s.setUpdateRunning(false)
		return false, err
	}
	go func() {
		_ = cmd.Wait()
		_ = workerLog.Close()
		s.setUpdateRunning(false)
	}()
	return true, nil
}

// attachAutoUpdateWorkerLog must not point the worker at os.Stdout/os.Stderr.
// Under private-services those descriptors are the supervised process's log
// pipe. The detached updater survives the parent restart; inheriting that pipe
// keeps it open, so cmd.Wait in the supervisor cannot observe EOF and will not
// launch the replacement service until the updater's health timeout expires.
func attachAutoUpdateWorkerLog(cmd *exec.Cmd, dataDir string) (*os.File, error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dataDir, "auto-update-worker.log"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, err
	}
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return nil, err
	}
	cmd.Dir = dataDir
	cmd.Stdout = f
	cmd.Stderr = f
	return f, nil
}

func (s *Server) setUpdateRunning(v bool) {
	s.updateMu.Lock()
	s.updateRunning = v
	s.updateMu.Unlock()
}

func (s *Server) autoUpdateStatePath() string {
	return filepath.Join(s.store.DataDir(), "auto-update.json")
}
