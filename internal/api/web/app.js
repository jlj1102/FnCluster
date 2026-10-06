const $ = id => document.getElementById(id);
const bytes = n => {
  if (!Number.isFinite(n)) return '—';
  if (n >= 2 ** 40) return `${(n / 2 ** 40).toFixed(2)} TB`;
  if (n >= 2 ** 30) return `${(n / 2 ** 30).toFixed(1)} GB`;
  return `${(n / 2 ** 20).toFixed(1)} MB`;
};
const uptime = seconds => Number.isFinite(seconds) && seconds > 0
  ? `${Math.floor(seconds / 86400)}天 ${Math.floor(seconds % 86400 / 3600)}小时`
  : '—';
const short = id => id ? `${id.slice(0, 8)}…` : '—';
let targetScrolled = false;

// Only touch classes/attributes when the value actually changes: the poll
// runs every few seconds and unconditional toggles made controls blink.
function setHidden(element, hidden) {
  if (element.classList.contains('hidden') !== hidden) element.classList.toggle('hidden', hidden);
}

/* ---------- theme: follow the main WebUI / relay frame / OS ---------- */

function parentTheme() {
  try {
    if (!window.parent || window.parent === window) return null;
    const parentDocument = window.parent.document;
    const body = parentDocument.body;
    if (body) {
      const attr = body.getAttribute('theme-mode');
      if (attr) return attr === 'dark' ? 'dark' : 'light';
      return parentDocument.documentElement.classList.contains('dark') ? 'dark' : 'light';
    }
  } catch (_) { /* cross-origin parent */ }
  return null;
}

function themeMode() {
  const inherited = parentTheme();
  if (inherited) return inherited;
  const own = document.body.getAttribute('theme-mode');
  if (own) return own === 'dark' ? 'dark' : 'light';
  return window.matchMedia && window.matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light';
}

function applyTheme() {
  const dark = themeMode() === 'dark';
  document.documentElement.classList.toggle('dark', dark);
  document.documentElement.classList.toggle('light', !dark);
  if (dark) document.body.setAttribute('theme-mode', 'dark');
  else document.body.setAttribute('theme-mode', 'light');
}

applyTheme();
setInterval(() => { if (!document.hidden) applyTheme(); }, 1000);
document.addEventListener('visibilitychange', () => { if (!document.hidden) applyTheme(); });

/* ---------- navigation ---------- */

const views = {
  overview: ['总览', '集群状态与容量'],
  nodes: ['节点', '各节点状态、存储与应用'],
  cluster: ['集群', '组建与管理集群'],
  integration: ['桌面集成', '把 FnCluster 放进飞牛桌面'],
  logs: ['日志', '服务与审计日志'],
  about: ['关于', '说明与高级操作'],
};

function switchView(name) {
  if (!views[name]) return;
  for (const item of document.querySelectorAll('.nav-item')) {
    item.classList.toggle('active', item.dataset.view === name);
  }
  for (const view of document.querySelectorAll('.view')) {
    setHidden(view, view.dataset.view !== name);
  }
  $('view-title').textContent = views[name][0];
  $('view-desc').textContent = views[name][1];
}

$('fncluster-nav').addEventListener('click', event => {
  const item = event.target.closest('.nav-item');
  if (item) switchView(item.dataset.view);
});

// A remote panel entry opens /app/fncluster/?node=...&panel=...; show the node
// list and highlight the card the click landed on.
function focusTarget() {
  const params = new URLSearchParams(location.search);
  const nodeId = params.get('node');
  if (!nodeId) return;
  switchView('nodes');
  const card = document.querySelector(`.node[data-node-id="${CSS.escape(nodeId)}"]`);
  if (!card) return;
  card.classList.add('target');
  const panelId = params.get('panel');
  if (panelId) {
    for (const chip of card.querySelectorAll('.panel-chip')) {
      if (chip.dataset.panelId === panelId) {
        chip.classList.add('target');
        break;
      }
    }
  }
  if (!targetScrolled) {
    targetScrolled = true;
    card.scrollIntoView({ behavior: 'smooth', block: 'center' });
  }
}

/* ---------- api helpers ---------- */

async function api(path, options = {}) {
  const method = options.method || 'GET';
  const init = { method, credentials: 'same-origin', headers: {} };
  if (method !== 'GET') {
    init.headers['Content-Type'] = 'application/json';
    init.headers['X-FnCluster'] = '1';
    init.body = JSON.stringify(options.body || {});
  }
  const response = await fetch(path, init);
  const text = await response.text();
  let data = null;
  if (text) {
    try { data = JSON.parse(text); } catch { data = null; }
  }
  if (!response.ok) throw new Error((data && data.error) || `HTTP ${response.status}`);
  return data;
}

function showMeshError(error) {
  const box = $('mesh-error');
  box.textContent = error.message;
  box.classList.remove('hidden');
  switchView('cluster');
}

async function runAction(button, task, confirmText) {
  if (confirmText && !confirm(confirmText)) return;
  button.disabled = true;
  $('mesh-error').classList.add('hidden');
  try {
    await task();
    await refresh();
  } catch (error) {
    showMeshError(error);
  } finally {
    button.disabled = false;
  }
}

async function loadInterfaces() {
  const data = await api('api/v1/net/interfaces');
  const port = data.port || '38671';
  const addresses = data.addresses || [];
  const recommended = (addresses.find(item => item.recommended) || addresses[0]);
  const preferred = recommended ? `${recommended.ip}:${port}` : '';
  for (const select of [$('mesh-address'), $('mesh-address-join')]) {
    select.replaceChildren(...addresses.map(item => {
      const option = document.createElement('option');
      option.value = `${item.ip}:${port}`;
      option.textContent = `${item.ip}:${port}`;
      return option;
    }));
    if (preferred) select.value = preferred;
  }
}

/* ---------- cluster (mesh) ---------- */

let memberSignature = '';

function renderMembers(mesh) {
  const rows = Object.entries(mesh.members || {}).sort((a, b) => a[0].localeCompare(b[0]));
  const revokedIds = Object.keys(mesh.revoked || {});
  const signature = JSON.stringify([rows, revokedIds, mesh.isController, mesh.nodeId]);
  if (signature === memberSignature) return;
  memberSignature = signature;
  const items = rows.map(([id, address]) => {
    const item = document.createElement('li');
    item.className = 'member';
    const text = document.createElement('span');
    text.textContent = `${short(id)}${id === mesh.nodeId ? '（本机）' : ''} ${address}`;
    item.append(text);
    if (mesh.isController && id !== mesh.nodeId) {
      const button = document.createElement('button');
      button.type = 'button';
      button.className = 'danger small';
      button.textContent = '移除';
      button.addEventListener('click', () => removeNode(id));
      item.append(button);
    }
    return item;
  });
  if (revokedIds.length) {
    const item = document.createElement('li');
    item.className = 'member revoked';
    item.textContent = `已吊销：${revokedIds.map(short).join('、')}`;
    items.push(item);
  }
  if (!items.length) {
    const item = document.createElement('li');
    item.className = 'member revoked';
    item.textContent = '暂无成员';
    items.push(item);
  }
  $('mesh-member-list').replaceChildren(...items);
}

function renderMesh(mesh) {
  const configured = Boolean(mesh.configured);
  setHidden($('mesh-setup-card'), configured);
  setHidden($('mesh-join-card'), configured);
  setHidden($('mesh-token-card'), !(configured && mesh.isController));
  $('mesh-status').classList.remove('error');

  // The leave control is persistent: disabled with an explanation instead of
  // appearing/disappearing with each poll.
  const leave = $('mesh-leave');
  leave.disabled = !configured || Boolean(mesh.isController);
  $('mesh-leave-hint').textContent = !configured
    ? '未加入集群'
    : mesh.isController
      ? '创建者不能退出（解散集群未实现）'
      : '';

  if (!configured) {
    memberSignature = '';
    renderMembers({ ...mesh, members: {}, revoked: {} });
    $('mesh-status').textContent = '尚未加入集群。';
    return;
  }
  if (mesh.localRevoked) {
    $('mesh-status').textContent = '本机已被移出集群。请到创建者处生成新配对码，再到「关于」重置节点身份后重新加入。';
    $('mesh-status').classList.add('error');
    renderMembers(mesh);
    return;
  }
  const running = mesh.running ? `运行中 ${mesh.listen || ''}` : '未运行';
  $('mesh-status').textContent = `集群 ${short(mesh.clusterId)} · 本节点 ${short(mesh.nodeId)} · 控制器 ${short(mesh.controllerId)} · ${running}`;
  $('mesh-status').title = '节点间连接使用 mTLS。';
  renderMembers(mesh);
}

const webuiStatusText = {
  PATCHED: '已启用',
  UNPATCHED: '未启用',
  SAFE_MODE: '安全模式',
  BROKEN: '注入异常',
  OUTDATED: '飞牛已更新，需重新启用',
  UNSUPPORTED: '版本不匹配',
};

function renderWebUI(webui) {
  const state = webuiStatusText[webui.status] || webui.status;
  const suffix = webui.integrationConfigured ? '' : '（未配置注入路径）';
  $('webui-status').textContent = `${state}${suffix}${webui.compatible ? ' · 未登记版本，可兼容模式启用' : ''}`;
  setHidden($('webui-apply-compatible'), !webui.compatible);
  $('audit-path').textContent = webui.auditLog || '—';
  const archive = webui.archive || {};
  const emergency = archive.emergency ? ' · 应急备份已就位' : '';
  $('archive-hint').textContent = archive.path
    ? `还原包：${archive.path}${archive.backedUp ? '（已备份，重启后仍生效）' : '（未备份）'}${emergency}`
    : '还原包未同步，飞牛更新可能覆盖补丁';
  const fndesk = webui.fndesk || {};
  if (fndesk.detected) {
    $('fndesk-hint').textContent = '检测到 FnDesk：用兼容模式启用可与它共存。';
    $('fndesk-hint').title = (fndesk.evidence || []).join('、');
  }
  setHidden($('fndesk-hint'), !fndesk.detected);
}

async function loadLogs() {
  try {
    const source = $('log-source').value;
    const data = await api(`api/v1/logs?source=${encodeURIComponent(source)}&lines=200`);
    const lines = data.lines || [];
    $('log-output').textContent = lines.length ? lines.join('\n') : '暂无日志';
  } catch (error) {
    $('log-output').textContent = `读取失败：${error.message}`;
  }
}

async function removeNode(id) {
  if (!confirm(`移除 ${id}？它的证书会立即失效。`)) return;
  try {
    await api('api/v1/mesh/remove', { method: 'POST', body: { nodeId: id } });
    await refresh();
  } catch (error) {
    showMeshError(error);
  }
}

/* ---------- polling ---------- */

async function refresh() {
  try {
    const [clusterResponse, nodesResponse] = await Promise.all([
      fetch('api/v1/cluster'), fetch('api/v1/nodes')
    ]);
    if (!clusterResponse.ok || !nodesResponse.ok) throw new Error('API 请求失败');
    const cluster = await clusterResponse.json();
    const nodes = await nodesResponse.json();
    $('status').textContent = cluster.status === 'healthy' ? '运行正常' : '有节点离线';
    $('status').classList.toggle('error', cluster.status !== 'healthy');
    $('nodes').textContent = `${cluster.nodes.online} / ${cluster.nodes.total}`;
    $('storage').textContent = cluster.storage.totalBytes
      ? `${bytes(cluster.storage.usedBytes)} / ${bytes(cluster.storage.totalBytes)}`
      : '未配置存储挂载点';
    $('node-list').replaceChildren(...nodes.map(node => {
      const card = document.createElement('article');
      card.className = 'node';
      card.dataset.nodeId = node.id;
      const title = document.createElement('h3');
      title.textContent = `${node.name} · ${node.online ? '在线' : '离线'}`;
      const info = document.createElement('p');
      info.textContent = `fnOS ${node.fnosVersion} · ${node.architecture}`;
      const metrics = document.createElement('p');
      metrics.textContent = node.metrics.collectedAt
        ? `CPU ${node.metrics.cpuPercent == null ? '—' : `${node.metrics.cpuPercent.toFixed(1)}%`} · 内存 ${bytes(node.metrics.memoryUsedBytes)} / ${bytes(node.metrics.memoryTotalBytes)} · 负载 ${(node.metrics.load || []).map(n => n.toFixed(2)).join(' / ')} · 运行 ${uptime(node.metrics.uptimeSeconds)}`
        : '指标加载中…';
      const volumes = document.createElement('p');
      const filesystems = node.metrics.filesystems || [];
      volumes.textContent = filesystems.length
        ? filesystems.map(fs => `${fs.mount} ${bytes(fs.usedBytes)} / ${bytes(fs.totalBytes)}`).join(' · ')
        : '未发现数据卷';
      const network = document.createElement('p');
      const counters = Object.entries(node.metrics.network || {}).filter(([name]) => name !== 'lo');
      const rx = counters.reduce((sum, [, item]) => sum + (item.receiveBytes || 0), 0);
      const tx = counters.reduce((sum, [, item]) => sum + (item.transmitBytes || 0), 0);
      network.textContent = counters.length ? `网络累计：接收 ${bytes(rx)} · 发送 ${bytes(tx)}` : '暂无网络数据';
      const panels = document.createElement('div');
      panels.className = 'panels';
      const panelList = node.panels || [];
      for (const [label, kind] of [['系统面板', 'system'], ['应用', 'application'], ['外部', 'external']]) {
        const group = panelList.filter(panel => panel.kind === kind);
        if (!group.length) continue;
        const line = document.createElement('div');
        line.className = 'panel-line';
        const name = document.createElement('span');
        name.className = 'panel-kind';
        name.textContent = label;
        line.append(name);
        for (const panel of group) {
          const chip = document.createElement('span');
          chip.className = 'panel-chip';
          chip.dataset.panelId = panel.id;
          chip.textContent = panel.title;
          if (panel.native) chip.title = '原生面板';
          line.append(chip);
        }
        panels.append(line);
      }
      if (!panels.children.length) {
        panels.textContent = '未发现应用';
      } else if (node.panelsSource === 'scan') {
        const hint = document.createElement('div');
        hint.className = 'panel-hint';
        hint.textContent = '来自扫描，打开该节点桌面后更完整';
        panels.append(hint);
      }
      card.append(title, info, metrics, volumes, network, panels);
      return card;
    }));
    focusTarget();
  } catch (error) {
    $('status').textContent = error.message;
    $('status').classList.add('error');
  }
  try {
    const [mesh, webui] = await Promise.all([api('api/v1/mesh'), api('api/v1/webui')]);
    renderMesh(mesh);
    renderWebUI(webui);
  } catch (error) {
    showMeshError(error);
  }
}

/* ---------- actions ---------- */

$('mesh-init').addEventListener('click', () => {
  const advertise = $('mesh-address').value;
  if (!advertise) return showMeshError(new Error('请选择本机地址'));
  runAction($('mesh-init'), () => api('api/v1/mesh/init', { method: 'POST', body: { advertise } }));
});

$('mesh-token-btn').addEventListener('click', () => runAction($('mesh-token-btn'), async () => {
  const result = await api('api/v1/mesh/token', { method: 'POST' });
  $('mesh-code-out').value = result.code;
}));

$('mesh-copy').addEventListener('click', async () => {
  const value = $('mesh-code-out').value;
  if (!value) return;
  try {
    await navigator.clipboard.writeText(value);
  } catch {
    $('mesh-code-out').select();
    document.execCommand('copy');
  }
  $('mesh-copy').textContent = '已复制';
  setTimeout(() => { $('mesh-copy').textContent = '复制'; }, 1500);
});

$('mesh-join-btn').addEventListener('click', () => {
  const seed = $('mesh-seed').value.trim();
  const code = $('mesh-code').value.trim();
  const advertise = $('mesh-address-join').value;
  if (!seed || !code || !advertise) return showMeshError(new Error('请填写种子地址、配对码和本机地址'));
  runAction($('mesh-join-btn'), () => api('api/v1/mesh/join', { method: 'POST', body: { seed, code, advertise } }));
});

$('mesh-leave').addEventListener('click', () => runAction(
  $('mesh-leave'),
  () => api('api/v1/mesh/leave', { method: 'POST' }),
  '退出集群？本机会断开与其他节点的连接。'
));

$('mesh-reset').addEventListener('click', () => runAction(
  $('mesh-reset'),
  () => api('api/v1/mesh/reset-identity', { method: 'POST' }),
  '重置节点身份？会删除本机的集群状态与节点 ID。'
));

$('webui-apply').addEventListener('click', () => runAction(
  $('webui-apply'),
  () => api('api/v1/webui/apply', { method: 'POST' }),
  '启用桌面集成？会备份并修改飞牛桌面文件，失败自动回滚。'
));

$('webui-apply-compatible').addEventListener('click', () => runAction(
  $('webui-apply-compatible'),
  () => api('api/v1/webui/apply', { method: 'POST', body: { compatible: true } }),
  '当前不是已登记版本。继续以兼容模式启用？失败会自动回滚。'
));

$('webui-restore').addEventListener('click', () => runAction(
  $('webui-restore'),
  () => api('api/v1/webui/restore', { method: 'POST' }),
  '恢复原始桌面？会移除注入并进入安全模式。'
));

$('webui-enable').addEventListener('click', () => runAction(
  $('webui-enable'),
  () => api('api/v1/webui/enable', { method: 'POST' }),
  '解除安全模式？请确认当前飞牛版本受支持。'
));

const badgeHiddenKey = 'fncluster.badge.hidden';

function badgeHidden() {
  try { return localStorage.getItem(badgeHiddenKey) === '1'; } catch { return false; }
}

function syncBadgeButton() {
  $('webui-badge-show').textContent = badgeHidden() ? '显示桌面角标' : '隐藏桌面角标';
}

$('webui-badge-show').addEventListener('click', () => {
  try {
    badgeHidden() ? localStorage.removeItem(badgeHiddenKey) : localStorage.setItem(badgeHiddenKey, '1');
  } catch { /* ignore */ }
  syncBadgeButton();
});

syncBadgeButton();

$('log-refresh').addEventListener('click', loadLogs);
$('log-source').addEventListener('change', loadLogs);

loadInterfaces().catch(showMeshError);
loadLogs();
refresh();
setInterval(refresh, 5000);
setInterval(loadLogs, 10000);
