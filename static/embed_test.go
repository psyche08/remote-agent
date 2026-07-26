package webui

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestHandlerServesVersionedDeviceUI(t *testing.T) {
	rr := httptest.NewRecorder()
	Handler("abc12345").ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, `const APP_STATIC_VERSION = "abc12345"`) {
		t.Fatalf("device UI was not stamped: %s", body[:min(len(body), 200)])
	}
	if strings.Contains(body, "__REMOTE_AGENT_STATIC_VERSION__") {
		t.Fatal("device UI retained the build placeholder")
	}
}

func TestRelayShellOnlyBootstrapsDevices(t *testing.T) {
	body, err := os.ReadFile("shell.html")
	if err != nil {
		t.Fatal(err)
	}
	shell := string(body)
	for _, want := range []string{
		"_pt/devices", "_pt/devices/stream", "healthz", "d/",
		`<iframe id="console"`, "localStorage.getItem(DEVICE_KEY)", "connected_at", "frame.src = deviceURL(id, retry)",
		"loadDevice(event.data.device)",
	} {
		if !strings.Contains(shell, want) {
			t.Fatalf("relay shell missing %q", want)
		}
	}
	for _, forbidden := range []string{
		"sendPrompt(", "pending_approvals", "session_preview", `id="devices"`, `id="picker"`, "remote-agent-show-devices",
	} {
		if strings.Contains(shell, forbidden) {
			t.Fatalf("relay shell contains device UI behavior %q", forbidden)
		}
	}
	if strings.Contains(shell, "location.href = deviceURL") {
		t.Fatal("relay shell must host device content without navigating away")
	}
}

func TestRelayShellRecoversFromTransientDeviceGatewayErrors(t *testing.T) {
	body, err := os.ReadFile("shell.html")
	if err != nil {
		t.Fatal(err)
	}
	shell := string(body)
	for _, want := range []string{
		`let frameReady = false`,
		`function frameHasDeviceUI()`,
		`doc.getElementById("app") && doc.getElementById("device")`,
		`function scheduleFrameRetry(id)`,
		`Math.min(frameRetryDelay * 2, FRAME_RETRY_MAX_DELAY)`,
		`params.set("_rc_retry", Date.now().toString())`,
		`if (!force && frameReady && hostDevice === id`,
		`loadDevice(id, true, true)`,
	} {
		if !strings.Contains(shell, want) {
			t.Fatalf("relay shell missing transient gateway recovery behavior %q", want)
		}
	}
	if strings.Contains(shell, `frame.addEventListener("load", () => statusBox.classList.add("hidden"))`) {
		t.Fatal("relay shell must not treat an HTTP error document as a ready device UI")
	}
}

func TestDeviceUISynchronizesSessionDeviceAndProvider(t *testing.T) {
	body, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	ui := string(body)
	for _, want := range []string{
		`provider: providerId || ""`,
		`activate_session: !!activateSession`,
		`async function syncTabProvider(t)`,
		`fetch(tabUrl(t, "/provider/select")`,
		`chip.classList.toggle("active", chip.title === providerId)`,
		`rememberShellDevice(CUR_DEVICE, t.provider || CUR_PROVIDER, true)`,
	} {
		if !strings.Contains(ui, want) {
			t.Fatalf("device UI missing session selection synchronization %q", want)
		}
	}
}

func TestDeviceUISessionRefreshConvergesWithoutStaleOverwrite(t *testing.T) {
	body, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	ui := string(body)
	for _, want := range []string{
		`const SESSION_REFRESH_FLIGHTS = new Map()`,
		`const SESSION_REFRESH_RETRY_MS = [250, 500, 1000, 2000]`,
		`const SESSION_REFRESH_TIMEOUT_MS = 5000`,
		`const SESSION_REFRESH_POLL_MS = 5000`,
		`controller: new AbortController()`,
		`force: options.force === true`,
		`flight.force && attempt === 0 ? "&refresh=1" : ""`,
		`function sessionRefreshIsCurrent(flight)`,
		`SESSION_REFRESH_META = nativeRefreshMeta(body)`,
		`body && body.refreshed_at`,
		`body && body.source`,
		`SESSION_REFRESH_META.refreshing`,
		`livePromise = refreshLiveSessionsFor(flight)`,
		`setInterval(refreshSessionsIfVisible, SESSION_REFRESH_POLL_MS)`,
		`document.visibilityState === "visible"`,
		`s.hidden_from_lists !== true && s.subagent !== true`,
	} {
		if !strings.Contains(ui, want) {
			t.Fatalf("device UI missing convergent session refresh behavior %q", want)
		}
	}
	for _, forbidden := range []string{
		`const [nres, lres] = await Promise.all`,
		`refreshProviders(); refreshSessions();`,
	} {
		if strings.Contains(ui, forbidden) {
			t.Fatalf("device UI retains duplicate/blocking session refresh behavior %q", forbidden)
		}
	}
}

func TestDeviceUISortsAllSessionsGloballyAndUsesDeviceTimeZone(t *testing.T) {
	body, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	ui := string(body)
	for _, want := range []string{
		`function combinedVisibleSessions(pid)`,
		`existing.last_reply_at = newerSessionTimestamp(existing.last_reply_at, row.last_reply_at)`,
		`.sort(compareSessionsNewest)`,
		`const isLive = s._session_list_live === true`,
		`function sessionDeviceTimeMeta(body)`,
		`SESSION_REFRESH_META = { ...SESSION_REFRESH_META, device_time: deviceTime }`,
		`new Intl.DateTimeFormat("en-CA",`,
		`timeZone, year: "numeric"`,
		`deviceTime.utc_offset_minutes`,
		`timestampOwnOffsetMinutes(value)`,
		`date.getUTCFullYear()`,
	} {
		if !strings.Contains(ui, want) {
			t.Fatalf("device UI missing global session ordering/device-time behavior %q", want)
		}
	}
	for _, forbidden := range []string{
		`g.className = "live-grp"`,
		`sessionSortAt(s).slice(0, 16)`,
		`sessionSortAt(b).localeCompare(sessionSortAt(a))`,
	} {
		if strings.Contains(ui, forbidden) {
			t.Fatalf("device UI retains grouped/browser-time session behavior %q", forbidden)
		}
	}
}

func TestDeviceUIUsesTypedActionsAndConfirmedTerminalStates(t *testing.T) {
	body, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	ui := string(body)
	for _, want := range []string{
		`ACTIVE_ACTIONS`,
		`actionSupported("turn.steer", "steer")`,
		`actionSupported("turn.interrupt", "interrupt")`,
		`r.confirmed === false ? "已请求停止，等待 Codex 确认终态…"`,
		"r.confirmed === false ? `'${decision}' 已发送，等待 Codex 确认…`",
	} {
		if !strings.Contains(ui, want) {
			t.Fatalf("device UI missing typed action/terminal behavior %q", want)
		}
	}
}

func TestDeviceUIRefreshesReconnectedDevicesAndUsesNewestFallback(t *testing.T) {
	body, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	ui := string(body)
	for _, want := range []string{
		`async function refreshDevices(initial = false)`,
		`setInterval(refreshDevices, 10000)`,
		`onclick="refreshAll(true)"`,
		`mostRecentlyConnected(healthy)`,
		`Date.parse((b && b.connected_at) || "")`,
		`CUR_DEVICE_MISSES >= 2`,
	} {
		if !strings.Contains(ui, want) {
			t.Fatalf("device UI missing reconnect discovery behavior %q", want)
		}
	}
	if strings.Contains(ui, `|| DEVICES[0] || CUR_DEVICE`) {
		t.Fatal("device UI must not fall back to the alphabetically first device")
	}
}

func TestDeviceUIRendersConversationImagesAndUploadsFiles(t *testing.T) {
	body, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	ui := string(body)
	for _, want := range []string{
		`id="file-upload"`, `async function uploadFiles(files)`, `body:form`,
		`/session_asset?provider_id=`, `m.kind === "image"`, `attachments: attachmentIDs`,
	} {
		if !strings.Contains(ui, want) {
			t.Fatalf("device UI missing attachment/image behavior %q", want)
		}
	}
}

func TestDeviceUIRendersSessionAndPerTurnCosts(t *testing.T) {
	body, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	ui := string(body)
	for _, want := range []string{
		`Cost (USD)`, `m.kind === "turn_usage"`, `Cache Create <b>`,
		`Duration <b>`, `fmtCost(u.cost_usd, u.cost_known)`, `API-equivalent estimate`,
	} {
		if !strings.Contains(ui, want) {
			t.Fatalf("device UI missing usage/cost behavior %q", want)
		}
	}
}

func TestDeviceUIMovesStandardLimitsIntoUsageHeaderAndHidesSpark(t *testing.T) {
	body, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	ui := string(body)
	for _, want := range []string{
		`if (b.fast) return ""`,
		`[seg("5h", p5), seg("Weekly", p7)]`,
		`acct.limits.filter(b => !b.fast).map(limLine).filter(Boolean)`,
		`<div class="usage-head">🧠 ${parts.join(" · ")}${quotas}</div>`,
	} {
		if !strings.Contains(ui, want) {
			t.Fatalf("device UI missing inline standard quota layout %q", want)
		}
	}
	if strings.Contains(ui, `⚡Spark`) {
		t.Fatal("device UI should not render Spark quota")
	}
}

func TestDeviceUITabCloseHasMobileTouchTarget(t *testing.T) {
	body, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	ui := string(body)
	for _, want := range []string{
		`.tab .x { width:44px; height:44px; min-width:44px; min-height:44px;`,
		`<button type="button" class="x"`,
		`ev.target.closest(".x")`,
	} {
		if !strings.Contains(ui, want) {
			t.Fatalf("device UI missing accessible tab close target %q", want)
		}
	}
}

func TestDeviceUIDoesNotCollapseSameImageFromDifferentToolResults(t *testing.T) {
	body, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	ui := string(body)
	if !strings.Contains(ui, `m.asset_id || "", m.tool_use_id || ""`) || !strings.Contains(ui, `(a.tool_use_id || "") === (b.tool_use_id || "")`) {
		t.Fatal("device UI image dedupe must preserve distinct tool-result occurrences")
	}
}

func TestDeviceUIOnlyChecksAgentReleaseVersion(t *testing.T) {
	body, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	ui := string(body)
	for _, want := range []string{
		`id="version-agent"`,
		`id="version-latest"`,
		`let AGENT_UPDATE_STATE = null`,
		`(AGENT_UPDATE_STATE || {}).last_to`,
		`async function refreshAgentUpdateInfo()`,
		`api("/update")`,
	} {
		if !strings.Contains(ui, want) {
			t.Fatalf("device UI missing agent release check %q", want)
		}
	}
	for _, forbidden := range []string{
		`id="version-web"`,
		`网页版本和 Agent 不一致`,
		`web !== agentCommit`,
		`scheduleVersionAction`,
		`autoUpdateForVersion`,
		`VERSION_ACTION_PAIR`,
	} {
		if strings.Contains(ui, forbidden) {
			t.Fatalf("device UI still compares PWA and agent versions %q", forbidden)
		}
	}
}

func TestDeviceUIDirectlySendsNativeCodexSessionWithoutAttach(t *testing.T) {
	body, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	ui := string(body)
	for _, want := range []string{
		`const directCodex = !!(!sid && tab && tab.resumeId && providerIsCodex(tab.provider));`,
		`if (directCodex) sid = tab.resumeId;`,
		`if (directCodex && tab && r.session_id)`,
		`const canSend = hasSession || directCodex;`,
		`可直接发送到此 Codex thread`,
	} {
		if !strings.Contains(ui, want) {
			t.Fatalf("device UI missing direct native Codex send behavior %q", want)
		}
	}
	for _, forbidden := range []string{`发消息将接入此 Codex thread`, `接入 Codex thread…`} {
		if strings.Contains(ui, forbidden) {
			t.Fatalf("device UI still exposes Codex attach flow %q", forbidden)
		}
	}
}

func TestHandlerServesEmbeddedAssets(t *testing.T) {
	for _, path := range []string{"/manifest.webmanifest", "/sw.js", "/icon-192.png"} {
		rr := httptest.NewRecorder()
		Handler("abc12345").ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
		if rr.Code != http.StatusOK || rr.Body.Len() == 0 {
			t.Fatalf("path=%s status=%d size=%d", path, rr.Code, rr.Body.Len())
		}
	}
}
