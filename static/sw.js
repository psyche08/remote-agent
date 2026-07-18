// remote-agent service worker.
//   - Receive Web Push events (waiting_approval / waiting_input) and render them.
//   - Route notification clicks back to the PWA, focusing the right session.
//
// No fetch handler — no offline / no asset caching. Live tunnel traffic
// (/s/remotecoding/d/<device>/..., /_pt/...) must always hit the network.
//
// Note: with userVisibleOnly subscriptions every push MUST show a
// notification, so we do NOT suppress when the app is foreground (that would
// trip Chrome's "site updated in background" penalty). The `tag` dedups
// per-session instead. True foreground suppression needs a server-side
// presence signal — deferred.

const APP_STATIC_VERSION = "__REMOTE_AGENT_STATIC_VERSION__";
function versionHeaders(extra = {}) {
  const v = APP_STATIC_VERSION && !APP_STATIC_VERSION.startsWith("__") ? APP_STATIC_VERSION : "dev";
  return {
    "Content-Type": "application/json",
    "X-Remote-Agent-Web-Version": v,
    "X-Remote-Agent-Client-Id": "sw:" + self.registration.scope,
    "X-Remote-Agent-Client-Kind": "service-worker",
    ...extra,
  };
}

self.addEventListener('install', () => self.skipWaiting());
self.addEventListener('activate', (e) => e.waitUntil(self.clients.claim()));

self.addEventListener('push', (event) => {
  let data = {};
  try { data = event.data ? event.data.json() : {}; } catch (e) {}
  const session = data.session || '';
  const nativeSession = data.native_session || session;
  const opts = {
    body: data.body || '',
    tag: data.tag || session || 'remotecoding',
    renotify: true,
    icon: self.registration.scope + 'icon-192.png',
    badge: self.registration.scope + 'icon-192.png',
    data: { focus: nativeSession, session, url: data.url || '', device: data.device || '', kind: data.kind || '',
      provider: data.provider || '', requestId: data.request_id || '' },
  };
  // Phase 2b: an approval can be allow/denied straight from the notification.
  // (AskUserQuestion / "input" needs option selection, so no inline buttons.)
  if (data.kind === 'approval') {
    opts.actions = [
      { action: 'approve', title: '✅ 批准' },
      { action: 'deny', title: '❌ 拒绝' },
    ];
    opts.requireInteraction = true;   // keep approvals visible until acted on
  }
  event.waitUntil(self.registration.showNotification(data.title || 'Remote Agent', opts));
});

self.addEventListener('notificationclick', (event) => {
  const d = (event.notification && event.notification.data) || {};
  const act = event.action;
  event.notification.close();

  // Inline approve/deny: call back THIS device's agent (no app open needed).
  if (act === 'approve' || act === 'deny') {
    const base = self.registration.scope + (d.device ? 'd/' + d.device + '/' : '');
    event.waitUntil((async () => {
      let ok = false, detail = '';
      try {
        const r = await fetch(base + 'push/approve', {
          method: 'POST',
          headers: versionHeaders(),
          body: JSON.stringify({ provider_id: d.provider, session_id: d.session || d.focus,
            native_session_id: d.focus, request_id: d.requestId || '', decision: act === 'approve' ? 'allow' : 'deny' }),
        });
        ok = r.ok;
        if (!ok) { try { detail = (await r.json()).detail || ''; } catch (e) {} }
      } catch (e) { detail = String(e); }
      await self.registration.showNotification(
        ok ? (act === 'approve' ? '✅ 已批准' : '已拒绝') : '操作失败',
        { body: ok ? '' : (detail || '回调失败，请打开 app 处理'),
          tag: (d.focus || 'rc') + '-ack', silent: ok,
          icon: self.registration.scope + 'icon-192.png' });
    })());
    return;
  }

  // Body click: focus the app on the session.
  const focus = d.focus || '';
  event.waitUntil((async () => {
    const all = await self.clients.matchAll({ type: 'window', includeUncontrolled: true });
    const pwa = all.find((c) => c.url.startsWith(self.registration.scope));
    if (pwa) {
      pwa.postMessage({ type: 'focus-session', session: focus, provider: d.provider || '' });
      return pwa.focus();
    }
    return self.clients.openWindow(self.registration.scope + '?focus=' + encodeURIComponent(focus) +
      (d.provider ? '&provider=' + encodeURIComponent(d.provider) : ''));
  })());
});
