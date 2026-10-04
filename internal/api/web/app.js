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

function renderMesh(mesh) {
  const configured = Boolean(mesh.configured);
  $('mesh-setup').classList.toggle('hidden', configured);
  $('mesh-join').classList.toggle('hidden', configured);
  $('mesh-members').classList.toggle('hidden', !configured);
  $('mesh-token').classList.toggle('hidden', !(configured && mesh.isController));
  $('mesh-leave').classList.toggle('hidden', !configured || mesh.isController);
  $('mesh-status').classList.remove('error');
  if (!configured) {
    $('mesh-status').textContent = '本机尚未加入集群：可以创建新集群，或使用另一台生成的配对码加入。';
    return;
  }
  if (mesh.localRevoked) {
    $('mesh-status').textContent = `本机已被集群创建者移除（节点 ${short(mesh.nodeId)}）。重新加入：在创建者处生成新配对码 → 展开下方“高级 / 调试”重置节点身份 → 用新配对码加入。`;
    $('mesh-status').classList.add('error');
    return;
  }
  const running = mesh.running ? `运行中 ${mesh.listen || ''}` : '未运行';
  $('mesh-status').textContent = `集群 ${short(mesh.clusterId)} · 本节点 ${short(mesh.nodeId)} · 控制器 ${short(mesh.controllerId)} · mTLS ${running}`;
  const list = $('mesh-member-list');
  const rows = Object.entries(mesh.members || {}).sort((a, b) => a[0].localeCompare(b[0]));
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
  const revokedIds = Object.keys(mesh.revoked || {});
  if (revokedIds.length) {
    const item = document.createElement('li');
    item.className = 'member revoked';
    item.textContent = `已吊销：${revokedIds.map(short).join('、')}`;
    items.push(item);
  }
  list.replaceChildren(...items);
}

function renderWebUI(webui) {
  const suffix = webui.integrationConfigured ? '' : '（未配置注入路径）';
  $('webui-status').textContent = `${webui.status}${suffix}${webui.compatible ? ' · 未登记版本，可兼容模式启用' : ''}`;
  $('webui-apply-compatible').classList.toggle('hidden', !webui.compatible);
  $('audit-path').textContent = webui.auditLog || '—';
  const archive = webui.archive || {};
  const emergency = archive.emergency ? ' · 应急备份已就位' : '';
  $('archive-hint').textContent = archive.path
    ? `fnOS 还原包：${archive.path}${archive.backedUp ? '（已备份原包，重启后仍生效）' : '（原包未备份）'}${emergency}`
    : 'fnOS 还原包：未同步（fnOS 可能还原补丁）';
}

async function loadLogs() {
  try {
    const source = $('log-source').value;
    const data = await api(`api/v1/logs?source=${encodeURIComponent(source)}&lines=200`);
    const lines = data.lines || [];
    $('log-output').textContent = lines.length ? lines.join('\n') : '暂无日志';
  } catch (error) {
    $('log-output').textContent = `日志读取失败：${error.message}`;
  }
}

async function removeNode(id) {
  if (!confirm(`确定移除节点 ${id}？其证书将立即失效，且不能再用同一节点 ID 加入。`)) return;
  try {
    await api('api/v1/mesh/remove', { method: 'POST', body: { nodeId: id } });
    await refresh();
  } catch (error) {
    showMeshError(error);
  }
}

async function refresh() {
  try {
    const [clusterResponse, nodesResponse] = await Promise.all([
      fetch('api/v1/cluster'), fetch('api/v1/nodes')
    ]);
    if (!clusterResponse.ok || !nodesResponse.ok) throw new Error('API 请求失败');
    const cluster = await clusterResponse.json();
    const nodes = await nodesResponse.json();
    $('status').textContent = cluster.status === 'healthy' ? '运行正常' : '节点异常';
    $('status').classList.toggle('error', cluster.status !== 'healthy');
    $('nodes').textContent = `${cluster.nodes.online} / ${cluster.nodes.total}`;
    $('storage').textContent = cluster.storage.totalBytes
      ? `${bytes(cluster.storage.usedBytes)} / ${bytes(cluster.storage.totalBytes)}`
      : '未配置存储挂载点';
    $('node-list').replaceChildren(...nodes.map(node => {
      const card = document.createElement('article');
      card.className = 'node';
      const title = document.createElement('h3');
      title.textContent = `${node.name} · ${node.online ? '在线' : '离线'}`;
      const info = document.createElement('p');
      info.textContent = `fnOS ${node.fnosVersion} · ${node.architecture}`;
      const metrics = document.createElement('p');
      metrics.textContent = node.metrics.collectedAt
        ? `CPU ${node.metrics.cpuPercent == null ? '—' : `${node.metrics.cpuPercent.toFixed(1)}%`} · 内存 ${bytes(node.metrics.memoryUsedBytes)} / ${bytes(node.metrics.memoryTotalBytes)} · 负载 ${(node.metrics.load || []).map(n => n.toFixed(2)).join(' / ')} · 运行 ${uptime(node.metrics.uptimeSeconds)}`
        : '等待 Linux 指标';
      const volumes = document.createElement('p');
      const filesystems = node.metrics.filesystems || [];
      volumes.textContent = filesystems.length
        ? `本节点存储：${filesystems.map(fs => `${fs.mount} ${bytes(fs.usedBytes)} / ${bytes(fs.totalBytes)}`).join(' · ')}`
        : '本节点存储：未发现 fnOS 数据卷';
      const network = document.createElement('p');
      const counters = Object.entries(node.metrics.network || {}).filter(([name]) => name !== 'lo');
      const rx = counters.reduce((sum, [, item]) => sum + (item.receiveBytes || 0), 0);
      const tx = counters.reduce((sum, [, item]) => sum + (item.transmitBytes || 0), 0);
      network.textContent = counters.length ? `网络累计：接收 ${bytes(rx)} · 发送 ${bytes(tx)}` : '网络：暂无数据';
      const apps = document.createElement('p');
      const panels = node.panels || [];
      apps.textContent = panels.length
        ? `应用与面板：${panels.map(panel => `${panel.title}${panel.native ? '（原生）' : ''}`).join(' · ')}`
        : '应用与面板：未发现';
      card.append(title, info, metrics, volumes, network, apps);
      return card;
    }));
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
  '确定退出集群？本机将断开与其他节点的连接。'
));

$('mesh-reset').addEventListener('click', () => runAction(
  $('mesh-reset'),
  () => api('api/v1/mesh/reset-identity', { method: 'POST' }),
  '重置节点身份？将删除本机 mesh.json 与 node-id。创建者上仍保留旧成员记录，需要在那里移除；其他节点的数据和存储不受影响。'
));

$('webui-apply').addEventListener('click', () => runAction(
  $('webui-apply'),
  () => api('api/v1/webui/apply', { method: 'POST' }),
  '启用飞牛桌面集成？将备份并修改 /usr/trim/www/index.html，健康检查失败会自动恢复。'
));

$('webui-apply-compatible').addEventListener('click', () => runAction(
  $('webui-apply-compatible'),
  () => api('api/v1/webui/apply', { method: 'POST', body: { compatible: true } }),
  '当前页面不是已登记的 fnOS 版本。\n\n兼容模式会在结构校验（唯一的 </head> 与 <div id="root">）通过后修改 /usr/trim/www/index.html。仍然会备份原文件、健康检查，失败自动恢复。\n\n确定继续？'
));

$('webui-restore').addEventListener('click', () => runAction(
  $('webui-restore'),
  () => api('api/v1/webui/restore', { method: 'POST' }),
  '恢复原始飞牛桌面？将移除 FnCluster 注入并进入安全模式。'
));

$('webui-enable').addEventListener('click', () => runAction(
  $('webui-enable'),
  () => api('api/v1/webui/enable', { method: 'POST' }),
  '解除安全模式？仅在确认当前页面为受支持的 fnOS 版本后继续。'
));

$('log-refresh').addEventListener('click', loadLogs);
$('log-source').addEventListener('change', loadLogs);

loadInterfaces().catch(showMeshError);
loadLogs();
refresh();
setInterval(refresh, 5000);
setInterval(loadLogs, 10000);
