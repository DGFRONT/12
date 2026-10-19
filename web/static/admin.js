(async () => {
  const me = await api.session();
  if (!me) { location.replace('/?next=/admin'); return; }
  if (me.role !== 'admin') { location.replace('/app'); return; }
  document.getElementById('me').textContent = me.display_name;
  document.getElementById('logout').onclick = api.logout;

  const $ = (id) => document.getElementById(id);
  const fail = (e) => alert(e.message);

  // ---- tabs ----
  const loaders = {};
  let roomsTimer = null;
  function showTab(name) {
    document.querySelectorAll('#tabs button').forEach((b) => b.classList.toggle('active', b.dataset.tab === name));
    document.querySelectorAll('[data-panel]').forEach((p) => p.classList.toggle('hidden', p.dataset.panel !== name));
    clearInterval(roomsTimer);
    if (name === 'rooms') roomsTimer = setInterval(() => loaders.rooms(), 5000);
    loaders[name]?.();
    history.replaceState(null, '', '#' + name);
  }
  $('tabs').addEventListener('click', (e) => { if (e.target.dataset.tab) showTab(e.target.dataset.tab); });

  // ---- users ----
  loaders.users = async () => {
    const q = $('user-search').value.trim();
    const users = await api.get('/api/admin/users?q=' + encodeURIComponent(q));
    $('users').replaceChildren(...users.map(userRow));
  };
  let searchTimer;
  $('user-search').addEventListener('input', () => { clearTimeout(searchTimer); searchTimer = setTimeout(loaders.users, 300); });

  function userRow(u) {
    const self = u.id === me.id;
    return h('tr', {},
      h('td', {}, u.username),
      h('td', {}, u.display_name),
      h('td', {}, h('span', { class: 'badge ' + u.role }, ROLE_NAMES[u.role])),
      h('td', {}, u.is_blocked ? h('span', { class: 'badge blocked' }, 'заблокирован') : 'активен'),
      h('td', {}, fmtDate(u.created_at)),
      h('td', {}, fmtDate(u.last_login_at)),
      h('td', { class: 'actions' },
        h('button', { class: 'link', onclick: () => rename(u) }, 'имя'),
        h('button', { class: 'link', onclick: () => resetPassword(u) }, 'сброс пароля'),
        self ? null : h('button', { class: 'link', onclick: () => patchUser(u, { role: u.role === 'admin' ? 'user' : 'admin' }) },
          u.role === 'admin' ? 'снять админа' : 'сделать админом'),
        self ? null : h('button', { class: 'link', onclick: () => patchUser(u, { is_blocked: !u.is_blocked }) },
          u.is_blocked ? 'разблокировать' : 'заблокировать'),
        self ? null : h('button', { class: 'link danger', onclick: () => deleteUser(u) }, 'удалить'),
      ),
    );
  }

  async function patchUser(u, body) {
    try { await api.patch('/api/admin/users/' + u.id, body); } catch (e) { fail(e); }
    loaders.users();
  }
  async function rename(u) {
    const name = prompt('Новое отображаемое имя', u.display_name);
    if (name && name.trim() !== u.display_name) patchUser(u, { display_name: name.trim() });
  }
  async function resetPassword(u) {
    if (!confirm(`Сгенерировать новый пароль для ${u.username}? Текущие сессии будут завершены.`)) return;
    try {
      const res = await api.patch('/api/admin/users/' + u.id, { reset_password: true });
      prompt(`Новый пароль для ${u.username} (показывается один раз):`, res.generated_password);
    } catch (e) { fail(e); }
  }
  async function deleteUser(u) {
    if (!confirm(`Удалить ${u.username}? Его активные комнаты будут закрыты.`)) return;
    try { await api.del('/api/admin/users/' + u.id); } catch (e) { fail(e); }
    loaders.users();
  }

  $('user-form').addEventListener('submit', async (e) => {
    e.preventDefault();
    $('user-error').textContent = '';
    $('user-created').classList.add('hidden');
    try {
      const res = await api.post('/api/admin/users', {
        username: $('u-username').value.trim(),
        display_name: $('u-name').value.trim(),
        password: $('u-password').value,
        role: $('u-role').value,
      });
      const msg = $('user-created');
      msg.replaceChildren(`Пользователь ${res.user.username} зарегистрирован.`);
      if (res.generated_password) {
        msg.append(' Пароль (показывается один раз): ', h('span', { class: 'secret' }, res.generated_password));
      }
      msg.classList.remove('hidden');
      e.target.reset();
      loaders.users();
    } catch (err) {
      $('user-error').textContent = err.message;
    }
  });

  // ---- rooms ----
  let openRoomId = null;
  loaders.rooms = async () => {
    const rooms = await api.get('/api/admin/rooms?status=' + $('room-status').value);
    $('rooms').replaceChildren(...rooms.map((r) => h('tr', {},
      h('td', {}, h('button', { class: 'link', onclick: () => showRoom(r.id) }, r.name)),
      h('td', {}, r.host_name || '—'),
      h('td', {}, `${r.online} / ${r.max_participants}`),
      h('td', {}, h('span', { class: 'badge ' + r.status }, STATUS_NAMES[r.status])),
      h('td', {}, fmtDate(r.created_at)),
      h('td', { class: 'actions' }, r.status === 'active' ? [
        h('a', { href: r.url, target: '_blank' }, 'войти'), ' ',
        h('button', { class: 'link danger', onclick: () => closeRoom(r) }, 'закрыть'),
      ] : null),
    )));
    if (!rooms.length) $('rooms').append(h('tr', {}, h('td', { colspan: 6, class: 'muted' }, 'Нет комнат')));
    if (openRoomId) showRoom(openRoomId);
  };
  $('room-status').addEventListener('change', loaders.rooms);

  async function closeRoom(r) {
    if (!confirm(`Закрыть «${r.name}»? Все участники будут отключены.`)) return;
    try { await api.post(`/api/admin/rooms/${r.id}/close`); } catch (e) { fail(e); }
    loaders.rooms();
  }

  async function showRoom(id) {
    openRoomId = id;
    const d = await api.get('/api/admin/rooms/' + id);
    // Live state from the hub when the room is open; otherwise fall back to the participation log.
    const media = (p) => p.audio !== undefined ? ` · микрофон ${p.audio ? 'вкл.' : 'выкл.'}, камера ${p.video ? 'вкл.' : 'выкл.'}` : '';
    const inside = d.live ? d.live.participants.map((p) => ({ display_name: p.name, kind: p.kind, audio: p.audio, video: p.video }))
      : d.participants.filter((p) => p.admitted_at && !p.left_at);
    const waiting = d.live ? d.live.queue.map((q) => ({ display_name: q.name }))
      : d.participants.filter((p) => !p.admitted_at && !p.left_at);
    const box = $('room-detail');
    box.replaceChildren(
      h('div', { class: 'row' }, h('h2', {}, d.room.name), h('span', { class: 'spacer' }),
        h('button', { onclick: () => { openRoomId = null; box.classList.add('hidden'); } }, 'Скрыть')),
      h('p', { class: 'muted small' }, `Создатель: ${d.room.owner_name || '—'} · хост: ${d.room.host_name || '—'} · ${STATUS_NAMES[d.room.status]}`
        + (d.live && !d.live.host_connected ? ' · организатор не в сети (комната закроется через 5 минут)' : '')),
      h('h3', {}, `В комнате (${inside.length})`),
      inside.length ? h('ul', {}, inside.map((p) => h('li', {}, p.display_name, ' ', h('span', { class: 'badge' }, KIND_NAMES[p.kind]), h('span', { class: 'muted small' }, media(p))))) : h('p', { class: 'muted' }, 'никого'),
      h('h3', {}, `В очереди (${waiting.length})`),
      waiting.length ? h('ul', {}, waiting.map((p) => h('li', {}, p.display_name))) : h('p', { class: 'muted' }, 'пусто'),
      h('h3', {}, `Чат (${d.messages.length})`),
      h('div', { class: 'chat-log' }, d.messages.length ? d.messages.map((m) =>
        h('div', { class: 'msg' + (m.deleted ? ' deleted' : '') },
          h('span', { class: 'muted small' }, fmtDate(m.created_at) + ' '),
          h('b', {}, m.author), ` (${KIND_NAMES[m.author_kind]}): `, m.body)) : h('p', { class: 'muted' }, 'сообщений нет')),
    );
    box.classList.remove('hidden');
  }

  // ---- stats ----
  const isoDate = (d) => d.toISOString().slice(0, 10);
  $('s-to').value = isoDate(new Date());
  $('s-from').value = isoDate(new Date(Date.now() - 29 * 864e5));
  loaders.stats = async () => {
    let st;
    try { st = await api.get(`/api/admin/stats?from=${$('s-from').value}&to=${$('s-to').value}`); } catch (e) { return fail(e); }
    const tile = (value, label) => h('div', { class: 'stat' }, h('div', { class: 'value' }, value), h('div', { class: 'label' }, label));
    const round = (x) => Math.round(x * 10) / 10;
    $('stats').replaceChildren(
      tile(st.users_total, 'пользователей'),
      tile(st.active_users, 'входили за период'),
      tile(st.admins, 'администраторов'),
      tile(st.users_blocked, 'заблокировано'),
      tile(st.rooms_active_now, 'комнат идёт сейчас'),
      tile(st.online_now, 'человек онлайн'),
      tile(st.rooms_created, 'комнат создано'),
      tile(round(st.avg_room_minutes), 'мин — средняя встреча'),
      tile(round(st.max_room_minutes), 'мин — самая долгая'),
      tile(st.participants, 'входов в комнаты'),
      tile(st.guests, 'из них гостей'),
      tile(round(st.avg_wait_seconds), 'сек — ожидание в очереди'),
      tile(st.rejected, 'отказов во входе'),
      tile(st.messages, 'сообщений в чате'),
    );
    const max = Math.max(1, ...st.daily.map((d) => d.participants));
    $('bars').replaceChildren(...st.daily.map((d) => {
      const bar = h('div', { title: `${d.day}: ${d.participants} входов, ${d.rooms} комнат, ${d.messages} сообщений` });
      bar.style.height = (d.participants / max * 100) + '%';
      return bar;
    }));
    $('bars-legend').textContent = st.daily.length ? `${st.daily[0].day} — ${st.daily[st.daily.length - 1].day}, максимум ${max}` : '';
    $('top-users').replaceChildren(...st.top_users.map((u) => h('tr', {}, h('td', {}, u.display_name), h('td', {}, round(u.minutes)), h('td', {}, u.rooms))));
  };
  $('stats-form').addEventListener('submit', (e) => { e.preventDefault(); loaders.stats(); });

  // ---- data browser ----
  const PAGE = 50;
  let offset = 0;
  loaders.data = async () => {
    const sel = $('table-name');
    if (!sel.options.length) {
      const names = await api.get('/api/admin/tables');
      sel.replaceChildren(...names.map((n) => h('option', { value: n }, n)));
    }
    const page = await api.get(`/api/admin/tables/${sel.value}?limit=${PAGE}&offset=${offset}`);
    const cols = page.rows.length ? Object.keys(page.rows[0]) : [];
    const cell = (v) => v === null ? h('span', { class: 'muted' }, 'null') : typeof v === 'object' ? h('pre', {}, JSON.stringify(v)) : String(v);
    $('table-view').replaceChildren(
      h('thead', {}, h('tr', {}, cols.map((c) => h('th', {}, c)))),
      h('tbody', {}, page.rows.map((row) => h('tr', {}, cols.map((c) => h('td', {}, cell(row[c])))))),
    );
    $('page-info').textContent = page.total ? `${offset + 1}–${Math.min(offset + PAGE, page.total)} из ${page.total}` : 'пусто';
    $('prev').disabled = offset === 0;
    $('next').disabled = offset + PAGE >= page.total;
  };
  $('table-name').addEventListener('change', () => { offset = 0; loaders.data(); });
  $('prev').onclick = () => { offset = Math.max(0, offset - PAGE); loaders.data(); };
  $('next').onclick = () => { offset += PAGE; loaders.data(); };

  // ---- audit ----
  loaders.audit = async () => {
    const list = await api.get('/api/admin/audit?limit=200');
    $('audit').replaceChildren(...list.map((a) => h('tr', {},
      h('td', {}, fmtDate(a.created_at)),
      h('td', {}, a.actor_name || 'система'),
      h('td', {}, a.action),
      h('td', {}, a.target_type ? `${a.target_type} ${a.target_id}` : ''),
      h('td', {}, a.meta ? h('pre', {}, JSON.stringify(a.meta)) : ''),
      h('td', {}, a.ip || ''),
    )));
  };

  // ---- export ----
  loaders.export = () => $('export-password').focus();
  $('export-form').addEventListener('submit', async (e) => {
    e.preventDefault();
    $('export-error').textContent = '';
    try {
      const res = await api.raw('POST', '/api/admin/export', { password: $('export-password').value });
      const blob = await res.blob();
      const name = (res.headers.get('Content-Disposition') || '').match(/filename="(.+)"/)?.[1] || 'export.zip';
      const a = h('a', { href: URL.createObjectURL(blob), download: name });
      a.click();
      setTimeout(() => URL.revokeObjectURL(a.href), 10000);
      e.target.reset();
    } catch (err) {
      $('export-error').textContent = err.message;
    }
  });

  showTab(location.hash.slice(1) in loaders ? location.hash.slice(1) : 'users');
  window.addEventListener('hashchange', () => { if (location.hash.slice(1) in loaders) showTab(location.hash.slice(1)); });
})();
