(async () => {
  const slug = location.pathname.split('/').pop();
  const title = document.getElementById('room-name');
  const info = document.getElementById('room-info');
  document.getElementById('login-link').href = '/?next=' + encodeURIComponent(location.pathname);

  let room;
  try {
    room = await api.get('/api/rooms/' + encodeURIComponent(slug));
  } catch (e) {
    title.textContent = 'Комната не найдена';
    info.textContent = 'Проверьте ссылку-приглашение.';
    return;
  }
  title.textContent = room.name;
  document.title = room.name + ' — Конференции';
  if (room.status !== 'active') {
    info.textContent = 'Конференция уже завершена.';
    return;
  }
  info.textContent = `Организатор: ${room.host_name || '—'} · участников: ${room.online} из ${room.max_participants}`;

  const me = await api.session();
  if (me) {
    document.getElementById('member-name').textContent = me.display_name;
    document.getElementById('member-join').classList.remove('hidden');
  } else {
    document.getElementById('guest-form').classList.remove('hidden');
  }
})();
