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
trap 'fail "скрипт остановился на строке $LINENO. Пришлите его вывод целиком."' ERR

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

HAVE_IMAGE=1
if docker image inspect "${APP_IMAGE:-vcos-app:latest}" >/dev/null 2>&1; then
  ok "образ ${APP_IMAGE:-vcos-app:latest} загружен"
else
  HAVE_IMAGE=0
  warn "образа ${APP_IMAGE:-vcos-app:latest} пока нет — команда сборки будет в плане ниже"
fi

# ---------------------------------------------------------------- proxy
step "3. Прокси на 443"
PROXY="$(docker ps --filter publish=443 --format '{{.Names}}' 2>/dev/null | head -1 || true)"
if [[ -z "$PROXY" ]]; then
  fail "443 держит не Docker-контейнер. Пришлите вывод: sudo ss -ltnp '( sport = :443 )'"
  exit 1
fi
IMAGE="$(docker inspect "$PROXY" --format '{{.Config.Image}}')"
ok "контейнер: $PROXY (образ $IMAGE)"

NETWORK="$(docker inspect "$PROXY" --format '{{range $k, $v := .NetworkSettings.Networks}}{{$k}}{{"\n"}}{{end}}' | grep -v '^$' | head -1)"
if [[ "$NETWORK" == "host" || "$NETWORK" == "bridge" ]]; then
  warn "контейнер в сети '$NETWORK': в ней Docker не даёт обращаться к контейнерам по имени. Пришлите вывод скрипта."
fi
ok "сеть прокси: $NETWORK"

# Host path mounted at the given container path (exact match).
mount_src() {
  docker inspect "$PROXY" --format '{{range .Mounts}}{{.Destination}}|{{.Source}}{{"\n"}}{{end}}' |
    awk -F'|' -v d="$1" '$1==d {print $2; exit}'
}

# .env with generated secrets; an existing one is kept (it holds the passwords in use).
write_env() {
  if [[ -f "$OUT/.env" ]]; then
    ok "$OUT/.env уже есть — не перезаписываю (там ваши пароли)"
    return
  fi
  local admin_password
  admin_password="$(openssl rand -base64 18 | tr -d '/+=' | cut -c1-16)"
  sed -e "s|^DOMAIN=.*|DOMAIN=$DOMAIN|" \
      -e "s|^PUBLIC_IP=.*|PUBLIC_IP=$PUBLIC_IP|" \
      -e "s|^POSTGRES_PASSWORD=.*|POSTGRES_PASSWORD=$(openssl rand -hex 24)|" \
      -e "s|^JWT_SECRET=.*|JWT_SECRET=$(openssl rand -hex 32)|" \
      -e "s|^TURN_SECRET=.*|TURN_SECRET=$(openssl rand -hex 32)|" \
      -e "s|^ADMIN_PASSWORD=.*|ADMIN_PASSWORD=$admin_password|" \
      .env.example > "$OUT/.env"
  echo "PROXY_NETWORK=$NETWORK" >> "$OUT/.env"
  chmod 600 "$OUT/.env"
  ok "$OUT/.env (секреты сгенерированы; пароль администратора admin: $admin_password)"
}

C="docker compose -f docker-compose.yml -f docker-compose.shared-proxy.yml"
plan_start() {
  echo
  if [[ "$HAVE_IMAGE" != 1 ]]; then
    echo "  # 6.0 Собрать образ (1–3 минуты)"
    echo "  (cd .. && docker build -t vcos-app:latest -f deploy/Dockerfile .)"
    echo
  fi
  cat <<EOF
  # 6.1 Запустить сервис (порты 80/443 он не занимает)
  cp $OUT/.env .env
  $C up -d
  curl -s http://127.0.0.1:\${APP_PORT:-8080}/healthz     # ожидается: ok
EOF
}

# ---------------------------------------------------------------- Caddy
if docker exec "$PROXY" caddy version >/dev/null 2>&1; then
  ok "это Caddy $(docker exec "$PROXY" caddy version 2>/dev/null | awk '{print $1}')"
  ARGS="$(docker inspect "$PROXY" --format '{{join .Config.Entrypoint " "}} {{join .Config.Cmd " "}}')"
  CONFIG_IN="$(grep -Eo -- '--config[ =][^ ]+' <<<"$ARGS" | head -1 | sed -E 's/--config[ =]//' || true)"
  CONFIG_IN="${CONFIG_IN:-/etc/caddy/Caddyfile}"
  if grep -Eq -- '--adapter[ =]json' <<<"$ARGS" || [[ "$CONFIG_IN" == *.json ]]; then
    fail "Caddy работает с JSON-конфигом ($CONFIG_IN). Пришлите вывод скрипта."
    exit 1
  fi
  CADDYFILE="$(mount_src "$CONFIG_IN")"
  if [[ -z "$CADDYFILE" ]]; then
    DIR="$(mount_src "$(dirname "$CONFIG_IN")")"
    [[ -n "$DIR" ]] && CADDYFILE="$DIR/$(basename "$CONFIG_IN")"
  fi
  if [[ -z "$CADDYFILE" || ! -f "$CADDYFILE" ]]; then
    fail "не нашёл на сервере файл $CONFIG_IN. Пришлите вывод:"
    echo "    docker inspect $PROXY --format '{{range .Mounts}}{{.Source}} -> {{.Destination}}{{println}}{{end}}'"
    exit 1
  fi
  ok "Caddyfile на сервере: $CADDYFILE (в контейнере $CONFIG_IN)"
  SUDO=""; [[ -w "$CADDYFILE" ]] || SUDO="sudo "
  if grep -Fq "$DOMAIN" "$CADDYFILE"; then
    fail "$DOMAIN уже упоминается в Caddyfile — проверьте, не занят ли поддомен"
    exit 1
  fi
  if grep -Eq '^[[:space:]]*admin[[:space:]]+off' "$CADDYFILE"; then
    fail "в Caddyfile выключен admin API (admin off): перечитать конфиг без перезапуска Caddy нельзя. Пришлите вывод скрипта."
    exit 1
  fi
  # A first HTTPS site makes Caddy redirect every other host on port 80 to HTTPS (checked on a test Caddy).
  if docker exec "$PROXY" caddy adapt --config "$CONFIG_IN" --adapter caddyfile 2>/dev/null | grep -q '":443"'; then
    ok "в Caddy уже есть HTTPS-сайты — для ваших сайтов ничего не изменится"
  else
    warn "в Caddy сейчас нет HTTPS-сайтов. После добавления нашего блока запросы на порт 80 по IP и по чужим доменам"
    warn "будут перенаправляться на HTTPS (ваши http://-сайты продолжат отвечать как раньше). Если это важно — напишите."
  fi
  ok "Caddy сам выпустит сертификат для $DOMAIN (certbot не нужен)"

  step "4. Файлы в deploy/$OUT/"
  cat > "$OUT/vcos.caddy" <<EOF

# --- vcos: сервис видеоконференций (начало) ---
$DOMAIN {
	reverse_proxy meet-app:8080
}
# --- vcos (конец) ---
EOF
  ok "$OUT/vcos.caddy — блок, который допишется в конец Caddyfile"
  write_env

  step "5. Что выполнить (по порядку)"
  plan_start
  V="docker exec $PROXY caddy validate --config $CONFIG_IN --adapter caddyfile"
  R="docker exec $PROXY caddy reload --config $CONFIG_IN --adapter caddyfile"
  cat <<EOF

  # 6.2 Резервная копия вашего Caddyfile
  ${SUDO}cp $CADDYFILE $CADDYFILE.bak-vcos

  # 6.3 Дописать наш блок в конец. Только через >> (tee -a): редактор или sed -i создают новый
  #     файл, и контейнер с подключённым файлом продолжит видеть старый.
  cat $OUT/vcos.caddy | ${SUDO}tee -a $CADDYFILE >/dev/null

  # 6.4 Проверить и перечитать конфиг без перезапуска (при ошибке Caddy оставит старый конфиг)
  $V && $R

  # 6.5 Проверка: сертификат выпускается 10–60 секунд
  docker logs $PROXY --since 5m 2>&1 | grep -i "$DOMAIN" | tail -5
  curl -s https://$DOMAIN/healthz          # ожидается: ok

  # 6.6 Файрвол (если включён ufw; также у провайдера, если есть свой файрвол)
  sudo ufw allow 50000/udp && sudo ufw allow 3478 && sudo ufw allow 49160:49200/udp

  # Откат в любой момент (содержимое возвращается в тот же файл):
  cat $CADDYFILE.bak-vcos | ${SUDO}tee $CADDYFILE >/dev/null && $R
  $C down
EOF
  exit 0
fi

if ! docker exec "$PROXY" nginx -v >/dev/null 2>&1; then
  fail "в контейнере не nginx и не Caddy. Пришлите вывод этого скрипта — подготовлю вариант для $IMAGE."
  exit 1
fi
ok "это nginx"
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
# First "root" after the ACME location (a line may hold several locations, so take one match only).
WEBROOT="$(grep -A4 'acme-challenge' <<<"$NGINX_T" | grep -Eo 'root[[:space:]]+[^;]+' | head -1 | awk '{print $2}' || true)"
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

write_env

# ---------------------------------------------------------------- plan
step "6. Что выполнить (по порядку)"
plan_start
cat <<EOF

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
