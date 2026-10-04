/* FnCluster desktop widget bridge for the verified fnOS 1.2.0701 build.
   Adds a node selector next to the native desktop widget's display settings
   with two choices: the local native cards, or the cluster overview panel.
   The injected nodes live inside the native widget DOM (same stacking and
   clipping as the expanded window) and a MutationObserver re-attaches them if
   React reconciliation ever removes them. Browser-only. */
(() => {
  if (window.__fnclusterWidget) return;
  window.__fnclusterWidget = true;

  const BUNDLE = '/assets/index-DhUqaTaA.js';
  const API_NODES = '/app/fncluster/api/v1/nodes';
  const STORAGE_KEY = 'fncluster.widget.mode';
  const POLL_MS = 500;
  const COLLAPSED_HEIGHT = 80;

  const state = {
    api: null,
    nodes: [],
    mode: 'local',
    timer: null,
    button: null,
    menu: null,
    panel: null,
    observer: null,
    observedWrapper: null,
  };

  const isUsrd = value => !!value && value.usrd && typeof value.usrd.get === 'function' && !!value.sysinfo && typeof value.sysinfo.getUptime === 'function';

  // Minified export names are build-specific; known name first, then feature scan.
  function resolveApi(mod) {
    let api = mod.Hc;
    if (!isUsrd(api)) api = Object.values(mod || {}).find(isUsrd) || api;
    return api;
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
  }

  // --- data polling ---
  async function poll() {
    try {
      const response = await fetch(API_NODES, { credentials: 'same-origin' });
      if (response.ok) {
        const nodes = await response.json();
        if (Array.isArray(nodes)) state.nodes = nodes;
      }
    } catch (_) { /* desktop must keep working */ }
    if (overviewActive()) await loadOverviewItems();
    syncUI();
  }

  async function setup() {
    if (!document.querySelector(`script[src="${BUNDLE}"]`)) return; // verified build gate
    let mod = null;
    try {
      mod = await import(BUNDLE);
    } catch (_) {
      return;
    }
    state.api = resolveApi(mod);
    if (!isUsrd(state.api)) {
      console.log('[FnCluster] desktop widget bridge disabled: usrd export not found');
      return;
    }
    // No debounce: reattach never renders, so running at the mutation
    // microtask keeps the overview attached in the same frame as React's
    // commit and cannot loop.
    state.observer = new MutationObserver(() => reattach(layout()));
    loadMode();
    await poll();
    state.timer = setInterval(poll, POLL_MS);
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
          nodes: state.nodes.map(node => ({ id: node.id, name: node.name, online: node.online })),
        };
      },
      refresh: () => poll(),
    };
  }

  if (document.readyState === 'complete') setup();
  else window.addEventListener('load', setup, { once: true });
})();
