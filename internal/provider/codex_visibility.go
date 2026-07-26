package provider

import (
	"bufio"
	"encoding/json"
	"os"
	"strings"
)

const hiddenFromSessionListsKey = "hidden_from_lists"

// HiddenSessionIDs returns Codex child-agent thread IDs that should remain
// directly addressable but must not appear as standalone user tasks.
func (c *Codex) HiddenSessionIDs() map[string]bool {
	return c.codexDiscoverySnapshot(false).hidden
}

func codexRolloutIsSubagent(path string) bool {
	_, hidden := codexRolloutDiscoveryMetadata(path)
	return hidden
}

func codexRolloutDiscoveryMetadata(path string) (string, bool) {
	f, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer f.Close()

	cwd := ""
	hidden := false
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for i := 0; scanner.Scan() && i <= 30; i++ {
		var rec map[string]any
		if err := json.Unmarshal([]byte(scanner.Text()), &rec); err != nil {
			continue
		}
		if cwd == "" {
			cwd = firstNonEmpty(stringAny(rec["cwd"]), stringAny(mapAny(rec["payload"])["cwd"]))
		}
		if stringAny(rec["type"]) != "session_meta" {
			continue
		}
		hidden = codexMetadataIsSubagent(mapAny(rec["payload"]))
		if cwd != "" {
			return cwd, hidden
		}
	}
	return cwd, hidden
}

func codexMetadataIsSubagent(meta map[string]any) bool {
	if len(meta) == 0 {
		return false
	}
	if firstNonEmpty(stringAny(meta["parentThreadId"]), stringAny(meta["parent_thread_id"])) != "" {
		return true
	}
	for _, key := range []string{"threadSource", "thread_source"} {
		if normalizedCodexSource(stringAny(meta[key])) == "subagent" {
			return true
		}
	}
	for _, key := range []string{"source", "sessionSource", "session_source"} {
		source := meta[key]
		if normalizedCodexSource(stringAny(source)) == "subagent" {
			return true
		}
		sourceMap := mapAny(source)
		if sourceMap == nil {
			continue
		}
		if _, ok := sourceMap["subAgent"]; ok {
			return true
		}
		if _, ok := sourceMap["subagent"]; ok {
			return true
		}
	}
	return false
}

func normalizedCodexSource(source string) string {
	source = strings.ToLower(strings.TrimSpace(source))
	source = strings.NewReplacer("_", "", "-", "", " ", "").Replace(source)
	return source
}

func markCodexSessionVisibility(row map[string]any, meta map[string]any) {
	if row == nil || !codexMetadataIsSubagent(meta) {
		return
	}
	row[hiddenFromSessionListsKey] = true
	row["subagent"] = true
	if parent := firstNonEmpty(stringAny(meta["parentThreadId"]), stringAny(meta["parent_thread_id"])); parent != "" {
		row["parent_thread_id"] = parent
	}
}
