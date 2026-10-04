/* fnOS 1.2.0701: read-only status badge. Keep this file small and removable. */
(() => {
  const endpoint = '/app/fncluster/api/v1/cluster';
  const id = 'fncluster-status';
  const storageId = 'fncluster-storage';
  // The bundle's own auth redirect (qUe) treats these routes as desktop-less.
  const publicPaths = ['/login', '/signin', '/system', '/sharing', '/reset-password', '/debug', '/404'];
  const publicPage = () => {
    const path = window.location.pathname.replace(/\/+$/, '') || '/';
    return publicPaths.some(publicPath => path === publicPath || path.endsWith(publicPath));
  };
  const size = value => {
    if (!Number.isFinite(value)) return '—';
    if (value >= 2 ** 40) return `${(value / 2 ** 40).toFixed(1)} TB`;
    if (value >= 2 ** 30) return `${(value / 2 ** 30).toFixed(1)} GB`;
    return `${(value / 2 ** 20).toFixed(0)} MB`;
  };
  async function mount() {
    if (!document.querySelector('script[src="/assets/index-DhUqaTaA.js"]')) return;
    if (!document.getElementById('fncluster-widget-script')) {
      const widget = document.createElement('script');
      widget.id = 'fncluster-widget-script';
      widget.src = '/app/fncluster/inject/widget.js?v=0.2.5';
      widget.defer = true;
      document.head.append(widget);
    }
    if (publicPage()) {
      document.getElementById(id)?.remove();
      return;
    }
    let summary;
    try {
      const response = await fetch(endpoint, { credentials: 'same-origin' });
      if (!response.ok) return;
      summary = await response.json();
    } catch { return; }
    if (!summary?.nodes || !document.body) return;
    let badge = document.getElementById(id);
    if (!badge) {
      badge = document.createElement('button');
      badge.id = id;
      badge.type = 'button';
      badge.style.cssText = 'position:fixed;right:16px;bottom:16px;z-index:2147483000;padding:8px 12px;border:0;border-radius:8px;background:#1476d4;color:white;box-shadow:0 2px 8px #0003;cursor:pointer';
      badge.addEventListener('click', () => window.open('/app/fncluster/', '_blank', 'noopener'));
      document.body.append(badge);
    }
    let indicator = badge.querySelector('.fncluster-indicator');
    if (!indicator) {
      indicator = document.createElement('span');
      indicator.className = 'fncluster-indicator';
      badge.append(indicator);
    }
    indicator.textContent = `Cluster ${summary.nodes.online}/${summary.nodes.total}${summary.status === 'healthy' ? '' : ' !'}`;
    let storage = document.getElementById(storageId);
    if (!storage) {
      storage = document.createElement('span');
      storage.id = storageId;
      storage.style.cssText = 'display:block;font-size:11px;opacity:.9;margin-top:3px';
      badge.append(storage);
    }
    storage.textContent = summary.storage?.totalBytes
      ? `集群总存储 ${size(summary.storage.usedBytes)} / ${size(summary.storage.totalBytes)}`
      : '集群总存储：未配置';
    badge.setAttribute('aria-label', `FnCluster 在线节点 ${summary.nodes.online}/${summary.nodes.total}${summary.status === 'healthy' ? '' : '，状态异常'}`);
  }
  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', mount, { once: true });
  else mount();
  setInterval(mount, 15000);
})();
