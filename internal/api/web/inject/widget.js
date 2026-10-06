/* FnCluster desktop widget bridge for the verified fnOS 1.2.0701 build.
   - Node selector next to the native desktop widget's display settings with two
     choices: the local native cards, or the cluster overview panel. The
     injected nodes live inside the native widget DOM (same stacking and
     clipping as the expanded window); a MutationObserver re-attaches them if
     React reconciliation ever removes them.
   - Reports the desktop's own localized entry list to the agent.
   - Appends remote node app entries to the native "全部应用" list by hooking
     the main socket's getEntryList response (append-only, feature-detected).
   Browser-only; nothing on disk changes. */
(() => {
  if (window.__fnclusterWidget) return;
  window.__fnclusterWidget = true;

  const BUNDLE = '/assets/index-DhUqaTaA.js';
  const API_NODES = '/app/fncluster/api/v1/nodes';
  const API_MESH = '/app/fncluster/api/v1/mesh';
  const API_PANELS_REPORT = '/app/fncluster/api/v1/panels/report';
  const API_RELAY_TOKENS = '/app/fncluster/api/v1/relay/tokens';
  const STORAGE_KEY = 'fncluster.widget.mode';
  const POLL_MS = 500;
  const PANELS_REPORT_MS = 30 * 60 * 1000;
  const RELAY_URL_TTL = 11 * 60 * 60 * 1000;
  const COLLAPSED_HEIGHT = 80;
  const ENTRY_LIST_REQ = 'appcgi.sac.entry.v1.getEntryList';
  const REMOTE_ENTRY_LIMIT = 50; // per node
  const REMOTE_ENTRY_ICON = '/app-center-static/icon/fncluster/icon.png';
  const DESKTOP_ENTRY_ICON = '/assets/desktop-Cid_qXcw.png';
  // Remote entries never expose these: FnCluster itself (recursive) and the
  // core Settings / App Center panels, which must stay local-only.
  const EXCLUDED_REMOTE_APPS = ['fncluster', 'trim.setting', 'trim.app-center'];
  const EXCLUDED_REMOTE_IDS = ['fnos.settings'];
  const GATEWAY_APP = 'fncluster.gateway';

  const state = {
    api: null,
    main: null,
    mainSocket: null,
    appsStore: null,
    mainPending: new Map(),
    localId: '',
    nodes: [],
    nodesPromise: null,
    remoteEntries: 0,
    relayUrls: new Map(),
    relayFetchedAt: 0,
    relayPromise: null,
    mode: 'local',
    entrySignature: '',
    timer: null,
    panelsTimer: null,
    button: null,
    menu: null,
    panel: null,
    observer: null,
    observedWrapper: null,
    panelsReporting: false,
  };

  const isUsrd = value => !!value && value.usrd && typeof value.usrd.get === 'function' && !!value.sysinfo && typeof value.sysinfo.getUptime === 'function';
  // eZ in the tested build: `new YG(url+'?type=main', {isMain:true})`.
  const isMainSocket = value => !!value && value.isMain === true && typeof value.useRes === 'function' && typeof value.send === 'function';
  const isSuccess = result => result === 'suc' || result === 'succ' || result === 'success';
  // Our own synthetic all-apps entries; never report or re-inject them, or the
  // catalog feedback loop composes titles like "系统设置 · FN02 · FN01 · FN02".
  const isSyntheticEntry = entry => !!entry && (
    entry.source === 'fncluster'
    || String(entry.entryKey || '').startsWith('fncluster:')
    || String(entry.appName || '').startsWith('fncluster.remote.')
  );

  // The desktop app store exposed as `Pa`: a derived state machine whose
  // `state.apps` is what the native launchpad renders.
  const isAppsStore = value => !!value && value.state && Array.isArray(value.state.apps);
  const isRemoteApp = app => !!app && String(app.appName || '').startsWith('fncluster.remote.');

  // Minified export names are build-specific; known names first, then feature scan.
  function resolveExports(mod) {
    let api = mod.Hc;
    let main = mod.Gc;
    let apps = mod.Pa;
    const values = Object.values(mod || {});
    if (!isUsrd(api)) api = values.find(isUsrd) || api;
    if (!isMainSocket(main)) main = values.find(isMainSocket) || main;
    if (!isAppsStore(apps)) apps = values.find(isAppsStore) || apps;
    return { api, main, apps };
  }

  function remember(pending, payload) {
    if (typeof payload === 'string') {
      // The signed request interceptor may run before us and prefix the JSON.
      const start = payload.indexOf('{');
      if (start < 0) return;
      try {
        payload = JSON.parse(payload.slice(start));
      } catch (_) {
        return;
      }
    }
    if (!payload || !payload.reqid || !payload.req) return;
    pending.set(payload.reqid, payload.req);
    if (pending.size > 512) {
      const first = pending.keys().next();
      if (!first.done) pending.delete(first.value);
    }
  }

  const loadMode = () => {
    try {
      const saved = JSON.parse(localStorage.getItem(STORAGE_KEY) || '{}');
      if (saved && saved.mode === 'overview') state.mode = 'overview';
    } catch (_) { /* keep defaults */ }
  };
  const saveMode = () => {
    try { localStorage.setItem(STORAGE_KEY, JSON.stringify({ mode: state.mode })); } catch (_) {}
  };

  const overviewActive = () => state.mode === 'overview';

  const size = value => {
    if (!Number.isFinite(value)) return '—';
    if (value >= 2 ** 40) return `${(value / 2 ** 40).toFixed(2)} TB`;
    if (value >= 2 ** 30) return `${(value / 2 ** 30).toFixed(1)} GB`;
    if (value >= 2 ** 20) return `${(value / 2 ** 20).toFixed(1)} MB`;
    return `${Math.round(value / 1024)} KB`;
  };
  const rate = value => `${size(value)}/s`;
  const cpuPercent = node => {
    const value = node && node.metrics ? node.metrics.cpuPercent : null;
    return typeof value === 'number' && value >= 0 ? Math.min(150, value) : 0;
  };
  const rates = node => (node && node.metrics && node.metrics.rates) || {};
  const uptimeText = seconds => {
    if (!Number.isFinite(seconds) || seconds <= 0) return '—';
    const days = Math.floor(seconds / 86400);
    const hours = Math.floor((seconds % 86400) / 3600);
    const minutes = Math.floor((seconds % 3600) / 60);
    return `${days}天 ${hours}小时 ${minutes}分`;
  };
  const nodeLabel = node => (node ? node.name || node.hostname || node.id.slice(0, 8) : '');

  // --- native widget geometry ---
  function widgetRoot() {
    for (const el of document.querySelectorAll('div.absolute')) {
      if (el.classList.contains('right-[14px]') && el.classList.contains('top-[17px]')) return el;
    }
    return null;
  }

  // root (absolute, z-[1], overflow-hidden rounded-2xl)
  //   wrapper (rounded-2xl card, p-[6px], gap-y-[6px])
  //     header (h-9, settings trigger + chevron)
  //     content (native cards, flex-1)
  function layout() {
    const root = widgetRoot();
    if (!root) return null;
    const wrapper = root.firstElementChild;
    const header = wrapper ? wrapper.children[0] : null;
    const content = wrapper ? wrapper.children[1] || null : null;
    const trigger = root.querySelector('div.flex.cursor-pointer.items-center.gap-x-\\[9px\\]');
    const rootRect = root.getBoundingClientRect();
    if (!rootRect.width || !rootRect.height) return null;
    // The bundle marks the open state with root `bottom-[17px]` and header
    // width 100%; the measured height cannot be used while the overview panel
    // keeps the wrapper tall. Fall back to height only when both are absent.
    const expanded = root.classList.contains('bottom-[17px]')
      || (!!header && header.style.width === '100%')
      || (!header && rootRect.height >= COLLAPSED_HEIGHT);
    return {
      root,
      wrapper,
      header,
      content,
      trigger,
      rootRect,
      collapsed: !expanded,
    };
  }

  function currentLabel() {
    return overviewActive() ? '总览' : '本机';
  }

  // --- embedded controls ---
  function ensureButton(view) {
    if (!view || !view.trigger) return;
    if (!state.button) {
      const button = document.createElement('button');
      button.type = 'button';
      button.id = 'fncluster-widget-switch';
      button.style.cssText = 'cursor:pointer;border:0;border-radius:7px;padding:2px 8px;font:inherit;font-size:12px;line-height:1.4;white-space:nowrap;background:var(--semi-color-fill-0);color:var(--semi-color-text-0)';
      // Stop only the click: the native settings dropdown toggles on click,
      // while its outside-close logic listens for document mousedown.
      button.addEventListener('click', event => {
        event.preventDefault();
        event.stopPropagation();
        toggleMenu();
      }, true);
      state.button = button;
    }
    const button = state.button;
    button.textContent = `${currentLabel()} ▾`;
    // Keep the collapsed mini strip at its native width.
    button.style.display = view.collapsed ? 'none' : '';
    if (button.parentElement !== view.trigger) view.trigger.append(button);
  }

  function ensureMenu(view) {
    if (!view || !view.header) return null;
    if (!state.menu) {
      const menu = document.createElement('div');
      menu.id = 'fncluster-widget-menu';
      menu.style.cssText = 'position:absolute;left:0;top:100%;margin-top:4px;z-index:10;display:none;min-width:120px;padding:4px;border-radius:10px;background:var(--semi-color-Component-card);box-shadow:0 4px 16px #0003;font-size:12px;color:var(--semi-color-text-0)';
      state.menu = menu;
      document.addEventListener('click', event => {
        if (!state.menu || state.menu.style.display === 'none') return;
        if (state.menu.contains(event.target)) return;
        if (state.button && state.button.contains(event.target)) return;
        closeMenu();
      }, true);
    }
    if (state.menu.parentElement !== view.header) view.header.append(state.menu);
    return state.menu;
  }

  function menuItem(label, active, onSelect) {
    const item = document.createElement('div');
    item.textContent = label;
    item.style.cssText = `padding:5px 10px;border-radius:7px;cursor:pointer;white-space:nowrap;${active ? 'background:var(--semi-color-fill-0);font-weight:600;' : ''}`;
    item.addEventListener('click', event => {
      event.preventDefault();
      event.stopPropagation();
      onSelect();
    });
    return item;
  }

  function toggleMenu() {
    const view = layout();
    const menu = ensureMenu(view);
    if (!menu) return;
    if (menu.style.display !== 'none') {
      closeMenu();
      return;
    }
    menu.replaceChildren(
      menuItem('总览', overviewActive(), () => selectMode('overview')),
      menuItem('本机', !overviewActive(), () => selectMode('local')),
    );
    if (state.button) menu.style.left = `${Math.max(0, state.button.offsetLeft)}px`;
    menu.style.display = '';
    // The native dropdown treats clicks inside its trigger as "inside", and
    // our button lives there. A mousedown on body is outside, so Semi's
    // document listener closes the native menu.
    document.body.dispatchEvent(new MouseEvent('mousedown', { bubbles: true, cancelable: true, view: window }));
  }

  function closeMenu() {
    if (state.menu) state.menu.style.display = 'none';
  }

  function syncMenuState(view) {
    if (!view || view.collapsed) closeMenu();
  }

  // --- overview panel ---
  // Single source of truth for the injected chrome, used by both the poll and
  // the observer fast path. The native content stays hidden for the whole time
  // overview mode is on (even while collapsed, where its cards are not
  // rendered anyway); only a local-mode switch restores it. That asymmetry
  // used to let the native cards and the overview show at the same time after
  // a collapse/expand cycle until the next poll.
  function applyOverviewState(view) {
    const overview = overviewActive();
    if (view && view.content) view.content.style.display = overview ? 'none' : '';
    const showing = overview && view && !view.collapsed;
    // The absolute root spans further than the card; let the empty area pass
    // clicks through while the overview is shown.
    if (view && view.root) view.root.style.pointerEvents = showing ? 'none' : '';
    if (view && view.wrapper) view.wrapper.style.pointerEvents = showing ? 'auto' : '';
    if (view && state.button) state.button.style.display = view.collapsed ? 'none' : '';
    if (view && view.collapsed) closeMenu();
    const panel = state.panel;
    if (!panel) return;
    if (showing && view.wrapper) {
      if (panel.parentElement !== view.wrapper) view.wrapper.append(panel);
      panel.style.display = 'flex';
      return;
    }
    panel.style.display = 'none';
    if (panel.parentElement) panel.remove();
  }

  function ensurePanel(view) {
    if (!state.panel) {
      const panel = document.createElement('div');
      panel.id = 'fncluster-widget-overview';
      panel.style.cssText = 'box-sizing:border-box;display:none;width:100%;flex-direction:column;gap:6px;min-height:0;overflow-y:auto;overflow-x:hidden;font-size:12px;color:var(--semi-color-text-0)';
      state.panel = panel;
    }
    if (overviewActive() && view && !view.collapsed) renderOverview();
    applyOverviewState(view);
  }

  function selectMode(mode) {
    state.mode = mode === 'overview' ? 'overview' : 'local';
    console.log(`[FnCluster] select ${currentLabel()}`);
    saveMode();
    if (state.api && state.api.usrd) {
      state.api.usrd.set({ name: 'browser.desktopWidgetWinOpen', value: 'open' }).catch(() => {});
    }
    closeMenu();
    syncUI();
  }

  function section(title, body) {
    const card = document.createElement('div');
    card.style.cssText = 'background:var(--semi-color-Component-card);border-radius:12px;padding:8px;box-sizing:border-box';
    const heading = document.createElement('div');
    heading.textContent = title;
    heading.style.cssText = 'font-weight:600;font-size:13px;margin-bottom:4px;color:var(--semi-color-text-0)';
    card.append(heading, body);
    return card;
  }

  function row(left, right) {
    const line = document.createElement('div');
    line.style.cssText = 'display:flex;justify-content:space-between;gap:8px;color:var(--semi-color-text-1);line-height:18px';
    const a = document.createElement('span');
    a.textContent = left;
    const b = document.createElement('span');
    b.textContent = right;
    line.append(a, b);
    return line;
  }

  let overviewItems = ['system', 'net', 'disk', 'storage'];

  async function loadOverviewItems() {
    if (!state.api || !state.api.usrd) return;
    try {
      const result = await state.api.usrd.get({ name: 'browser.desktopWidget' });
      const value = result && result.value ? JSON.parse(result.value) : null;
      if (Array.isArray(value) && value.length) overviewItems = value;
    } catch (_) { /* keep previous */ }
  }

  function renderOverview() {
    const panel = state.panel;
    panel.replaceChildren();
    const online = state.nodes.filter(node => node.online);
    if (!online.length) {
      panel.append(section('FnCluster', row('节点', '暂无数据')));
      return;
    }
    if (overviewItems.includes('system')) {
      const body = document.createElement('div');
      for (const node of online) {
        const metrics = node.metrics || {};
        const title = row(nodeLabel(node), '在线');
        title.style.color = 'var(--semi-color-text-0)';
        title.style.fontWeight = '600';
        body.append(
          title,
          row('CPU', `${cpuPercent(node).toFixed(1)}%`),
          row('内存', `${size(metrics.memoryUsedBytes)} / ${size(metrics.memoryTotalBytes)}`),
          row('负载', (metrics.load || [0, 0, 0]).map(n => n.toFixed(2)).join(' / ')),
          row('运行', uptimeText(metrics.uptimeSeconds)),
        );
      }
      panel.append(section('运行状态', body));
    }
    const columns = document.createElement('div');
    columns.style.cssText = 'display:flex;gap:6px;align-items:flex-start';
    if (overviewItems.includes('net')) {
      const body = document.createElement('div');
      for (const node of online) {
        const nodeRates = rates(node);
        body.append(
          row(nodeLabel(node), `↑ ${rate(nodeRates.networkTransmitBytesPerSecond || 0)}`),
          row('', `↓ ${rate(nodeRates.networkReceiveBytesPerSecond || 0)}`),
        );
      }
      const card = section('网络', body);
      card.style.flex = '1';
      columns.append(card);
    }
    if (overviewItems.includes('disk')) {
      const body = document.createElement('div');
      for (const node of online) {
        const nodeRates = rates(node);
        body.append(
          row(nodeLabel(node), `读 ${rate(nodeRates.diskReadBytesPerSecond || 0)}`),
          row('', `写 ${rate(nodeRates.diskWriteBytesPerSecond || 0)}`),
        );
      }
      const card = section('存储读写', body);
      card.style.flex = '1';
      columns.append(card);
    }
    if (columns.children.length) panel.append(columns);
    if (overviewItems.includes('storage')) {
      const body = document.createElement('div');
      for (const node of online) {
        body.append(row(nodeLabel(node), ''));
        for (const fs of node.metrics && node.metrics.filesystems ? node.metrics.filesystems : []) {
          body.append(row(`  ${fs.mount}`, `${size(fs.usedBytes)} / ${size(fs.totalBytes)}`));
        }
      }
      panel.append(section('存储空间', body));
    }
    if (overviewItems.includes('ups')) {
      const body = document.createElement('div');
      for (const node of online) body.append(row(nodeLabel(node), '未接入'));
      panel.append(section('UPS', body));
    }
  }

  // Re-attach injected nodes if React reconciliation removed them and apply
  // the same overview state as the poll. Never renders, so the observer cannot
  // loop; appending only happens when a node is actually missing.
  function reattach(view) {
    if (!view) return;
    if (state.button && view.trigger && state.button.parentElement !== view.trigger) view.trigger.append(state.button);
    if (state.menu && view.header && state.menu.parentElement !== view.header) view.header.append(state.menu);
    applyOverviewState(view);
  }

  function observeWrapper(view) {
    const wrapper = view ? view.wrapper : null;
    if (!wrapper || state.observedWrapper === wrapper) return;
    if (state.observer) state.observer.disconnect();
    state.observedWrapper = wrapper;
    if (state.observer) state.observer.observe(wrapper, { childList: true, subtree: true });
  }

  function syncUI() {
    const view = layout();
    observeWrapper(view);
    ensureButton(view);
    ensureMenu(view);
    syncMenuState(view);
    ensurePanel(view);
    refreshOnNewPanels();
  }

  // New remote panels (apps installed while the cluster runs) must reach the
  // launchpad without a page reload: watch the panel set and ask the desktop
  // to re-run getEntryList, which re-appends our entries.
  function remotePanelSignature() {
    const nodes = state.nodes;
    const hasLocalFlag = nodes.some(node => node && node.local === true);
    const parts = [];
    for (const node of nodes) {
      if (!node || !node.online || isLocalNode(node, hasLocalFlag)) continue;
      for (const panel of Array.isArray(node.panels) ? node.panels : []) {
        if (!panel || !panel.id || String(panel.id).startsWith('fncluster:')) continue;
        if (excludedRemotePanel(panel)) continue;
        parts.push(`${node.id}.${panel.id}`);
      }
    }
    return parts.sort().join(',');
  }

  function refreshOnNewPanels() {
    if (!state.mainSocket) return;
    const signature = remotePanelSignature();
    if (!signature) return;
    if (!state.entrySignature) {
      state.entrySignature = signature;
      return;
    }
    if (signature === state.entrySignature) return;
    state.entrySignature = signature;
    console.log('[FnCluster] remote panel set changed; requesting a desktop entry refresh');
    state.mainSocket.emit('domainEvent', { domainEvent: { event: 'user-desktop-entry-change', from: 'trim-sac' } });
  }

  // The desktop's own entry list is the complete, localized catalog (system
  // apps, FPK apps on any volume, external links). Report it to the agent so
  // the FnCluster UI and the remote panels can use it; best-effort only.
  async function reportPanels() {
    if (state.panelsReporting || publicPage()) return;
    const api = state.api;
    if (!api || !api.entry || typeof api.entry.getEntryList !== 'function') return;
    state.panelsReporting = true;
    try {
      const language = document.documentElement.lang || navigator.language || 'zh-CN';
      const result = await api.entry.getEntryList({ data: { language } });
      const list = result && result.data && result.data.list;
      if (!Array.isArray(list) || !list.length) return;
      const reportable = list.filter(entry => !isSyntheticEntry(entry));
      if (!reportable.length) return;
      await fetch(API_PANELS_REPORT, {
        method: 'POST',
        credentials: 'same-origin',
        headers: { 'Content-Type': 'application/json', 'X-FnCluster': '1' },
        body: JSON.stringify({ language, origin: window.location.origin, entries: reportable }),
      });
    } catch (_) {
      /* the catalog is best-effort; the scan fallback stays in place */
    } finally {
      state.panelsReporting = false;
    }
  }

  // --- native all-apps injection (remote entries) ---
  // The desktop reads its catalog from appcgi.sac.entry.v1.getEntryList; we
  // append one synthetic entry per remote node panel. Append-only, keyed by
  // reqid, and it never touches anything but that response.
  async function ensureNodes() {
    if (state.nodes.length) return state.nodes;
    if (!state.nodesPromise) {
      state.nodesPromise = (async () => {
        try {
          const response = await fetch(API_NODES, { credentials: 'same-origin' });
          if (response.ok) {
            const nodes = await response.json();
            if (Array.isArray(nodes) && nodes.length) state.nodes = nodes;
          }
        } catch (_) { /* no remote catalog yet */ }
        state.nodesPromise = null;
        return state.nodes;
      })();
    }
    return state.nodesPromise;
  }

  // DIRECT mode: web applications open on the remote node itself (its browser
  // origin + the app's own path); native panels have no URL and keep the
  // FnCluster placeholder. xJe() builds from `uri` when `fullUrl` is skipped,
  // so both are supplied for every entry.
  function directPanelURL(node, panel) {
    if (!node || !panel) return '';
    if (panel.kind === 'external' && panel.externalUrl) return panel.externalUrl;
    if (panel.kind === 'application' && panel.directUrl && node.webOrigin) {
      const base = String(node.webOrigin).replace(/\/+$/, '');
      const path = String(panel.directUrl);
      return path.startsWith('/') ? `${base}${path}` : `${base}/${path}`;
    }
    return '';
  }

  function entryURI(target) {
    try {
      const parsed = new URL(target, window.location.href);
      return {
        protocol: parsed.protocol.replace(':', ''),
        host: parsed.hostname,
        port: parsed.port,
        path: `${parsed.pathname}${parsed.search}${parsed.hash}`,
      };
    } catch (_) {
      return { host: '', path: target, port: '', protocol: '' };
    }
  }

  const isLocalNode = (node, hasLocalFlag) => node.local === true || (!hasLocalFlag && node.id === state.localId);
  const excludedRemotePanel = panel => !!panel && (
    EXCLUDED_REMOTE_APPS.includes(panel.appName) || EXCLUDED_REMOTE_IDS.includes(panel.id)
  );
  const desktopLabel = () => {
    try {
      return String(document.documentElement.lang || '').toLowerCase().startsWith('en') ? 'Remote Desktop' : '远端桌面';
    } catch (_) {
      return '远端桌面';
    }
  };

  const remoteIconURL = (node, panel) => panel.appName
    ? `/app/fncluster/api/v1/panels/icon/${encodeURIComponent(node.id)}/${encodeURIComponent(panel.appName)}`
    : REMOTE_ENTRY_ICON;

  // Relay frames are a different origin (same host, different port), so their
  // storage cannot see this desktop's theme; carry the effective mode over.
  const themeMode = () => {
    try {
      return document.body && document.body.getAttribute('theme-mode') === 'dark' ? 'dark' : 'light';
    } catch (_) {
      return 'light';
    }
  };

  function installThemeBridge() {
    window.addEventListener('message', event => {
      const data = event && event.data;
      if (!data || data.type !== 'fncluster:theme?') return;
      try {
        event.source.postMessage({ type: 'fncluster:theme', mode: themeMode() }, '*');
      } catch (_) { /* ignore */ }
    });
    const broadcast = () => {
      const mode = themeMode();
      document.querySelectorAll('iframe').forEach(frame => {
        try { frame.contentWindow.postMessage({ type: 'fncluster:theme', mode }, '*'); } catch (_) { /* ignore */ }
      });
    };
    try {
      new MutationObserver(broadcast).observe(document.body, { attributes: true, attributeFilter: ['theme-mode'] });
      new MutationObserver(broadcast).observe(document.documentElement, { attributes: true, attributeFilter: ['class'] });
    } catch (_) { /* no body yet: the request/reply path still works */ }
  }

  // Relay sessions are minted in one batch and cached for ~11h; without them
  // relayable panels fall back to DIRECT URLs.
  async function ensureRelayUrls() {
    if (state.relayUrls.size && Date.now() - state.relayFetchedAt < RELAY_URL_TTL) return;
    if (state.relayPromise) return state.relayPromise;
    const nodes = state.nodes;
    const hasLocalFlag = nodes.some(node => node && node.local === true);
    const targets = [];
    for (const node of nodes) {
      if (!node || !node.online || isLocalNode(node, hasLocalFlag)) continue;
      // One gateway session per node serves every native panel it hosts.
      targets.push({ nodeId: node.id, appName: GATEWAY_APP });
      for (const panel of Array.isArray(node.panels) ? node.panels : []) {
        if (panel && panel.relayable && panel.appName && !excludedRemotePanel(panel)) {
          targets.push({ nodeId: node.id, appName: panel.appName });
        }
      }
    }
    if (!targets.length) return;
    state.relayPromise = (async () => {
      try {
        const response = await fetch(API_RELAY_TOKENS, {
          method: 'POST',
          credentials: 'same-origin',
          headers: { 'Content-Type': 'application/json', 'X-FnCluster': '1' },
          body: JSON.stringify({ targets }),
        });
        if (response.ok) {
          const data = await response.json();
          const relayUrls = new Map();
          for (const item of (data && data.targets) || []) {
            if (item && item.nodeId && item.appName && item.url) {
              const key = item.appName === GATEWAY_APP
                ? `gateway:${item.nodeId}`
                : `${item.nodeId}|${item.appName}`;
              relayUrls.set(key, item.url);
            }
          }
          state.relayUrls = relayUrls;
          state.relayFetchedAt = Date.now();
        }
      } catch (_) {
        /* relay is optional; DIRECT stays as the fallback */
      } finally {
        state.relayPromise = null;
      }
    })();
    return state.relayPromise;
  }

  function remoteEntries() {
    const nodes = state.nodes;
    const hasLocalFlag = nodes.some(node => node && node.local === true);
    if (!hasLocalFlag && !state.localId) {
      // No local marker in the nodes payload (older agent) and mesh has not
      // answered yet: stay out rather than injecting local apps as remote.
      return [];
    }
    const entries = [];
    for (const node of nodes) {
      if (!node || !node.online) continue;
      if (isLocalNode(node, hasLocalFlag)) continue;
      const panels = Array.isArray(node.panels) ? node.panels : [];
      const label = nodeLabel(node);
      // One whole-desktop entry per node: the remote desktop covers the
      // system panels (Settings / App Center) that stay out of injection.
      const gatewayBase = state.relayUrls.get(`gateway:${node.id}`);
      if (gatewayBase) {
        entries.push({
          entryKey: `fncluster:desktop.${node.id}`,
          source: 'fncluster',
          sourceName: 'FnCluster',
          appName: `fncluster.remote.desktop.${node.id}`,
          appID: `fncluster.remote.desktop.${node.id}`,
          entryName: `fncluster.remote.desktop.${node.id}`,
          title: `${desktopLabel()} · ${label}`,
          desc: `FnCluster remote desktop on ${label}`,
          icon: DESKTOP_ENTRY_ICON,
          type: 'iframe',
          uri: entryURI(gatewayBase),
          fullUrl: gatewayBase,
          microApp: false,
          nativeApp: false,
          isDisplay: true,
          status: 'available',
        });
      }
      if (!panels.length) continue;
      let count = 0;
      for (const panel of panels) {
        if (count >= REMOTE_ENTRY_LIMIT) break;
        // Poisoned catalogs (pre-0.2.3 feedback loop) may contain our own
        // synthetic keys; never treat them as real remote panels.
        if (!panel || !panel.id || String(panel.id).startsWith('fncluster:')) continue;
        if (excludedRemotePanel(panel)) continue;
        const relayURL = panel.relayable && panel.appName
          ? state.relayUrls.get(`${node.id}|${panel.appName}`)
          : '';
        let target = relayURL;
        if (!target && panel.kind === 'external') target = directPanelURL(node, panel);
        if (!target && panel.appName) {
          // The gateway relay tunnels the remote desktop same-site, so its
          // cookies work. The relay page hides the remote appview window
          // header, so the local task window is the only visible frame; the
          // theme travels along because relay storage cannot see ours.
          const gatewayBase = state.relayUrls.get(`gateway:${node.id}`);
          if (gatewayBase) {
            target = `${gatewayBase}?anchor=${encodeURIComponent(panel.appName)}&fncluster_theme=${themeMode()}`;
          }
        }
        if (!target) target = directPanelURL(node, panel);
        if (!target && panel.appName && node.webOrigin) {
          // Fallback for nodes that have not enabled the desktop integration:
          // a gateway-served root with an anchor query.
          const origin = String(node.webOrigin).replace(/\/+$/, '');
          target = `${origin}/?fncluster_anchor=${encodeURIComponent(panel.appName)}`;
        }
        if (!target) {
          target = `/app/fncluster/?node=${encodeURIComponent(node.id)}&panel=${encodeURIComponent(panel.id)}`;
        }
        const id = `${node.id}.${panel.id}`;
        entries.push({
          entryKey: `fncluster:${id}`,
          source: 'fncluster',
          sourceName: 'FnCluster',
          appName: `fncluster.remote.${id}`,
          appID: `fncluster.remote.${id}`,
          entryName: `fncluster.remote.${id}`,
          title: `${panel.title} · ${label}`,
          desc: `FnCluster remote ${panel.kind} panel on ${label}`,
          icon: remoteIconURL(node, panel),
          type: 'iframe',
          uri: entryURI(target),
          fullUrl: target,
          microApp: false,
          nativeApp: false,
          // The launchpad renders `e.hiddenDesktopIcon ? null : ...` and AJe
          // maps hiddenDesktopIcon = !isDisplay, so hidden entries never show;
          // desktop icons still come from getUserDesktop, not this list.
          isDisplay: true,
          status: 'available',
        });
        count += 1;
      }
    }
    return entries;
  }

  function installEntryInjection() {
    const socket = state.main;
    if (!isMainSocket(socket) || state.mainSocket === socket) return false;
    state.mainSocket = socket;
    // Run before the socket's own signing interceptor so reqid/req stay readable.
    const recorder = {
      fulfilled: event => {
        remember(state.mainPending, event && event.data);
        return event;
      },
      rejected: error => Promise.reject(error),
    };
    if (socket.interceptors && Array.isArray(socket.interceptors.request)) {
      socket.interceptors.request.unshift(recorder);
    } else {
      socket.useReq(event => {
        remember(state.mainPending, event && event.data);
        return event;
      });
    }
    socket.useRes(async event => {
      const message = event && event.data;
      if (!message || !message.reqid) return event;
      if (state.mainPending.get(message.reqid) !== ENTRY_LIST_REQ || !isSuccess(message.result)) return event;
      const data = message.data;
      if (!data || !Array.isArray(data.list)) return event;
      await ensureNodes();
      await ensureRelayUrls();
      const extra = remoteEntries();
      if (!extra.length) return event;
      const existing = new Set(data.list.map(entry => entry && entry.entryKey));
      const fresh = extra.filter(entry => !existing.has(entry.entryKey));
      if (!fresh.length) return event;
      data.list = data.list.concat(fresh);
      state.remoteEntries = fresh.length;
      console.log(`[FnCluster] injected ${fresh.length} remote entries into the desktop app list`);
      return event;
    });
    console.log('[FnCluster] all-apps injection enabled');
    return true;
  }

  // The desktop asks for its entry list once at load. If this bridge was not
  // installed in time - or the nodes payload was still empty - the synthetic
  // entries never made it into the store. Detect that and request one refetch
  // through the same domain event fnOS itself emits.
  function ensureEntriesVisible() {
    if (!state.mainSocket || publicPage()) return;
    const store = state.appsStore;
    if (store && Array.isArray(store.state.apps) && store.state.apps.some(isRemoteApp)) return;
    ensureNodes().then(() => {
      if (!state.mainSocket || !remoteEntries().length) return;
      state.mainSocket.emit('domainEvent', { domainEvent: { event: 'user-desktop-entry-change', from: 'trim-sac' } });
      console.log('[FnCluster] requested a desktop entry refresh for remote apps');
    });
  }

  // Native remote panels arrive here as /?fncluster_anchor=<appName> on the
  // remote desktop. A whole-page /appview load 404s at the fnOS gateway, but
  // client-side router navigation works, so push the appview route and fall
  // back to the desktop's own open command if the router ignores it.
  function waitForDesktopReady() {
    return new Promise(resolve => {
      const deadline = Date.now() + 10000;
      const check = () => {
        if (widgetRoot() || Date.now() > deadline) {
          resolve();
          return;
        }
        setTimeout(check, 500);
      };
      check();
    });
  }

  async function handleAnchor(mod) {
    let anchor = '';
    try {
      anchor = new URLSearchParams(window.location.search).get('fncluster_anchor') || '';
    } catch (_) {
      return;
    }
    if (!anchor) return;
    if (excludedRemotePanel({ appName: anchor.split('/')[0] })) return;
    try {
      if (sessionStorage.getItem('fncluster.anchor.handled') === anchor) return;
      sessionStorage.setItem('fncluster.anchor.handled', anchor);
    } catch (_) { /* private mode: still handle once per load */ }
    try {
      const url = new URL(window.location.href);
      url.searchParams.delete('fncluster_anchor');
      window.history.replaceState(window.history.state, '', url.toString());
    } catch (_) { /* ignore */ }

    await waitForDesktopReady();
    const path = `/appview?anchor=${encodeURIComponent(anchor)}`;
    try {
      window.history.pushState(window.history.state, '', path);
      window.dispatchEvent(new PopStateEvent('popstate'));
    } catch (_) { /* fall through to the command bus */ }
    setTimeout(() => {
      if (window.location.pathname === '/appview') return;
      if (mod && typeof mod.ms === 'function') {
        try {
          mod.ms(anchor);
        } catch (error) {
          console.warn('[FnCluster] anchor open failed', error);
        }
      }
    }, 1500);
  }

  // --- data polling ---
  async function poll() {
    try {
      const requests = [fetch(API_NODES, { credentials: 'same-origin' })];
      if (!state.localId) requests.push(fetch(API_MESH, { credentials: 'same-origin' }));
      const [nodesResponse, meshResponse] = await Promise.all(requests);
      if (nodesResponse.ok) {
        const nodes = await nodesResponse.json();
        if (Array.isArray(nodes)) state.nodes = nodes;
      }
      if (meshResponse && meshResponse.ok) {
        const mesh = await meshResponse.json();
        if (mesh && mesh.nodeId) state.localId = mesh.nodeId;
      }
    } catch (_) { /* desktop must keep working */ }
    if (overviewActive()) await loadOverviewItems();
    syncUI();
  }

  // The fnOS SPA renders login/setup/share routes from the same index.html.
  // Any fnOS API call there is unauthenticated, and the desktop's own main
  // socket interceptor reacts to errno 65534 with location.reload(), which
  // cancels an in-flight login submit. Stay completely inert until a real
  // desktop route appears.
  const publicPaths = ['/login', '/signin', '/system', '/sharing', '/reset-password', '/debug', '/404'];
  const publicPage = () => {
    const path = window.location.pathname.replace(/\/+$/, '') || '/';
    return publicPaths.some(publicPath => path === publicPath || path.endsWith(publicPath));
  };

  async function waitForDesktop() {
    console.log('[FnCluster] public page; waiting for a desktop route before enabling the bridge');
    await new Promise(resolve => {
      const check = () => {
        if (publicPage()) return;
        clearInterval(interval);
        window.removeEventListener('popstate', check);
        resolve();
      };
      const interval = setInterval(check, 1000);
      window.addEventListener('popstate', check);
    });
    await start();
  }

  async function start() {
    let mod = null;
    try {
      mod = await import(BUNDLE);
    } catch (_) {
      return;
    }
    const found = resolveExports(mod);
    state.api = found.api;
    state.main = isMainSocket(found.main) ? found.main : null;
    state.appsStore = isAppsStore(found.apps) ? found.apps : null;
    if (!isUsrd(state.api)) {
      console.log('[FnCluster] desktop widget bridge disabled: usrd export not found');
      return;
    }
    state.main ? installEntryInjection() : console.log('[FnCluster] main socket not found; all-apps injection disabled');
    installThemeBridge();
    // No debounce: reattach never renders, so running at the mutation
    // microtask keeps the overview attached in the same frame as React's
    // commit and cannot loop.
    state.observer = new MutationObserver(() => reattach(layout()));
    loadMode();
    await poll();
    reportPanels();
    state.timer = setInterval(poll, POLL_MS);
    state.panelsTimer = setInterval(reportPanels, PANELS_REPORT_MS);
    setTimeout(ensureEntriesVisible, 1500);
    setTimeout(ensureEntriesVisible, 6000);
    window.addEventListener('resize', syncUI);
    window.__fnclusterWidgetDebug = {
      get state() {
        return {
          mode: state.mode,
          label: currentLabel(),
          panel: !!state.panel && state.panel.style.display !== 'none',
          embedded: {
            button: !!(state.button && state.button.parentElement),
            menu: !!(state.menu && state.menu.parentElement),
            panel: !!(state.panel && state.panel.parentElement),
          },
          localId: state.localId,
          mainSocket: !!state.mainSocket,
          remoteEntries: state.remoteEntries,
          nodes: state.nodes.map(node => ({ id: node.id, name: node.name, online: node.online })),
        };
      },
      refresh: () => poll(),
    };
    handleAnchor(mod);
  }

  async function setup() {
    // Inside the gateway relay the page already is the remote desktop: running
    // the bridge there would inject recursive entries whose tokens and relay
    // origins cross nodes. Stay inert.
    if (window.__fnclusterRelay) {
      console.log('[FnCluster] relay frame detected; widget bridge disabled');
      return;
    }
    if (!document.querySelector(`script[src="${BUNDLE}"]`)) return; // verified build gate
    if (publicPage()) {
      await waitForDesktop();
      return;
    }
    await start();
  }

  if (document.readyState === 'complete') setup();
  else window.addEventListener('load', setup, { once: true });
})();
