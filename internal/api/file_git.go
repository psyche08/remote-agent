package api

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func fileBody(path string) (map[string]any, int, string) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, http.StatusNotFound, "file not found"
	}
	if st.IsDir() {
		return nil, http.StatusBadRequest, "path is a directory"
	}
	body := map[string]any{
		"ok":        true,
		"path":      path,
		"name":      filepath.Base(path),
		"size":      st.Size(),
		"mtime":     float64(st.ModTime().UnixNano()) / 1e9,
		"kind":      "binary",
		"truncated": false,
	}
	ext := strings.ToLower(filepath.Ext(path))
	if mime := imageMime[ext]; mime != "" && st.Size() <= imageMaxBytes {
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, http.StatusInternalServerError, err.Error()
		}
		body["kind"] = "image"
		body["mime"] = mime
		body["content_b64"] = base64.StdEncoding.EncodeToString(b)
		return body, 0, ""
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, http.StatusInternalServerError, err.Error()
	}
	defer f.Close()
	buf := make([]byte, fileMaxBytes+1)
	n, _ := f.Read(buf)
	raw := buf[:n]
	truncated := len(raw) > fileMaxBytes
	if truncated {
		raw = raw[:fileMaxBytes]
	}
	scan := raw
	if len(scan) > 8192 {
		scan = scan[:8192]
	}
	if strings.Contains(string(scan), "\x00") {
		return body, 0, ""
	}
	text := string(raw)
	body["kind"] = "text"
	body["content"] = text
	body["truncated"] = truncated
	body["lines"] = strings.Count(text, "\n") + 1
	return body, 0, ""
}

func runGit(repo string, timeout time.Duration, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", repo}, args...)...)
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return "", errors.New(msg)
	}
	return string(out), nil
}

func parseIntDefault(s string, fallback int) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		return fallback
	}
	return n
}
