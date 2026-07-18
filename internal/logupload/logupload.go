package logupload

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/psyche08/remote-agent/internal/relaycert"
)

type Options struct {
	RelayURL    string
	Namespace   string
	UserID      string
	DeviceID    string
	CertDir     string
	CertFile    string
	KeyFile     string
	StatePath   string
	Sources     []string
	MaxChunk    int64
	Interval    time.Duration
	Once        bool
	LogWriter   io.Writer
	HTTPClient  *http.Client
	DiscoverTLS func(Options) (tls.Certificate, error)
}

type State struct {
	Offsets map[string]int64 `json:"offsets"`
}

type Result struct {
	UploadedFiles int
	UploadedBytes int64
}

func Run(ctx context.Context, opts Options) error {
	applyDefaults(&opts)
	if opts.Once {
		_, err := UploadOnce(ctx, opts)
		return err
	}
	ticker := time.NewTicker(opts.Interval)
	defer ticker.Stop()
	for {
		res, err := UploadOnce(ctx, opts)
		if err != nil {
			logf(opts, "log upload failed: %v", err)
		} else if res.UploadedBytes > 0 {
			logf(opts, "log upload: files=%d bytes=%d", res.UploadedFiles, res.UploadedBytes)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func UploadOnce(ctx context.Context, opts Options) (Result, error) {
	applyDefaults(&opts)
	if strings.TrimSpace(opts.RelayURL) == "" {
		return Result{}, errors.New("relay URL required")
	}
	if opts.DeviceID == "" {
		return Result{}, errors.New("device id required")
	}
	if len(opts.Sources) == 0 {
		return Result{}, errors.New("at least one source log required")
	}
	st, err := loadState(opts.StatePath)
	if err != nil {
		return Result{}, err
	}
	client := opts.HTTPClient
	if client == nil {
		certFn := opts.DiscoverTLS
		if certFn == nil {
			certFn = discoverTLS
		}
		cert, err := certFn(opts)
		if err != nil {
			return Result{}, err
		}
		client = &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{
			TLSClientConfig: &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12},
		}}
	}
	var out Result
	for _, raw := range opts.Sources {
		src := expandUser(raw)
		n, err := uploadSource(ctx, client, opts, st, src)
		if err != nil {
			return out, err
		}
		if n > 0 {
			out.UploadedFiles++
			out.UploadedBytes += n
		}
	}
	if out.UploadedBytes > 0 {
		if err := saveState(opts.StatePath, st); err != nil {
			return out, err
		}
	}
	return out, nil
}

func uploadSource(ctx context.Context, client *http.Client, opts Options, st State, src string) (int64, error) {
	info, err := os.Stat(src)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	if info.IsDir() {
		return 0, nil
	}
	off := st.Offsets[src]
	if off < 0 || off > info.Size() {
		off = 0
	}
	if off == info.Size() {
		return 0, nil
	}
	f, err := os.Open(src)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return 0, err
	}
	buf, err := io.ReadAll(io.LimitReader(f, opts.MaxChunk))
	if err != nil {
		return 0, err
	}
	if len(buf) == 0 {
		return 0, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, uploadURL(opts), bytes.NewReader(buf))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "text/plain; charset=utf-8")
	req.Header.Set("X-Remote-Agent-Log-Source", filepath.Base(src))
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return 0, fmt.Errorf("relay log upload failed: HTTP %d %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	st.Offsets[src] = off + int64(len(buf))
	return int64(len(buf)), nil
}

func applyDefaults(opts *Options) {
	if opts.Namespace == "" {
		opts.Namespace = "remocoding"
	}
	if opts.CertDir == "" {
		opts.CertDir = "/opt/private-tunnel/certs"
	}
	if opts.StatePath == "" {
		opts.StatePath = "/opt/private-tunnel/state/remote-agent/data/log-upload-state.json"
	}
	if opts.MaxChunk <= 0 {
		opts.MaxChunk = 1024 * 1024
	}
	if opts.Interval <= 0 {
		opts.Interval = time.Minute
	}
	if len(opts.Sources) == 0 {
		opts.Sources = []string{"~/Library/Logs/private-services/remote-agent.log"}
	}
}

func uploadURL(opts Options) string {
	return strings.TrimRight(opts.RelayURL, "/") + "/_pt/logs/" + opts.Namespace + "/" + opts.DeviceID
}

func loadState(path string) (State, error) {
	st := State{Offsets: map[string]int64{}}
	b, err := os.ReadFile(expandUser(path))
	if err != nil {
		if os.IsNotExist(err) {
			return st, nil
		}
		return st, err
	}
	if err := json.Unmarshal(b, &st); err != nil {
		return st, err
	}
	if st.Offsets == nil {
		st.Offsets = map[string]int64{}
	}
	return st, nil
}

func saveState(path string, st State) error {
	path = expandUser(path)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func discoverTLS(opts Options) (tls.Certificate, error) {
	return relaycert.Discover(opts.CertDir, opts.CertFile, opts.KeyFile, opts.UserID, opts.DeviceID)
}

func expandUser(path string) string {
	if strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, path[2:])
		}
	}
	return path
}

func logf(opts Options, format string, args ...any) {
	if opts.LogWriter == nil {
		opts.LogWriter = os.Stderr
	}
	fmt.Fprintf(opts.LogWriter, format+"\n", args...)
}
