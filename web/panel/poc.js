/* FnCluster research PoC for fnOS 1.2.0701. Read-only, DevTools only.
   Not shipped in the FPK and not for production use.
   No payloads, tokens or credentials are logged.
   Paste into the desktop console, then call __fnclusterPoc.*; see docs/DEVICE_VALIDATION.md. */
(() => {
  if (window.__fnclusterPoc) {
    console.log('[FnCluster] PoC already installed');
    return;
  }
  const originalSend = WebSocket.prototype.send;
  const seen = new Map();
  WebSocket.prototype.send = function (data) {
    try {
      if (typeof data === 'string') {
        const start = data.indexOf('{');
        const envelope = JSON.parse(start >= 0 ? data.slice(start) : data);
        const meta = seen.get(this) || { url: this.url, count: 0, lastReq: '' };
        meta.count += 1;
        meta.lastReq = envelope.req;
        seen.set(this, meta);
        console.log('[FnCluster] send', this.url, envelope.req);
      }
    } catch (_) {
      // Passive hook. Never break fnOS send().
    }
    return Reflect.apply(originalSend, this, [data]);
  };
  const bundleURL = () => {
    const found = [...document.scripts].map(script => script.src).find(src => /\/assets\/index-[^/]+\.js$/.test(src));
    if (!found) throw new Error('fnOS entry bundle not found; unsupported build');
    if (!/index-DhUqaTaA\.js$/.test(found)) console.warn('[FnCluster] unexpected entry bundle; results are build-specific:', found);
    return found;
  };
  const waitOpen = (socket, timeout = 5000) => new Promise(resolve => {
    const started = Date.now();
    const tick = () => {
      if (socket.status === 1) return resolve(true);
      if (Date.now() - started > timeout) return resolve(false);
      setTimeout(tick, 50);
    };
    tick();
  });
  const api = {
    sockets() {
      return [...seen.values()];
    },
    async bundle() {
      const mod = await import(bundleURL());
      const wanted = ['Vd', 'Kc', 'qc', 'Gc', 'Hc', 'Dd', 'sl'];
      console.log('[FnCluster] bundle exports', Object.fromEntries(wanted.map(key => [key, typeof mod[key]])));
      return mod;
    },
    async preflight(url) {
      const mod = await api.bundle();
      const socket = new mod.Vd(url, { timeout: -1, isMain: true, genReqId: mod.Dd });
      try {
        if (!await waitOpen(socket)) return { ok: false, error: 'socket did not open' };
        const response = await socket.send({ req: 'util.crypto.getRSAPub' }, { skipPreflight: true, timeout: 5000 });
        return { ok: true, hasPub: Boolean(response && response.pub), hasSi: Boolean(response && response.si) };
      } catch (error) {
        return { ok: false, code: error && error.code, result: error && error.result, error: error && error.errmsg };
      } finally {
        try { socket.close(); } catch (_) { /* research cleanup */ }
      }
    },
    probe(url) {
      return new Promise(resolve => {
        const socket = new WebSocket(url);
        const result = { url, opened: false, closed: false, code: undefined, error: false, firstMessage: false };
        const done = () => {
          clearTimeout(timer);
          try { socket.close(); } catch (_) { /* research cleanup */ }
          resolve(result);
        };
        const timer = setTimeout(done, 5000);
        socket.onopen = () => { result.opened = true; };
        socket.onmessage = () => { result.firstMessage = true; done(); };
        socket.onerror = () => { result.error = true; };
        socket.onclose = event => { result.closed = true; result.code = event.code; done(); };
      });
    },
    uninstall() {
      WebSocket.prototype.send = originalSend;
      delete window.__fnclusterPoc;
      console.log('[FnCluster] PoC removed');
    },
  };
  window.__fnclusterPoc = api;
  console.log('[FnCluster] read-only PoC installed. Try __fnclusterPoc.sockets()');
})();
