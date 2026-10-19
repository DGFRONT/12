(async () => {
  const next = new URLSearchParams(location.search).get('next');
  // Only allow same-site relative redirects.
  const target = (u) => (next && next.startsWith('/') && !next.startsWith('//')) ? next : (u.role === 'admin' ? '/admin' : '/app');

  const existing = await api.session();
  if (existing) { location.replace(target(existing)); return; }

  const form = document.getElementById('login-form');
  const error = document.getElementById('error');
  form.addEventListener('submit', async (e) => {
    e.preventDefault();
    error.textContent = '';
    try {
      const u = await api.login(form.username.value.trim(), form.password.value);
      location.replace(target(u));
    } catch (err) {
      error.textContent = err.message;
    }
  });
})();
