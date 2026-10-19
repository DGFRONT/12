(async () => {
  const $ = (id) => document.getElementById(id);
  const slug = location.pathname.split('/').pop();
  const guestKey = 'guest:' + slug;
  $('login-link').href = '/?next=' + encodeURIComponent(location.pathname);

  const BADGES = { host: 'организатор', admin: 'администратор', guest: 'гость' };
  const REASONS = {
    ended: ['Конференция завершена', 'Организатор завершил встречу.'],
    closed_by_admin: ['Конференция закрыта', 'Комнату закрыл администратор.'],
    host_left: ['Конференция завершена', 'Организатор вышел и не вернулся.'],
    kicked: ['Вас удалили из комнаты', 'Организатор удалил вас из конференции.'],
    rejected: ['Вход отклонён', 'Организатор не впустил вас в комнату.'],
    replaced: ['Вы подключились в другом окне', 'Эта вкладка отключена.'],
    account_disabled: ['Доступ закрыт', 'Ваша учётная запись заблокирована.'],
    left: ['Вы вышли из конференции', ''],
  };

  // ---- screens ----
  function show(screen) {
    for (const s of ['prejoin', 'waiting', 'final', 'room']) $('screen-' + s).classList.toggle('hidden', s !== screen);
  }
  function finish(reason) {
    state.done = true;
    const [title, text] = REASONS[reason] || ['Соединение закрыто', ''];
    $('final-title').textContent = title;
    $('final-text').textContent = text;
    if (!me) $('final-home').classList.add('hidden');
    show('final');
    if (ws) ws.close();
  }
  function toast(text, error = false) {
    const el = h('div', { class: 'toast' + (error ? ' error' : '') }, text);
    $('toasts').append(el);
    setTimeout(() => el.remove(), 4000);
  }

  // ---- room info and identity ----
  let room;
  try {
    room = await api.get('/api/rooms/' + encodeURIComponent(slug));
  } catch (e) {
    $('room-name').textContent = 'Комната не найдена';
    $('room-info').textContent = 'Проверьте ссылку-приглашение.';
    return;
  }
  $('room-name').textContent = room.name;
  $('waiting-room-name').textContent = room.name;
  $('room-title').textContent = room.name;
  document.title = room.name + ' — Конференции';
  if (room.status !== 'active') {
    $('room-info').textContent = 'Конференция уже завершена.';
    return;
  }
  $('room-info').textContent = `Организатор: ${room.host_name || '—'} · участников: ${room.online} из ${room.max_participants}`;

  const me = await api.session();
  let guest = null;
  try { guest = JSON.parse(sessionStorage.getItem(guestKey)); } catch (_) { /* storage unavailable */ }

  // ---- connection ----
  const state = {
    self: null, kind: null, admitted: false, done: false,
    participants: [], queue: [], max: room.max_participants,
  };
  let ws = null;
  let attempts = 0;

  async function connect() {
    let token = guest?.token;
    if (me) {
      await api.refresh(); // fresh access token for the handshake
      token = api.token;
      if (!token) { location.href = '/?next=' + encodeURIComponent(location.pathname); return; }
    }
    const proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
    ws = new WebSocket(`${proto}//${location.host}/ws/rooms/${slug}`);
    ws.onopen = () => ws.send(JSON.stringify({ type: 'auth', data: { token } }));
    ws.onmessage = (e) => {
      const msg = JSON.parse(e.data);
      handlers[msg.type]?.(msg.data);
    };
    ws.onclose = () => {
      if (state.done) return;
      if (attempts >= 6) { finish('connection'); return; }
      attempts++;
      $('reconnecting').classList.remove('hidden');
      setTimeout(connect, Math.min(1000 * 2 ** (attempts - 1), 10000));
    };
  }

  function send(type, data) {
    if (ws && ws.readyState === WebSocket.OPEN) ws.send(JSON.stringify({ type, data }));
  }

  const handlers = {
    hello(d) {
      attempts = 0;
      $('reconnecting').classList.add('hidden');
      state.self = d.participant_id;
      state.kind = d.kind;
      state.max = d.max;
    },
    waiting() {
      state.admitted = false;
      show('waiting');
    },
    admitted(d) {
      state.admitted = true;
      show('room');
      $('messages').replaceChildren();
      d.history.forEach(addMessage);
      setParticipants(d.participants);
    },
    participants: setParticipants,
    queue(list) {
      state.queue = list;
      renderQueue();
    },
    'chat.message': addMessage,
    'chat.deleted'(d) { document.querySelector(`.msg[data-id="${d.id}"]`)?.remove(); },
    permission(d) {
      // The organizer allowed a device: turn it on (in stage 3 the browser asks for consent here).
      if (d.audio === true) { toast('Организатор разрешил вам говорить'); send('media.toggle', { kind: 'audio', enabled: true }); }
      if (d.video === true) { toast('Организатор разрешил вам включить камеру'); send('media.toggle', { kind: 'video', enabled: true }); }
      if (d.audio === false || d.video === false) toast('Организатор ограничил ваши микрофон или камеру');
    },
    notice(d) { toast(d.text); },
    error(d) {
      if (d.code === 'unauthorized') {
        try { sessionStorage.removeItem(guestKey); } catch (_) { /* ignore */ }
        state.done = true;
        show('prejoin');
        $('prejoin-error').textContent = d.message;
        return;
      }
      toast(d.message, true);
    },
    removed(d) { finish(d.reason); },
    'room.closed'(d) {
      try { sessionStorage.removeItem(guestKey); } catch (_) { /* ignore */ }
      finish(d.reason);
    },
  };

  // ---- participants ----
  const isManager = () => state.admitted && (state.kind === 'host' || state.kind === 'admin');
  const selfView = () => state.participants.find((p) => p.id === state.self);

  function setParticipants(list) {
    state.participants = list;
    const mine = selfView();
    if (mine) state.kind = mine.kind;
    const seats = list.filter((p) => p.kind !== 'admin').length;
    $('room-count').textContent = `${seats} из ${state.max}`;
    $('people-count').textContent = `(${list.length})`;
    $('queue-tab').classList.toggle('hidden', !isManager());
    $('end-btn').classList.toggle('hidden', !isManager());
    renderTiles();
    renderPeople();
    renderQueue();
    renderControls();
  }

  const ICONS = {
    audio: 'M12 14a3 3 0 0 0 3-3V5a3 3 0 0 0-6 0v6a3 3 0 0 0 3 3zm5-3a5 5 0 0 1-10 0H5a7 7 0 0 0 6 6.9V21h2v-3.1A7 7 0 0 0 19 11h-2z',
    video: 'M17 10.5V7a1 1 0 0 0-1-1H4a1 1 0 0 0-1 1v10a1 1 0 0 0 1 1h12a1 1 0 0 0 1-1v-3.5l4 4v-11l-4 4z',
  };
  function icon(kind, on) {
    const span = h('span', { class: 'icon' + (on ? '' : ' off'), title: (kind === 'audio' ? 'микрофон ' : 'камера ') + (on ? 'вкл.' : 'выкл.') });
    const svg = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
    svg.setAttribute('viewBox', '0 0 24 24');
    const path = document.createElementNS('http://www.w3.org/2000/svg', 'path');
    path.setAttribute('d', ICONS[kind]);
    svg.append(path);
    span.append(svg);
    return span;
  }
  const initials = (name) => name.split(/\s+/).slice(0, 2).map((w) => w[0] || '').join('').toUpperCase();
  const badge = (kind) => BADGES[kind] ? h('span', { class: 'badge ' + kind }, BADGES[kind]) : null;

  function renderTiles() {
    $('tiles').replaceChildren(...state.participants.map((p) => h('div', { class: 'tile' + (p.audio ? ' speaking' : '') },
      h('div', { class: 'avatar' }, initials(p.name)),
      h('div', { class: 'label' },
        icon('audio', p.audio), icon('video', p.video),
        h('span', { class: 'name' }, p.name + (p.id === state.self ? ' (вы)' : '')),
        badge(p.kind)),
    )));
  }

  function renderPeople() {
    const manager = isManager();
    $('people').replaceChildren(...state.participants.map((p) => {
      const acts = [];
      const self = p.id === state.self;
      const protectedTarget = p.kind === 'admin' && state.kind === 'host';
      if (manager && !self && !protectedTarget) {
        if (p.audio) acts.push(h('button', { onclick: () => send('media.mute', { participant_id: p.id, kind: 'audio' }) }, 'Выкл. микрофон'));
        if (p.video) acts.push(h('button', { onclick: () => send('media.mute', { participant_id: p.id, kind: 'video' }) }, 'Выкл. камеру'));
        if (p.kind === 'guest') {
          acts.push(h('button', { class: p.allow_audio ? 'on' : '', onclick: () => send('media.permission', { participant_id: p.id, audio: !p.allow_audio }) },
            p.allow_audio ? 'Микрофон разрешён' : 'Разрешить микрофон'));
          acts.push(h('button', { class: p.allow_video ? 'on' : '', onclick: () => send('media.permission', { participant_id: p.id, video: !p.allow_video }) },
            p.allow_video ? 'Камера разрешена' : 'Разрешить камеру'));
        }
        if (p.kind === 'member') {
          acts.push(h('button', { onclick: () => confirm(`Передать права организатора участнику ${p.name}?`) && send('host.transfer', { participant_id: p.id }) }, 'Сделать организатором'));
        }
        if (p.kind !== 'host') {
          acts.push(h('button', { class: 'danger', onclick: () => confirm(`Удалить ${p.name} из комнаты?`) && send('participant.kick', { participant_id: p.id }) }, 'Выгнать'));
        }
      }
      return h('li', {},
        h('div', { class: 'who' }, icon('audio', p.audio), icon('video', p.video), h('b', {}, p.name), self ? h('span', { class: 'muted small' }, '(вы)') : null, badge(p.kind)),
        acts.length ? h('div', { class: 'acts' }, acts) : null);
    }));
  }

  function renderQueue() {
    const manager = isManager();
    const n = state.queue.length;
    $('queue-count').textContent = n;
    $('queue-count').classList.toggle('hidden', !manager || n === 0);
    const seats = state.participants.filter((p) => p.kind === 'member' || p.kind === 'guest').length;
    const free = Math.max(0, state.max - 1 - seats);
    $('queue-seats').textContent = `Свободных мест: ${free}`;
    $('admit-all').disabled = n === 0 || free === 0;
    $('queue').replaceChildren(...(n ? state.queue.map((q) => h('li', {},
      h('div', { class: 'who' }, h('b', {}, q.name), q.registered ? h('span', { class: 'badge' }, 'пользователь') : badge('guest')),
      h('div', { class: 'acts' },
        h('button', { class: 'primary', disabled: free === 0, onclick: () => send('queue.admit', { participant_id: q.id }) }, 'Впустить'),
        h('button', { onclick: () => send('queue.reject', { participant_id: q.id }) }, 'Отклонить')),
    )) : [h('li', { class: 'muted' }, 'Никто не ждёт')]));
  }
  let lastQueue = 0;
  const origQueue = handlers.queue;
  handlers.queue = (list) => {
    if (isManager() && list.length > lastQueue) toast(`${list[list.length - 1].name} просит войти`);
    lastQueue = list.length;
    origQueue(list);
  };
  $('admit-all').onclick = () => send('queue.admit_all');

  // ---- media controls (state only until stage 3) ----
  function renderControls() {
    const mine = selfView();
    if (!mine) return;
    for (const [kind, btn] of [['audio', $('mic-btn')], ['video', $('cam-btn')]]) {
      const on = mine[kind];
      const allowed = mine['allow_' + kind];
      btn.classList.toggle('on', on);
      btn.classList.toggle('locked', !allowed);
      btn.title = allowed ? '' : 'Включить можно после разрешения организатора';
      btn.textContent = (kind === 'audio' ? 'Микрофон' : 'Камера') + (on ? ': вкл.' : ': выкл.');
    }
  }
  function toggle(kind) {
    const mine = selfView();
    if (!mine) return;
    if (!mine[kind] && !mine['allow_' + kind]) { toast('Организатор ещё не разрешил вам это включить', true); return; }
    send('media.toggle', { kind, enabled: !mine[kind] });
  }
  $('mic-btn').onclick = () => toggle('audio');
  $('cam-btn').onclick = () => toggle('video');

  // ---- chat ----
  function addMessage(m) {
    const mine = m.participant_id === state.self;
    const el = h('div', { class: 'msg', 'data-id': m.id },
      h('div', { class: 'meta' },
        h('b', {}, mine ? 'Вы' : m.author), badge(m.author_kind),
        h('span', {}, new Date(m.created_at).toLocaleTimeString('ru-RU', { hour: '2-digit', minute: '2-digit' })),
        isManager() ? h('button', { class: 'link del', title: 'Удалить', onclick: () => send('chat.delete', { id: m.id }) }, '×') : null),
      h('div', { class: 'text' }, m.body));
    const box = $('messages');
    const atBottom = box.scrollHeight - box.scrollTop - box.clientHeight < 40;
    box.append(el);
    if (atBottom || mine) box.scrollTop = box.scrollHeight;
    if (!mine && document.querySelector('[data-side="chat"]:not(.active)')) toast(`${m.author}: ${m.body.slice(0, 60)}`);
  }
  $('chat-form').addEventListener('submit', (e) => {
    e.preventDefault();
    const text = $('chat-input').value.trim();
    if (!text) return;
    send('chat.send', { text });
    $('chat-input').value = '';
  });

  // ---- side tabs ----
  $('side-tabs').addEventListener('click', (e) => {
    const name = e.target.closest('button')?.dataset.side;
    if (!name) return;
    document.querySelectorAll('#side-tabs button').forEach((b) => b.classList.toggle('active', b.dataset.side === name));
    document.querySelectorAll('[data-pane]').forEach((p) => p.classList.toggle('hidden', p.dataset.pane !== name));
  });

  // ---- leaving ----
  function leave() {
    send('leave');
    try { sessionStorage.removeItem(guestKey); } catch (_) { /* ignore */ }
    finish('left');
  }
  $('cancel-wait').onclick = leave;
  $('leave-btn').onclick = () => {
    if (state.kind !== 'host') { leave(); return; }
    const members = state.participants.filter((p) => p.kind === 'member');
    $('transfer-to').replaceChildren(...members.map((p) => h('option', { value: p.id }, p.name)));
    $('transfer-field').classList.toggle('hidden', !members.length);
    $('transfer-leave').classList.toggle('hidden', !members.length);
    $('no-transfer').classList.toggle('hidden', members.length > 0);
    $('leave-dialog').showModal();
  };
  $('transfer-leave').onclick = () => {
    send('host.transfer', { participant_id: $('transfer-to').value });
    $('leave-dialog').close();
    setTimeout(leave, 300); // let the transfer reach the server first
  };
  const endForAll = () => {
    if (confirm('Завершить конференцию для всех участников?')) send('room.end');
  };
  $('end-leave').onclick = () => { $('leave-dialog').close(); endForAll(); };
  $('cancel-leave').onclick = () => $('leave-dialog').close();
  $('end-btn').onclick = endForAll;
  // ---- start ----
  if (me) {
    $('member-name').textContent = me.display_name;
    $('member-join').classList.remove('hidden');
    $('member-join-btn').onclick = () => connect();
  } else if (guest) {
    connect(); // reload of the page: keep the same guest identity
  } else {
    $('guest-form').classList.remove('hidden');
    $('guest-form').addEventListener('submit', async (e) => {
      e.preventDefault();
      $('prejoin-error').textContent = '';
      try {
        const res = await api.post(`/api/rooms/${slug}/guest`, { display_name: $('display-name').value });
        guest = { token: res.token, name: res.display_name };
        try { sessionStorage.setItem(guestKey, JSON.stringify(guest)); } catch (_) { /* ignore */ }
        connect();
      } catch (err) {
        $('prejoin-error').textContent = err.message;
      }
    });
  }
})();
