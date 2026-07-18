// Package autoupdate keeps a device's remote-agent binary in sync with the
// release published on the relay.
//
// 机制(2026-07-03 起):发布方用 deploy/publish-release.sh 把
// assets/release/{manifest.json,remote-agent-<platform>,update.sh} 发到 relay
// 的 remotecoding static_dir;设备侧每 5 分钟(api.autoUpdateLoop)执行一次
// Apply:拉 manifest → 与 healthz 汇报的运行版本对比 → 一致则忽略;不一致则
// 下载 update.sh + 对应平台二进制,sha256 校验后执行脚本(原子替换二进制并
// 经 supervisor 重启),再等 healthz 收敛到 manifest 版本。设备上不再 git
// pull、不再本地编译。
package autoupdate

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/psyche08/remote-agent/internal/relaycert"
)

const (
	DefaultMinInterval = 30 * time.Minute
	DefaultService     = "remotecoding"
)

// cst — 项目约定所有构建/更新时间戳统一东八区。
var cst = time.FixedZone("UTC+8", 8*60*60)

// Manifest is assets/release/manifest.json on the relay — the single source
// of truth for what every device should be running.
type Manifest struct {
	Commit       string              `json:"commit"`
	BuiltAt      string              `json:"built_at"`
	Binaries     map[string]Artifact `json:"binaries"`
	UpdateScript Artifact            `json:"update_script"`
	Assets       []Artifact          `json:"assets,omitempty"`
}

// Artifact is one downloadable file, path relative to assets/release/.
type Artifact struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

type Options struct {
	RelayURL string // e.g. https://relay.example.com:8443
	Service  string // relay service name owning the release static dir
	CertDir  string
	CertFile string
	KeyFile  string
	UserID   string

	DeviceID   string
	TargetPath string // running binary path the update script replaces
	StagingDir string // downloads land here; default dir(StatePath)/update-staging
	StatePath  string

	MinInterval   time.Duration
	HealthUDS     string
	HealthURL     string
	HealthTimeout time.Duration
	Reason        string
	LogWriter     io.Writer
	Runner        Runner
	HealthCheck   func(context.Context, Options) (string, error)
	// Fetch returns the artifact at relPath under /s/<Service>/assets/release/.
	// nil means mTLS HTTP against RelayURL (cert via relaycert discovery).
	Fetch    func(ctx context.Context, opts Options, relPath string) ([]byte, error)
	Platform string // e.g. darwin-arm64; defaults to runtime.GOOS-GOARCH
	Now      func() time.Time
}

type State struct {
	LastSuccessAt       string `json:"last_success_at,omitempty"`
	LastAttemptAt       string `json:"last_attempt_at,omitempty"`
	LastResult          string `json:"last_result,omitempty"`
	LastReason          string `json:"last_reason,omitempty"`
	LastError           string `json:"last_error,omitempty"`
	LastFrom            string `json:"last_from,omitempty"`
	LastTo              string `json:"last_to,omitempty"`
	LastUnchangedTarget string `json:"last_unchanged_target,omitempty"`
}

type Result struct {
	Status  string `json:"status"`
	Reason  string `json:"reason,omitempty"`
	From    string `json:"from,omitempty"`
	To      string `json:"to,omitempty"`
	Message string `json:"message,omitempty"`
}

type Runner interface {
	Run(ctx context.Context, dir string, name string, args ...string) (string, error)
}

type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, dir string, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	s := strings.TrimSpace(string(out))
	if err != nil {
		if s == "" {
			s = err.Error()
		}
		return s, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, s)
	}
	return s, nil
}

func Apply(ctx context.Context, opts Options) (Result, error) {
	opts.applyDefaults()
	if opts.Fetch == nil && strings.TrimSpace(opts.RelayURL) == "" {
		return Result{}, errors.New("relay URL required")
	}
	state, _ := LoadState(opts.StatePath)
	now := opts.now()
	result := Result{Status: "unknown", Reason: opts.Reason}

	manifest, err := fetchManifest(ctx, opts)
	if err != nil {
		return saveError(opts, state, now, result, err)
	}
	result.To = manifest.Commit

	observed, _ := currentVersion(ctx, opts)
	result.From = observed

	if versionMatches(observed, manifest.Commit) {
		result.Status = "no_update"
		result.Message = "running version " + observed + " matches manifest"
		state.LastUnchangedTarget = ""
		state.recordAttempt(now, result, "")
		_ = SaveState(opts.StatePath, state)
		return result, nil
	}

	// Cooldown protects against repeatedly installing the same target, but it
	// must never delay a newly published manifest. Fetch and compare first, then
	// apply the guard only when the last successful target is still current.
	if last, ok := parseTime(state.LastSuccessAt); ok && now.Sub(last) < opts.MinInterval && state.LastTo == manifest.Commit {
		result.Status = "refused"
		result.Reason = "cooldown"
		result.Message = "last successful update to this target was less than 30 minutes ago"
		state.recordAttempt(now, result, "")
		_ = SaveState(opts.StatePath, state)
		return result, nil
	}

	if state.LastUnchangedTarget != "" && state.LastUnchangedTarget == manifest.Commit {
		result.Status = "refused"
		result.Reason = "unchanged_version"
		result.Message = "previous update to manifest " + manifest.Commit + " did not change the running version"
		state.recordAttempt(now, result, "")
		_ = SaveState(opts.StatePath, state)
		return result, nil
	}

	if opts.TargetPath == "" {
		return saveError(opts, state, now, result, errors.New("target path required to install an update"))
	}
	binary, ok := manifest.Binaries[opts.Platform]
	if !ok || binary.Path == "" {
		return saveError(opts, state, now, result, fmt.Errorf("manifest %s has no binary for platform %s", manifest.Commit, opts.Platform))
	}

	scriptPath, err := stageArtifact(ctx, opts, manifest.UpdateScript)
	if err != nil {
		return saveError(opts, state, now, result, err)
	}
	binaryPath, err := stageArtifact(ctx, opts, binary)
	if err != nil {
		return saveError(opts, state, now, result, err)
	}
	for _, asset := range manifest.Assets {
		if _, err := stageArtifact(ctx, opts, asset); err != nil {
			return saveError(opts, state, now, result, err)
		}
	}

	logf(opts, "updating %s -> %s (built_at=%s)", firstNonEmpty(observed, "unknown"), manifest.Commit, manifest.BuiltAt)
	if out, err := opts.Runner.Run(ctx, opts.StagingDir, scriptPath, binaryPath, opts.TargetPath, opts.DeviceID); err != nil {
		return saveError(opts, state, now, result, fmt.Errorf("update script failed: %w", err))
	} else if strings.TrimSpace(out) != "" {
		logf(opts, "%s", out)
	}

	observed, err = waitVersion(ctx, opts, manifest.Commit)
	if err != nil {
		result.Status = "refused"
		result.Reason = "unchanged_version"
		result.Message = err.Error()
		state.LastUnchangedTarget = manifest.Commit
		state.recordAttempt(now, result, "")
		_ = SaveState(opts.StatePath, state)
		return result, nil
	}

	result.Status = "updated"
	result.Message = "running version is " + observed
	state.LastSuccessAt = now.In(cst).Format(time.RFC3339Nano)
	state.LastUnchangedTarget = ""
	state.recordAttempt(now, result, "")
	_ = SaveState(opts.StatePath, state)
	return result, nil
}

func fetchManifest(ctx context.Context, opts Options) (Manifest, error) {
	b, err := fetchArtifact(ctx, opts, "manifest.json")
	if err != nil {
		return Manifest{}, fmt.Errorf("fetch manifest: %w", err)
	}
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return Manifest{}, fmt.Errorf("parse manifest: %w", err)
	}
	if strings.TrimSpace(m.Commit) == "" {
		return Manifest{}, errors.New("manifest missing commit")
	}
	if m.UpdateScript.Path == "" {
		return Manifest{}, errors.New("manifest missing update_script")
	}
	return m, nil
}

// stageArtifact downloads one artifact into StagingDir, verifying its sha256
// against the manifest before anything gets executed or installed.
func stageArtifact(ctx context.Context, opts Options, a Artifact) (string, error) {
	if a.Path == "" {
		return "", errors.New("artifact path empty")
	}
	b, err := fetchArtifact(ctx, opts, a.Path)
	if err != nil {
		return "", fmt.Errorf("fetch %s: %w", a.Path, err)
	}
	sum := sha256.Sum256(b)
	got := hex.EncodeToString(sum[:])
	if !strings.EqualFold(got, strings.TrimSpace(a.SHA256)) {
		return "", fmt.Errorf("%s sha256 mismatch: manifest %s, downloaded %s", a.Path, a.SHA256, got)
	}
	if err := os.MkdirAll(opts.StagingDir, 0o700); err != nil {
		return "", err
	}
	dst := filepath.Join(opts.StagingDir, filepath.Base(a.Path))
	tmp := dst + ".tmp"
	if err := os.WriteFile(tmp, b, 0o755); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, dst); err != nil {
		return "", err
	}
	return dst, nil
}

func fetchArtifact(ctx context.Context, opts Options, relPath string) ([]byte, error) {
	if opts.Fetch != nil {
		return opts.Fetch(ctx, opts, relPath)
	}
	return httpFetch(ctx, opts, relPath)
}

func httpFetch(ctx context.Context, opts Options, relPath string) ([]byte, error) {
	cert, err := relaycert.Discover(opts.CertDir, opts.CertFile, opts.KeyFile, opts.UserID, opts.DeviceID)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: 5 * time.Minute, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12},
	}}
	url := strings.TrimRight(opts.RelayURL, "/") + "/s/" + opts.Service + "/assets/release/" + relPath
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("GET %s: HTTP %d %s", url, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return io.ReadAll(io.LimitReader(resp.Body, 200<<20))
}

func currentVersion(ctx context.Context, opts Options) (string, error) {
	if opts.HealthCheck != nil {
		return opts.HealthCheck(ctx, opts)
	}
	return fetchVersion(ctx, opts)
}

func versionMatches(v string, want string) bool {
	v = strings.TrimSpace(v)
	want = strings.TrimSpace(want)
	return v != "" && want != "" && (v == want || strings.HasPrefix(want, v) || strings.HasPrefix(v, want))
}

func firstNonEmpty(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}

func LoadState(path string) (State, error) {
	var st State
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return st, nil
		}
		return st, err
	}
	if len(strings.TrimSpace(string(b))) == 0 {
		return st, nil
	}
	return st, json.Unmarshal(b, &st)
}

func SaveState(path string, st State) error {
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (o *Options) applyDefaults() {
	if o.Service == "" {
		o.Service = DefaultService
	}
	if o.Platform == "" {
		o.Platform = runtime.GOOS + "-" + runtime.GOARCH
	}
	if o.CertDir == "" {
		o.CertDir = "/opt/private-tunnel/certs"
		if st, err := os.Stat(o.CertDir); err != nil || !st.IsDir() {
			o.CertDir = "/opt/private-tunnel/cert"
		}
	}
	if o.StagingDir == "" {
		if o.StatePath != "" {
			o.StagingDir = filepath.Join(filepath.Dir(o.StatePath), "update-staging")
		} else {
			o.StagingDir = filepath.Join(os.TempDir(), "remote-agent-update-staging")
		}
	}
	if o.MinInterval <= 0 {
		o.MinInterval = DefaultMinInterval
	}
	if o.HealthTimeout <= 0 {
		o.HealthTimeout = 45 * time.Second
	}
	if o.HealthURL == "" {
		o.HealthURL = "http://127.0.0.1:8765/healthz"
	}
	if o.Runner == nil {
		o.Runner = ExecRunner{}
	}
	if o.Now == nil {
		o.Now = time.Now
	}
}

func (o Options) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

func (s *State) recordAttempt(now time.Time, r Result, errText string) {
	s.LastAttemptAt = now.In(cst).Format(time.RFC3339Nano)
	s.LastResult = r.Status
	s.LastReason = r.Reason
	s.LastError = errText
	// Some attempts can fail before observing the running version or manifest.
	// Preserve non-empty values so the UI can still answer the only version
	// question that matters: is this agent on the latest manifest target?
	if r.From != "" {
		s.LastFrom = r.From
	}
	if r.To != "" {
		s.LastTo = r.To
	}
}

func saveError(opts Options, st State, now time.Time, r Result, err error) (Result, error) {
	r.Status = "error"
	r.Message = err.Error()
	st.recordAttempt(now, r, err.Error())
	_ = SaveState(opts.StatePath, st)
	return r, err
}

func waitVersion(ctx context.Context, opts Options, want string) (string, error) {
	deadline := time.Now().Add(opts.HealthTimeout)
	var last string
	check := fetchVersion
	if opts.HealthCheck != nil {
		check = opts.HealthCheck
	}
	for {
		if time.Now().After(deadline) {
			if last == "" {
				last = "unavailable"
			}
			return last, fmt.Errorf("running version stayed at %s; want %s", last, want)
		}
		v, err := check(ctx, opts)
		if err == nil && v != "" {
			last = v
			if versionMatches(v, want) {
				return v, nil
			}
		}
		select {
		case <-ctx.Done():
			return last, ctx.Err()
		case <-time.After(minDuration(2*time.Second, time.Until(deadline))):
		}
	}
}

func minDuration(a, b time.Duration) time.Duration {
	if b <= 0 {
		return time.Millisecond
	}
	if a < b {
		return a
	}
	return b
}

func fetchVersion(ctx context.Context, opts Options) (string, error) {
	client := &http.Client{Timeout: 3 * time.Second}
	url := opts.HealthURL
	if opts.HealthUDS != "" {
		uds := opts.HealthUDS
		client.Transport = &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", uds)
			},
		}
		url = "http://unix/healthz"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("health status %d", resp.StatusCode)
	}
	var body struct {
		Version map[string]any `json:"version"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&body); err != nil {
		return "", err
	}
	if body.Version == nil {
		return "", errors.New("health missing version")
	}
	for _, key := range []string{"commit", "version"} {
		if v, ok := body.Version[key].(string); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v), nil
		}
	}
	return "", errors.New("health missing commit")
}

func parseTime(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	return t, err == nil
}

func logf(opts Options, format string, args ...any) {
	if opts.LogWriter == nil {
		opts.LogWriter = os.Stderr
	}
	fmt.Fprintf(opts.LogWriter, format+"\n", args...)
}
