/* FnCluster v0.2 PoC: inject synthetic entries into the native "全部应用" page.
   Browser-only, reversible. Steps:
     1. Open the fnOS desktop and paste this whole file into the DevTools console.
     2. Verify the interceptor works:
          (await __fnclusterAllApps.fetch()).filter(e => e.entryKey.startsWith('fncluster:'))
     3. Make the desktop refetch its entry list through the interceptor:
          __fnclusterAllApps.refresh()
        then open "全部应用". (refresh emits the same `domainEvent` fnOS uses for
        `user-desktop-entry-change`, so nothing is switched; it is throttled to
        one trigger per second, just call it again after a moment.)
     4. Check: do the two probe entries render? Click one - it should open a
        native window pointing at /app/fncluster/?probe=N.
     5. __fnclusterAllApps.setDisplay(false) + refresh() confirms the launchpad
        filter (hiddenDesktopIcon entries are skipped); setDisplay(true) restores.
     6. __fnclusterAllApps.disable() detaches the injection at runtime; the file
        itself changes nothing on disk. */
(() => {
  const BUNDLE = '/assets/index-DhUqaTaA.js';
  const REQ = 'appcgi.sac.entry.v1.getEntryList';
  const PROBE_ICON = '/app-center-static/icon/fncluster/icon.png';

  let displayFlag = true;
  let mainSocket = null;
  const state = { socket: false, injections: 0, last: null, disabled: false, display: true };

  const isMainSocket = value => !!value && value.isMain === true && typeof value.useRes === 'function' && typeof value.send === 'function';

  function probeEntries() {
    const make = (n, title) => ({
      entryKey: `fncluster:probe:${n}`,
      source: 'fncluster',
      sourceName: 'FnCluster',
      appName: `fncluster.probe.${n}`,
      appID: `fncluster.probe.${n}`,
      entryName: `fncluster.probe.${n}`,
      title,
      desc: 'FnCluster all-apps injection probe',
      icon: PROBE_ICON,
      type: 'iframe',
      uri: { host: '', path: `/app/fncluster/?probe=${n}`, port: '', protocol: '' },
      fullUrl: `/app/fncluster/?probe=${n}`,
      microApp: false,
      nativeApp: false,
      isDisplay: displayFlag,
      status: 'available',
    });
    return [make(1, 'FnCluster 探针 A'), make(2, 'FnCluster 探针 B')];
  }

  function isSuccess(result) {
    return result === 'suc' || result === 'succ' || result === 'success';
  }

  function remember(pending, payload) {
    if (typeof payload === 'string') {
      const start = payload.indexOf('{');
      if (start < 0) return;
      try {
        payload = JSON.parse(payload.slice(start));
      } catch (_) {
        return;
      }
    }
    if (payload && payload.reqid && payload.req) pending.set(payload.reqid, payload.req);
  }

  async function install() {
    const mod = await import(BUNDLE);
    const main = mod.Gc || Object.values(mod || {}).find(isMainSocket);
    if (!isMainSocket(main)) throw new Error('main socket not found');
    const pending = new Map();
    const recorder = {
      fulfilled: event => {
        remember(pending, event && event.data);
        return event;
      },
      rejected: error => Promise.reject(error),
    };
    if (main.interceptors && Array.isArray(main.interceptors.request)) {
      main.interceptors.request.unshift(recorder);
    } else {
      main.useReq(event => {
        remember(pending, event && event.data);
        return event;
      });
    }
    main.useRes(event => {
      const message = event && event.data;
      if (state.disabled || !message || !message.reqid) return event;
      if (pending.get(message.reqid) !== REQ) return event;
      const data = message.data;
      if (!data || !Array.isArray(data.list)) return event;
      state.last = { at: new Date().toISOString(), result: message.result, count: data.list.length };
      if (!isSuccess(message.result)) return event;
      const existing = new Set(data.list.map(entry => entry && entry.entryKey));
      const extra = probeEntries().filter(entry => !existing.has(entry.entryKey));
      if (!extra.length) return event;
      data.list = data.list.concat(extra);
      state.injections += 1;
      console.log('[FnCluster PoC] injected', extra.length, 'entries; total', data.list.length);
      console.log('[FnCluster PoC] first native entry:', JSON.parse(JSON.stringify(data.list[0] || {})));
      console.log('[FnCluster PoC] injected entry:', JSON.parse(JSON.stringify(extra[0])));
      return event;
    });
    mainSocket = main;
    state.socket = true;
    console.log('[FnCluster PoC] interceptor installed on the main socket');
  }

  window.__fnclusterAllApps = {
    get state() {
      return { ...state };
    },
    entries: probeEntries,
    refresh() {
      if (!mainSocket) {
        console.warn('[FnCluster PoC] main socket not ready yet');
        return;
      }
      // Same domain event the desktop subscribes to for entry-list refreshes.
      mainSocket.emit('domainEvent', { domainEvent: { event: 'user-desktop-entry-change', from: 'trim-sac' } });
      console.log('[FnCluster PoC] desktop entry refetch triggered (throttled to 1/s; repeat after a moment if needed)');
    },
    setDisplay(value) {
      displayFlag = !!value;
      state.display = displayFlag;
      console.log('[FnCluster PoC] isDisplay =', displayFlag, '- re-run __fnclusterAllApps.refresh()');
    },
    disable() {
      state.disabled = true;
      console.log('[FnCluster PoC] injection disabled');
    },
    enable() {
      state.disabled = false;
      console.log('[FnCluster PoC] injection enabled');
    },
    async fetch() {
      const mod = await import(BUNDLE);
      const language = document.documentElement.lang || navigator.language || 'zh-CN';
      const result = await mod.Hc.entry.getEntryList({ data: { language } });
      return result && result.data && result.data.list;
    },
  };

  install().catch(error => console.error('[FnCluster PoC] install failed:', error));
})();
