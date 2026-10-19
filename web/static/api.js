// Shared API client. The access token lives in memory only; the refresh token is an httpOnly cookie.
const api = (() => {
  let accessToken = null;
  let user = null;
  let refreshing = null;

  async function refresh() {
    if (!refreshing) {
      refreshing = fetch('/api/auth/refresh', { method: 'POST' })
        .then(async (res) => {
          if (!res.ok) { accessToken = null; user = null; return false; }
          const data = await res.json();
          accessToken = data.access_token;
          user = data.user;
          return true;
        })
        .finally(() => { refreshing = null; });
    }
    return refreshing;
  }

  async function request(method, path, body, { raw = false, retry = true } = {}) {
    const headers = {};
    if (accessToken) headers.Authorization = 'Bearer ' + accessToken;
    if (body !== undefined) headers['Content-Type'] = 'application/json';
    const res = await fetch(path, { method, headers, body: body === undefined ? undefined : JSON.stringify(body) });
    if (res.status === 401 && retry && await refresh()) {
      return request(method, path, body, { raw, retry: false });
    }
    if (!res.ok) {
      let msg = res.statusText;
      try { msg = (await res.json()).error || msg; } catch (_) { /* not JSON */ }
      const err = new Error(msg);
      err.status = res.status;
      throw err;
    }
    if (raw) return res;
    if (res.status === 204) return null;
    return res.json();
  }

  async function login(username, password) {
    const data = await request('POST', '/api/auth/login', { username, password }, { retry: false });
    accessToken = data.access_token;
    user = data.user;
    return user;
  }

  async function logout() {
    await fetch('/api/auth/logout', { method: 'POST' });
    accessToken = null;
    user = null;
    location.href = '/';
  }

  // Restores the session from the refresh cookie; returns the user or null.
  async function session() {
    if (user) return user;
    return (await refresh()) ? user : null;
  }

  return {
    login, logout, session, refresh,
    get: (p) => request('GET', p),
    post: (p, b) => request('POST', p, b ?? {}),
    patch: (p, b) => request('PATCH', p, b),
    del: (p) => request('DELETE', p),
    raw: (m, p, b) => request(m, p, b, { raw: true }),
    get user() { return user; },
  };
})();

// Tiny DOM helper: h('td', {class: 'x', onclick: fn}, 'text', child)
function h(tag, attrs = {}, ...children) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (v === undefined || v === null || v === false) continue;
    if (k.startsWith('on')) el.addEventListener(k.slice(2), v);
    else if (k === 'class') el.className = v;
    else el.setAttribute(k, v === true ? '' : v);
  }
  for (const c of children.flat()) {
    if (c === null || c === undefined || c === false) continue;
    el.append(c instanceof Node ? c : document.createTextNode(String(c)));
  }
  return el;
}

function fmtDate(s) {
  return s ? new Date(s).toLocaleString('ru-RU') : '—';
}

const ROLE_NAMES = { admin: 'админ', user: 'пользователь' };
const STATUS_NAMES = { active: 'идёт', ended: 'завершена', closed_by_admin: 'закрыта админом' };
const KIND_NAMES = { host: 'хост', member: 'участник', guest: 'гость', admin: 'администратор' };
