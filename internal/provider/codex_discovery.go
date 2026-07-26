package provider

import (
	"os"
	"strings"
	"time"
)

const (
	codexDiscoveryRefreshCoalesce = 500 * time.Millisecond
	codexThreadListTotalTimeout   = 4 * time.Second
)

var codexInteractiveSourceKinds = []string{
	"cli",
	"vscode",
	"exec",
	"appServer",
	"unknown",
}

type codexDiscoveryEntry struct {
	summary     codexRolloutSummary
	size        int64
	modTimeNano int64
}

type codexDiscoveryCatalog struct {
	entries     map[string]codexDiscoveryEntry
	refreshedAt time.Time
	scans       uint64
	filesParsed uint64
}

type codexDiscoverySnapshot struct {
	summaries   map[string]codexRolloutSummary
	hidden      map[string]bool
	scans       uint64
	filesParsed uint64
}

func codexThreadListFastParams() map[string]any {
	sourceKinds := make([]string, len(codexInteractiveSourceKinds))
	copy(sourceKinds, codexInteractiveSourceKinds)
	return map[string]any{
		"limit":          nativeSessionListLimit,
		"useStateDbOnly": true,
		"sortKey":        "recency_at",
		"sortDirection":  "desc",
		"sourceKinds":    sourceKinds,
	}
}

func (c *Codex) listCodexThreads(client codexAppClient) (any, error) {
	c.threadListMu.Lock()
	defer c.threadListMu.Unlock()

	deadline := time.Now().Add(codexThreadListTotalTimeout)
	list := func(params map[string]any) (any, error) {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, CodexAppServerError{"timeout waiting for thread/list capability fallback"}
		}
		return client.ThreadList(remaining, params)
	}
	if c.threadListLegacy {
		return list(nil)
	}
	result, err := list(codexThreadListFastParams())
	if err == nil || !codexThreadListParamsUnsupported(err) {
		return result, err
	}

	// An older app-server rejects fields it does not know. Remember that
	// capability result for this provider process so every refresh does not
	// pay for the same failed negotiation.
	c.threadListLegacy = true
	return list(nil)
}

func codexThreadListParamsUnsupported(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, `"code":-32602`) ||
		strings.Contains(msg, "invalid params") ||
		strings.Contains(msg, "invalid parameter") ||
		strings.Contains(msg, "unknown field") ||
		strings.Contains(msg, "unknown variant")
}

func (c *Codex) localCodexSessions() []map[string]any {
	snapshot := c.codexDiscoverySnapshot(true)
	return codexSessionsFromSummaries(
		stringExtra(c.cfg.Extra, "codex_session_index", ""),
		snapshot.summaries,
		nativeSessionListLimit,
	)
}

func (c *Codex) codexDiscoverySnapshot(refresh bool) codexDiscoverySnapshot {
	c.discoveryMu.Lock()
	defer c.discoveryMu.Unlock()

	if c.discovery != nil {
		if !refresh || time.Since(c.discovery.refreshedAt) < codexDiscoveryRefreshCoalesce {
			return cloneCodexDiscovery(c.discovery)
		}
	}

	previous := c.discovery
	paths := codexRolloutPaths(stringSliceExtra(c.cfg.Extra, "codex_sessions_dirs", nil))
	entries := make(map[string]codexDiscoveryEntry, len(paths))
	filesParsed := uint64(0)
	scans := uint64(1)
	if previous != nil {
		filesParsed = previous.filesParsed
		scans = previous.scans + 1
	}

	for id, path := range paths {
		info, err := os.Stat(path)
		if err != nil || info.IsDir() {
			continue
		}
		if previous != nil {
			if cached, ok := previous.entries[id]; ok &&
				cached.summary.Path == path &&
				cached.size == info.Size() &&
				cached.modTimeNano == info.ModTime().UnixNano() {
				entries[id] = cached
				continue
			}
		}

		cwd, hidden := codexRolloutDiscoveryMetadata(path)
		entries[id] = codexDiscoveryEntry{
			summary: codexRolloutSummary{
				Path:      path,
				UpdatedAt: epochToISO(float64(info.ModTime().UnixNano()) / 1e9),
				Cwd:       cwd,
				Hidden:    hidden,
			},
			size:        info.Size(),
			modTimeNano: info.ModTime().UnixNano(),
		}
		filesParsed++
	}

	c.discovery = &codexDiscoveryCatalog{
		entries: entries, refreshedAt: time.Now(),
		scans: scans, filesParsed: filesParsed,
	}
	return cloneCodexDiscovery(c.discovery)
}

func cloneCodexDiscovery(catalog *codexDiscoveryCatalog) codexDiscoverySnapshot {
	snapshot := codexDiscoverySnapshot{
		summaries:   make(map[string]codexRolloutSummary, len(catalog.entries)),
		hidden:      map[string]bool{},
		scans:       catalog.scans,
		filesParsed: catalog.filesParsed,
	}
	for id, entry := range catalog.entries {
		snapshot.summaries[id] = entry.summary
		if entry.summary.Hidden {
			snapshot.hidden[id] = true
		}
	}
	return snapshot
}
