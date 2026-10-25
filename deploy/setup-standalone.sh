#!/usr/bin/env bash
# Установка на отдельный сервер, где порты 80/443 свободны: сервис со своим Caddy,
# который сам получает сертификат Let's Encrypt.
#
#   cd deploy && ./setup-standalone.sh vcos.example.ru
#
# Скрипт ничего не удаляет. Он проверяет сервер, создаёт .env (если его ещё нет),
# собирает образ, запускает сервис и в конце запускает check.sh.
# Повторный запуск безопасен: .env и база сохраняются, сервис обновляется.
#
# Переменные: PUBLIC_IP=1.2.3.4 — задать IP вручную; NO_BUILD=1 — не собирать образ,
# если он уже загружен (docker load -i vcos-app.tar.gz).
set -euo pipefail

DOMAIN="${1:-}"
if [[ -z "$DOMAIN" ]]; then
  echo "Использование: $0 <домен>, например: $0 vcos.rsocit.ru" >&2
  exit 1
fi
cd "$(dirname "$0")"

ok()   { printf '  \033[32m✔\033[0m %s\n' "$*"; }
warn() { printf '  \033[33m!\033[0m %s\n' "$*"; }
die()  { printf '  \033[31m✘\033[0m %s\n' "$*"; exit 1; }
step() { printf '\n\033[1m%s\033[0m\n' "$*"; }
trap 'printf "  \033[31m✘\033[0m скрипт остановился на строке %s. Пришлите его вывод.\n" "$LINENO"' ERR

COMPOSE=(docker compose --profile caddy)

# ---------------------------------------------------------------------------
step "1. Сервер"
command -v docker >/dev/null || die "Docker не установлен: curl -fsSL https://get.docker.com | sudo sh"
docker compose version >/dev/null 2>&1 || die "нет docker compose v2 (входит в свежий Docker)"
docker info >/dev/null 2>&1 || die "нет доступа к Docker: запустите через sudo или добавьте себя в группу docker"
ok "Docker $(docker version --format '{{.Server.Version}}'), архитектура $(uname -m)"
mem=$(awk '/MemTotal/ {print int($2/1024)}' /proc/meminfo)
[[ "$mem" -ge 1500 ]] && ok "память: ${mem} МБ" || warn "память: ${mem} МБ — для звонков лучше от 2 ГБ"

# Containers of this compose project (docker compose ps would need .env, which may not exist yet).
RUNNING="$(docker ps -q --filter "label=com.docker.compose.project.working_dir=$(pwd)" | wc -l)"
if [[ "$RUNNING" -gt 0 ]]; then
  ok "сервис уже запущен — будет обновлён"
elif ! command -v ss >/dev/null; then
  warn "утилиты ss нет — занятость портов не проверена (sudo apt install iproute2)"
else
  for p in 80/tcp 443/tcp 8080/tcp 50000/udp 3478/udp 3478/tcp; do
    port="${p%/*}"; flag=-lnt; [[ "${p#*/}" == udp ]] && flag=-lnu
    if ss $flag | awk '{print $4}' | grep -Eq "[:.]$port\$"; then
      die "порт $p занят. Посмотрите, кем: sudo ss -lntup | grep ':$port '"
    fi
  done
  ok "порты 80, 443, 8080, 50000/udp и 3478 свободны"
fi

# ---------------------------------------------------------------------------
step "2. Домен"
PUBLIC_IP="${PUBLIC_IP:-$(curl -4 -fsS --max-time 5 https://ifconfig.me 2>/dev/null || curl -4 -fsS --max-time 5 https://api.ipify.org 2>/dev/null || true)}"
[[ -n "$PUBLIC_IP" ]] || die "не удалось узнать внешний IP сервера (нет доступа в интернет?)"
ok "внешний IP: $PUBLIC_IP"
A="$(getent ahostsv4 "$DOMAIN" | awk 'NR==1{print $1}')"
if [[ "$A" != "$PUBLIC_IP" ]]; then
  # Without a correct A record Let's Encrypt fails and repeated failures hit its rate limit.
  die "A-запись $DOMAIN = ${A:-нет}, а нужен $PUBLIC_IP. Исправьте DNS, подождите и запустите снова."
fi
ok "A: $DOMAIN → $A"
AAAA="$(getent ahostsv6 "$DOMAIN" 2>/dev/null | awk '{print $1}' | grep -v '^::ffff:' | sort -u | head -1 || true)"
if [[ -n "$AAAA" ]] && ! ip -6 addr | grep -q "$AAAA"; then
  die "AAAA-запись $DOMAIN = $AAAA указывает не на этот сервер. Удалите её в DNS и запустите снова."
fi
ok "AAAA: ${AAAA:-нет}"

# ---------------------------------------------------------------------------
step "3. Настройки (.env)"
if [[ -f .env ]]; then
  grep -q "^DOMAIN=$DOMAIN\$" .env || die ".env уже есть, но для другого домена. Проверьте его вручную."
  sed -i "s|^PUBLIC_IP=.*|PUBLIC_IP=$PUBLIC_IP|" .env
  ok ".env уже есть — пароли и секреты сохранены"
  NEW_ADMIN=""
else
  NEW_ADMIN="$(openssl rand -base64 18 | tr -d '/+=' | cut -c1-16)"
  sed -e "s|^DOMAIN=.*|DOMAIN=$DOMAIN|" \
      -e "s|^PUBLIC_IP=.*|PUBLIC_IP=$PUBLIC_IP|" \
      -e "s|^POSTGRES_PASSWORD=.*|POSTGRES_PASSWORD=$(openssl rand -hex 24)|" \
      -e "s|^JWT_SECRET=.*|JWT_SECRET=$(openssl rand -hex 32)|" \
      -e "s|^TURN_SECRET=.*|TURN_SECRET=$(openssl rand -hex 32)|" \
      -e "s|^ADMIN_PASSWORD=.*|ADMIN_PASSWORD=$NEW_ADMIN|" \
      .env.example > .env
  chmod 600 .env
  ok ".env создан, секреты сгенерированы"
fi

# ---------------------------------------------------------------------------
step "4. Сборка и запуск"
if [[ "${NO_BUILD:-0}" == 1 ]] && docker image inspect vcos-app:latest >/dev/null 2>&1; then
  ok "образ vcos-app:latest уже есть (NO_BUILD=1)"
else
  (cd .. && docker build -q -t vcos-app:latest -f deploy/Dockerfile .) >/dev/null
  ok "образ vcos-app:latest собран"
fi
"${COMPOSE[@]}" up -d >/dev/null 2>&1 || { "${COMPOSE[@]}" up -d; die "не удалось запустить сервис"; }
ok "контейнеры запущены"

for _ in $(seq 1 30); do
  [[ "$(curl -s --max-time 2 http://127.0.0.1:8080/healthz)" == "ok" ]] && break
  sleep 2
done
[[ "$(curl -s --max-time 2 http://127.0.0.1:8080/healthz)" == "ok" ]] || die "приложение не стартовало: ${COMPOSE[*]} logs app | tail -30"
ok "приложение отвечает"

printf '  … жду сертификат Let'"'"'s Encrypt (до 2 минут)\n'
for _ in $(seq 1 40); do
  [[ "$(curl -s --max-time 3 --resolve "$DOMAIN:443:127.0.0.1" "https://$DOMAIN/healthz")" == "ok" ]] && break
  sleep 3
done
if [[ "$(curl -s --max-time 3 --resolve "$DOMAIN:443:127.0.0.1" "https://$DOMAIN/healthz")" == "ok" ]]; then
  ok "https://$DOMAIN работает"
else
  warn "сертификат ещё не получен. Логи: ${COMPOSE[*]} logs caddy | tail -30"
fi

# ---------------------------------------------------------------------------
step "5. Проверка"
./check.sh || true

step "Готово"
echo "  Адрес:  https://$DOMAIN"
echo "  Вход:   admin"
if [[ -n "$NEW_ADMIN" ]]; then
  echo "  Пароль: $NEW_ADMIN   (показывается один раз; смените его в админ-панели)"
else
  echo "  Пароль: прежний (ADMIN_PASSWORD в deploy/.env, если вы его не меняли)"
fi
echo
echo "  Откройте у провайдера (группа безопасности / файрвол) входящие порты:"
echo "    TCP 22, 80, 443 · UDP 443 · UDP 50000 · TCP+UDP 3478 · UDP 49160–49200"
