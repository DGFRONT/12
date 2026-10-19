#!/usr/bin/env bash
# Готовит подключение сервиса конференций к уже работающему прокси на 80/443.
#
#   cd deploy && ./prepare-proxy.sh vcos.rsocit.ru
#
# Скрипт ТОЛЬКО ЧИТАЕТ: смотрит docker ps / inspect / nginx -T и проверяет DNS.
# Ваш прокси и ваш проект он не меняет. Результат — файлы в deploy/generated/ и
# список команд, которые вы выполняете сами.
set -euo pipefail

DOMAIN="${1:-}"
if [[ -z "$DOMAIN" ]]; then
  echo "Использование: $0 <поддомен>, например: $0 vcos.rsocit.ru" >&2
  exit 1
fi
cd "$(dirname "$0")"
OUT=generated
mkdir -p "$OUT"

ok()   { printf '  \033[32m✔\033[0m %s\n' "$*"; }
warn() { printf '  \033[33m!\033[0m %s\n' "$*"; }
fail() { printf '  \033[31m✘\033[0m %s\n' "$*"; }
step() { printf '\n\033[1m%s\033[0m\n' "$*"; }

# ---------------------------------------------------------------- DNS
step "1. DNS"
PUBLIC_IP="$(curl -4 -fsS --max-time 5 https://ifconfig.me 2>/dev/null || curl -4 -fsS --max-time 5 https://api.ipify.org 2>/dev/null || true)"
if [[ -z "$PUBLIC_IP" ]]; then
  PUBLIC_IP="$(hostname -I 2>/dev/null | awk '{print $1}')"
  warn "не удалось узнать внешний IP через интернет, взят адрес интерфейса: $PUBLIC_IP"
else
  ok "внешний IP сервера: $PUBLIC_IP"
fi
DNS_IP="$(getent ahostsv4 "$DOMAIN" 2>/dev/null | awk 'NR==1{print $1}' || true)"
if [[ -z "$DNS_IP" ]]; then
  fail "$DOMAIN не резолвится. Создайте A-запись: $DOMAIN → $PUBLIC_IP и дождитесь обновления DNS."
  DNS_OK=0
elif [[ "$DNS_IP" != "$PUBLIC_IP" ]]; then
  fail "$DOMAIN указывает на $DNS_IP, а сервер — $PUBLIC_IP. Исправьте A-запись."
  DNS_OK=0
else
  ok "$DOMAIN → $DNS_IP"
  DNS_OK=1
fi

# ---------------------------------------------------------------- ports
step "2. Порты сервиса"
for p in 8080/tcp 50000/udp 3478/udp 3478/tcp; do
  port="${p%/*}"; proto="${p#*/}"
  flag=-lnt; [[ "$proto" == udp ]] && flag=-lnu
  if ss $flag 2>/dev/null | awk '{print $4}' | grep -Eq "[:.]$port\$"; then
    fail "порт $p занят — поменяйте его в .env (APP_PORT, RTC_UDP_PORT)"
  else
    ok "порт $p свободен"
  fi
done

# ---------------------------------------------------------------- proxy
step "3. Прокси на 443"
PROXY="$(docker ps --filter publish=443 --format '{{.Names}}' 2>/dev/null | head -1 || true)"
if [[ -z "$PROXY" ]]; then
  fail "443 держит не Docker-контейнер. Пришлите вывод: sudo ss -ltnp '( sport = :443 )'"
  exit 1
fi
IMAGE="$(docker inspect "$PROXY" --format '{{.Config.Image}}')"
ok "контейнер: $PROXY (образ $IMAGE)"

if ! docker exec "$PROXY" nginx -v >/dev/null 2>&1; then
  fail "в контейнере не nginx. Пришлите вывод этого скрипта — подготовлю вариант для $IMAGE."
  exit 1
fi
ok "это nginx"

NETWORK="$(docker inspect "$PROXY" --format '{{range $k, $v := .NetworkSettings.Networks}}{{$k}}{{"\n"}}{{end}}' | grep -v '^$' | head -1)"
if [[ "$NETWORK" == "host" || "$NETWORK" == "bridge" ]]; then
  warn "контейнер в сети '$NETWORK': в ней Docker не даёт обращаться к контейнерам по имени. Пришлите вывод скрипта."
fi
ok "сеть прокси: $NETWORK"

# Host directory mounted at the given container path (exact match or parent).
mount_src() {
  docker inspect "$PROXY" --format '{{range .Mounts}}{{.Destination}}|{{.Source}}{{"\n"}}{{end}}' |
    awk -F'|' -v d="$1" '$1==d {print $2; exit}'
}
CONF_DIR="$(mount_src /etc/nginx/conf.d)"
if [[ -n "$CONF_DIR" ]]; then
  ok "папка конфигов на сервере: $CONF_DIR (в контейнере /etc/nginx/conf.d)"
else
  NGINX_DIR="$(mount_src /etc/nginx)"
  if [[ -n "$NGINX_DIR" ]]; then
    CONF_DIR="$NGINX_DIR/conf.d"
    ok "папка конфигов на сервере: $CONF_DIR"
  else
    fail "не нашёл смонтированную папку с конфигами nginx. Пришлите вывод:"
    echo "    docker inspect $PROXY --format '{{range .Mounts}}{{.Source}} -> {{.Destination}}{{println}}{{end}}'"
    exit 1
  fi
fi

NGINX_T="$(docker exec "$PROXY" nginx -T 2>/dev/null || true)"
if grep -q 'default_server' <<<"$NGINX_T"; then
  ok "у вашего nginx есть явный default_server"
else
  warn "явного default_server нет — поэтому наш файл называется zz-meet.conf (идёт последним)"
fi
if grep -Eq "server_name[^;]*\\b${DOMAIN//./\\.}\\b" <<<"$NGINX_T"; then
  fail "$DOMAIN уже упоминается в конфиге вашего nginx — проверьте, не занят ли поддомен"
fi

# ---------------------------------------------------------------- certificates
step "4. Сертификаты"
LE_DIR="$(mount_src /etc/letsencrypt)"
WEBROOT="$(grep -A4 'acme-challenge' <<<"$NGINX_T" | grep -m1 -Eo 'root[[:space:]]+[^;]+' | awk '{print $2}' || true)"
WEBROOT_SRC=""
[[ -n "$WEBROOT" ]] && WEBROOT_SRC="$(mount_src "$WEBROOT")"
CERTBOT="$(docker ps -a --format '{{.Names}} {{.Image}}' | awk '/certbot/ {print $1; exit}' || true)"

if [[ -n "$LE_DIR" ]]; then ok "/etc/letsencrypt прокси = $LE_DIR на сервере"; else warn "папка /etc/letsencrypt в прокси не смонтирована"; fi
if [[ -n "$WEBROOT" ]]; then ok "проверка Let's Encrypt через webroot: $WEBROOT (на сервере: ${WEBROOT_SRC:-не смонтирован})"; else warn "location /.well-known/acme-challenge/ в вашем nginx не найден"; fi
if [[ -n "$CERTBOT" ]]; then ok "контейнер certbot: $CERTBOT"; fi

CERT_READY=0
if [[ -n "$LE_DIR" && -n "$WEBROOT" && -n "$WEBROOT_SRC" ]]; then
  CERT_READY=1
else
  warn "автоматически подготовить выпуск сертификата не получится — пришлите вывод скрипта"
fi

# ---------------------------------------------------------------- files
step "5. Файлы в deploy/$OUT/"
# Phase 1: only port 80, so Let's Encrypt can verify the domain.
{
  echo "# Фаза 1: только порт 80 для проверки домена Let's Encrypt. Заменяется на zz-meet.conf после выпуска сертификата."
  echo "server {"
  echo "    listen 80;"
  echo "    server_name $DOMAIN;"
  if [[ -n "$WEBROOT" ]]; then
    echo "    location /.well-known/acme-challenge/ { root $WEBROOT; }"
  fi
  echo "    location / { return 404; }"
  echo "}"
} > "$OUT/zz-meet.http.conf"
ok "$OUT/zz-meet.http.conf (фаза 1)"

# Phase 2: full site.
sed -e "s/meet.example.com/$DOMAIN/g" nginx/meet-docker.conf > "$OUT/zz-meet.conf"
if [[ -n "$WEBROOT" ]]; then
  sed -i "s|    # location /.well-known/acme-challenge/ { root /var/www/certbot; }|    location /.well-known/acme-challenge/ { root $WEBROOT; }|" "$OUT/zz-meet.conf"
fi
ok "$OUT/zz-meet.conf (фаза 2, HTTPS)"

if [[ -f "$OUT/.env" ]]; then
  ok "$OUT/.env уже есть — не перезаписываю (там ваши пароли)"
else
  ADMIN_PASSWORD="$(openssl rand -base64 18 | tr -d '/+=' | cut -c1-16)"
  sed -e "s|^DOMAIN=.*|DOMAIN=$DOMAIN|" \
      -e "s|^PUBLIC_IP=.*|PUBLIC_IP=$PUBLIC_IP|" \
      -e "s|^POSTGRES_PASSWORD=.*|POSTGRES_PASSWORD=$(openssl rand -hex 24)|" \
      -e "s|^JWT_SECRET=.*|JWT_SECRET=$(openssl rand -hex 32)|" \
      -e "s|^TURN_SECRET=.*|TURN_SECRET=$(openssl rand -hex 32)|" \
      -e "s|^ADMIN_PASSWORD=.*|ADMIN_PASSWORD=$ADMIN_PASSWORD|" \
      .env.example > "$OUT/.env"
  echo "PROXY_NETWORK=$NETWORK" >> "$OUT/.env"
  chmod 600 "$OUT/.env"
  ok "$OUT/.env (секреты сгенерированы; пароль администратора admin: $ADMIN_PASSWORD)"
fi

# ---------------------------------------------------------------- plan
step "6. Что выполнить (по порядку)"
C="docker compose -f docker-compose.yml -f docker-compose.shared-proxy.yml"
cat <<EOF

  # 6.1 Запустить сервис (порты 80/443 он не занимает)
  cp $OUT/.env .env
  $C up -d --build
  curl -s http://127.0.0.1:\${APP_PORT:-8080}/healthz     # ожидается: ok

  # 6.2 Фаза 1: сайт на 80 для проверки домена
  sudo cp $OUT/zz-meet.http.conf $CONF_DIR/zz-meet.conf
  docker exec $PROXY nginx -t && docker exec $PROXY nginx -s reload
EOF
if [[ "$CERT_READY" == 1 ]]; then
cat <<EOF

  # 6.3 Выпустить сертификат только для $DOMAIN (существующие сертификаты не затрагиваются)
  docker run --rm -v $LE_DIR:/etc/letsencrypt -v $WEBROOT_SRC:$WEBROOT certbot/certbot \\
    certonly --webroot -w $WEBROOT -d $DOMAIN --non-interactive --agree-tos --register-unsafely-without-email
EOF
else
cat <<EOF

  # 6.3 Выпуск сертификата: пришлите вывод скрипта — подготовлю команду под вашу схему
EOF
fi
cat <<EOF

  # 6.4 Фаза 2: полный сайт с HTTPS
  sudo cp $OUT/zz-meet.conf $CONF_DIR/zz-meet.conf
  docker exec $PROXY nginx -t && docker exec $PROXY nginx -s reload

  # 6.5 Файрвол (если включён ufw; также у провайдера, если есть свой файрвол)
  sudo ufw allow 50000/udp && sudo ufw allow 3478 && sudo ufw allow 49160:49200/udp

  # Откат в любой момент:
  sudo rm $CONF_DIR/zz-meet.conf && docker exec $PROXY nginx -t && docker exec $PROXY nginx -s reload
  $C down
EOF
[[ "$DNS_OK" == 1 ]] || { echo; warn "Сначала исправьте DNS (шаг 1), иначе сертификат не выпустится."; }
