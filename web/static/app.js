(async () => {
  const me = await api.session();
  if (!me) { location.replace('/?next=/app'); return; }
  document.getElementById('me').textContent = me.display_name;
  document.getElementById('logout').onclick = api.logout;
  if (me.role === 'admin') document.getElementById('admin-link').classList.remove('hidden');

  const tbody = document.getElementById('rooms');

  async function load() {
    const rooms = await api.get('/api/rooms');
    tbody.replaceChildren(...rooms.map((r) => h('tr', {},
      h('td', {}, r.name),
      h('td', {}, h('span', { class: 'badge ' + r.status }, STATUS_NAMES[r.status])),
      h('td', {}, fmtDate(r.created_at)),
      h('td', {}, r.status === 'active' ? h('a', { href: r.url }, 'открыть') : '—'),
      h('td', { class: 'actions' }, r.status === 'active' && r.host_id === me.id
        ? h('button', { class: 'danger', onclick: () => endRoom(r) }, 'Завершить')
        : null),
    )));
    if (!rooms.length) tbody.append(h('tr', {}, h('td', { colspan: 5, class: 'muted' }, 'Комнат пока нет')));
  }

  async function endRoom(r) {
    if (!confirm(`Завершить «${r.name}» для всех участников?`)) return;
    try { await api.post(`/api/rooms/${r.slug}/end`); } catch (e) { alert(e.message); }
    load();
  }

  const form = document.getElementById('create-form');
  form.addEventListener('submit', async (e) => {
    e.preventDefault();
    document.getElementById('create-error').textContent = '';
    try {
      const room = await api.post('/api/rooms', { name: document.getElementById('room-name').value.trim() });
      document.getElementById('created-url').textContent = room.url;
      document.getElementById('created').classList.remove('hidden');
      form.reset();
      load();
    } catch (err) {
      document.getElementById('create-error').textContent = err.message;
    }
  });
  document.getElementById('copy-url').onclick = () =>
    navigator.clipboard.writeText(document.getElementById('created-url').textContent);

  load();
})();
