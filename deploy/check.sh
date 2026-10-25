#!/usr/bin/env bash
# Проверка работоспособности сервиса конференций. Только читает, ничего не меняет.
#
#   cd deploy && ./check.sh
#
# Берёт настройки из deploy/.env. В конце печатает итог: сколько проверок прошло,
# сколько предупреждений и ошибок.
set -uo pipefail
cd "$(dirname "$0")"

PASS=0; WARN=0; FAIL=0
ok()   { PASS=$((PASS+1)); printf '  \033[32m✔\033[0m %s\n' "$*"; }
warn() { WARN=$((WARN+1)); printf '  \033[33m!\033[0m %s\n' "$*"; }
fail() { FAIL=$((FAIL+1)); printf '  \033[31m✘\033[0m %s\n' "$*"; }
hint() { printf '      → %s\n' "$*"; }
step() { printf '\n\033[1m%s\033[0m\n' "$*"; }

if [[ ! -f .env ]]; then
  echo "Нет файла deploy/.env — сервис ещё не настроен (см. prepare-proxy.sh)." >&2
  exit 1
fi
envval() { grep -E "^$1=" .env | tail -1 | cut -d= -f2- ; }
DOMAIN="$(envval DOMAIN)"
PUBLIC_IP="$(envval PUBLIC_IP)"
APP_PORT="$(envval APP_PORT)"; APP_PORT="${APP_PORT:-8080}"
RTC_UDP_PORT="$(envval RTC_UDP_PORT)"; RTC_UDP_PORT="${RTC_UDP_PORT:-50000}"
TURN_SECRET="$(envval TURN_SECRET)"
COMPOSE=(docker compose -f docker-compose.yml)
[[ -n "$(envval PROXY_NETWORK)" ]] && COMPOSE+=(-f docker-compose.shared-proxy.yml)
echo "Сервис: https://$DOMAIN   (сервер $PUBLIC_IP)"

# ---------------------------------------------------------------------------
step "1. Контейнеры"
for svc in app postgres coturn; do
  state="$("${COMPOSE[@]}" ps --format '{{.Service}} {{.State}} {{.Health}}' 2>/dev/null | awk -v s="$svc" '$1==s {print $2, $3}')"
  case "$state" in
    "running healthy"|"running ") ok "$svc работает" ;;
    running*)                    warn "$svc запущен, состояние: $state" ;;
    "")                          fail "$svc не запущен"; hint "${COMPOSE[*]} up -d" ;;
    *)                           fail "$svc: $state"; hint "${COMPOSE[*]} logs $svc | tail -30" ;;
  esac
done
APP_ID="$("${COMPOSE[@]}" ps -q app 2>/dev/null)"
restarts="$( [[ -n "$APP_ID" ]] && docker inspect "$APP_ID" --format '{{.RestartCount}}' 2>/dev/null || echo 0)"
[[ "$restarts" -gt 0 ]] && warn "app перезапускался $restarts раз(а)" && hint "${COMPOSE[*]} logs app | grep -i error | tail"

# ---------------------------------------------------------------------------
step "2. Приложение и база"
if [[ "$(curl -s --max-time 5 "http://127.0.0.1:$APP_PORT/healthz")" == "ok" ]]; then
  ok "приложение отвечает и видит базу (127.0.0.1:$APP_PORT/healthz)"
else
  fail "приложение не отвечает на 127.0.0.1:$APP_PORT"; hint "${COMPOSE[*]} logs app | tail -30"
fi
counts="$("${COMPOSE[@]}" exec -T postgres psql -U meet -d meet -tAc \
  "select (select count(*) from users), (select count(*) from rooms where status='active'), (select count(*) from participants where admitted_at is not null and left_at is null)" 2>/dev/null | tr '|' ' ')"
if [[ -n "$counts" ]]; then
  read -r users rooms online <<<"$counts"
  ok "база: пользователей $users, активных комнат $rooms, сейчас в звонках $online"
else
  fail "не удалось выполнить запрос к базе"
fi
errors="$("${COMPOSE[@]}" logs app --since 1h 2>/dev/null | grep -c '"level":"ERROR"')"
if [[ "$errors" -eq 0 ]]; then
  ok "ошибок в логе приложения за час нет"
else
  warn "ошибок в логе приложения за час: $errors"
  "${COMPOSE[@]}" logs app --since 1h 2>/dev/null | grep '"level":"ERROR"' | tail -3 | sed 's/^/      /'
fi

# ---------------------------------------------------------------------------
step "3. Прокси (HTTPS)"
PROXY="$(docker ps --filter publish=443 --format '{{.Names}}' | head -1)"
if [[ -z "$PROXY" ]]; then
  warn "контейнер на 443 не найден — прокси вне Docker, проверки 3.x частично пропущены"
else
  # Our own Caddy (profile "caddy") reaches the app as "app"; a shared proxy as "meet-app".
  OWN_PROXY=0; UPSTREAM=meet-app
  if [[ "$(docker inspect "$PROXY" --format '{{index .Config.Labels "com.docker.compose.project.working_dir"}}')" == "$(pwd)" ]]; then
    OWN_PROXY=1; UPSTREAM=app
  fi
  ok "прокси: $PROXY$([[ $OWN_PROXY == 1 ]] && echo ' (собственный Caddy сервиса)')"
  if [[ "$(docker exec "$PROXY" wget -qO- -T 5 "http://$UPSTREAM:8080/healthz" 2>/dev/null)" == "ok" ]]; then
    ok "прокси видит сервис по имени $UPSTREAM"
  else
    fail "прокси не достаёт до $UPSTREAM:8080"
    if [[ -z "$APP_ID" ]]; then
      hint "приложение не запущено — см. пункт 1"
    else
      hint "сервис должен быть в сети прокси: docker inspect $APP_ID --format '{{range \$k,\$v := .NetworkSettings.Networks}}{{\$k}} {{end}}'"
    fi
  fi
fi

# Ask the proxy on this machine directly, bypassing DNS and NAT.
LOCAL=(--resolve "$DOMAIN:443:127.0.0.1")
if [[ "$(curl -s --max-time 10 "${LOCAL[@]}" "https://$DOMAIN/healthz")" == "ok" ]]; then
  ok "https://$DOMAIN/healthz через прокси отвечает ok"
else
  fail "https://$DOMAIN через прокси не отвечает"
  hint "проверьте блок $DOMAIN в конфиге прокси и логи прокси"
fi
cert="$(echo | openssl s_client -connect 127.0.0.1:443 -servername "$DOMAIN" 2>/dev/null | openssl x509 -noout -issuer -enddate 2>/dev/null)"
issuer="$(sed -n 's/^issuer=//p' <<<"$cert")"
end="$(sed -n 's/^notAfter=//p' <<<"$cert")"
if [[ -z "$cert" ]]; then
  fail "не удалось получить сертификат $DOMAIN"
elif grep -qi "let's encrypt\|O = Let's Encrypt\|ZeroSSL" <<<"$issuer"; then
  days=$(( ( $(date -d "$end" +%s) - $(date +%s) ) / 86400 ))
  if [[ "$days" -ge 14 ]]; then ok "сертификат действителен ещё $days дн. ($issuer)"
  else warn "сертификат истекает через $days дн. — прокси должен продлить его сам"; fi
else
  fail "сертификат не от публичного центра: $issuer"
  hint "прокси ещё не выпустил сертификат: docker logs $PROXY --since 1h 2>&1 | grep -i $DOMAIN | tail"
fi
code="$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 --http1.1 "${LOCAL[@]}" \
  -H 'Connection: Upgrade' -H 'Upgrade: websocket' -H 'Sec-WebSocket-Version: 13' \
  -H 'Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==' -H "Origin: https://$DOMAIN" \
  "https://$DOMAIN/ws/rooms/check")"
if [[ "$code" == "101" ]]; then ok "WebSocket проходит через прокси (комнаты и чат)"
else fail "WebSocket через прокси не открывается (код $code)"; fi

# ---------------------------------------------------------------------------
step "4. DNS и доступ извне"
A="$(getent ahostsv4 "$DOMAIN" | awk 'NR==1{print $1}')"
if [[ "$A" == "$PUBLIC_IP" ]]; then ok "A: $DOMAIN → $A"
else fail "A-запись $DOMAIN = ${A:-нет}, а сервер $PUBLIC_IP"; fi
AAAA="$(getent ahostsv6 "$DOMAIN" 2>/dev/null | awk '{print $1}' | grep -v '^::ffff:' | sort -u | head -1)"
if [[ -z "$AAAA" ]]; then
  ok "AAAA-записи нет (используется только IPv4)"
elif ip -6 addr 2>/dev/null | grep -q "$AAAA"; then
  ok "AAAA: $DOMAIN → $AAAA (адрес этого сервера)"
else
  fail "AAAA: $DOMAIN → $AAAA — это не адрес этого сервера; клиенты с IPv6 попадут не туда"
  hint "удалите AAAA-запись для $DOMAIN в панели DNS"
fi
if [[ "$(curl -s --max-time 10 --resolve "$DOMAIN:443:$PUBLIC_IP" "https://$DOMAIN/healthz")" == "ok" ]]; then
  ok "сайт отвечает по публичному IP $PUBLIC_IP"
else
  warn "по публичному IP с самого сервера сайт не открывается"
  hint "если IP пробрасывает провайдер, это нормально: проверьте с телефона через мобильный интернет"
fi

# ---------------------------------------------------------------------------
step "5. Звук и видео"
udp_listening() {
  if command -v ss >/dev/null; then
    ss -lnu | awk '{print $4}' | grep -Eq "[:.]$RTC_UDP_PORT\$"
  else
    [[ -n "$APP_ID" ]] && docker port "$APP_ID" "$RTC_UDP_PORT/udp" 2>/dev/null | grep -q .
  fi
}
if udp_listening; then
  ok "UDP $RTC_UDP_PORT слушается (медиапотоки)"
else
  fail "UDP $RTC_UDP_PORT не слушается"; hint "${COMPOSE[*]} logs app | grep -i media"
fi
media="$("${COMPOSE[@]}" logs app 2>/dev/null | grep '"msg":"media enabled"' | tail -1)"
if grep -q "\"public_ip\":\"$PUBLIC_IP\"" <<<"$media"; then ok "в WebRTC-кандидатах публичный IP $PUBLIC_IP"
else warn "приложение объявляет другой IP: $(grep -o '"public_ip":"[^"]*"' <<<"$media")"; fi

turn_check() { # $1 = extra flags, $2 = secret; prints yes if a relay was allocated
  local out
  out="$(timeout 12 docker run --rm --network host --entrypoint turnutils_uclient coturn/coturn:4.6 \
    $1 -W "$2" -u check -n 1 -m 1 -c -e 203.0.113.1 127.0.0.1 2>&1)"
  if grep -q "Cannot complete Allocation" <<<"$out" || ! grep -q "Total connect time" <<<"$out"; then
    echo no
  else
    echo yes
  fi
}
if [[ -n "$TURN_SECRET" ]]; then
  [[ "$(turn_check "" "$TURN_SECRET")" == yes ]] && ok "TURN (UDP 3478) выдаёт relay-адрес" || fail "TURN по UDP не работает"
  [[ "$(turn_check "-t" "$TURN_SECRET")" == yes ]] && ok "TURN (TCP 3478) выдаёт relay-адрес" || fail "TURN по TCP не работает"
  [[ "$(turn_check "" "wrong-secret")" == no ]] && ok "TURN отклоняет чужой пароль" || fail "TURN принимает неверный пароль"
else
  warn "TURN_SECRET не задан — TURN не проверялся"
fi

if command -v ufw >/dev/null && sudo -n true 2>/dev/null; then
  if sudo ufw status | grep -q "Status: active"; then
    rules="$(sudo ufw status)"
    for r in "$RTC_UDP_PORT/udp" "3478" "49160:49200/udp"; do
      grep -q "^$r " <<<"$rules" && ok "ufw: открыт $r" || { fail "ufw: закрыт $r"; hint "sudo ufw allow $r"; }
    done
  else
    ok "ufw выключен"
  fi
else
  warn "ufw не проверен (нужен sudo без пароля). Проверьте вручную: sudo ufw status"
fi

# ---------------------------------------------------------------------------
if [[ -n "${PROXY:-}" && "${OWN_PROXY:-0}" == 0 ]]; then
  step "6. Ваши сайты на том же прокси"
  CF="$(docker inspect "$PROXY" --format '{{range .Mounts}}{{if eq .Destination "/etc/caddy/Caddyfile"}}{{.Source}}{{end}}{{end}}')"
  if [[ -n "$CF" && -r "$CF" ]]; then
    for site in $(grep -E '^[a-zA-Z0-9*][a-zA-Z0-9.*,: -]*\{[[:space:]]*$' "$CF" | tr -d '{,' | tr ' ' '\n' | grep '\.' | grep -v '^http://' | grep -vx "$DOMAIN" | sort -u); do
      host="${site#https://}"; host="${host%%:*}"
      [[ "$host" == *"*"* ]] && continue
      code="$(curl -s -o /dev/null -w '%{http_code}' --max-time 10 --resolve "$host:443:127.0.0.1" "https://$host/")"
      if [[ "$code" =~ ^[23] ]]; then ok "$host отвечает ($code)"
      else warn "$host: код $code — сравните с тем, как он отвечал до установки"; fi
    done
  else
    warn "Caddyfile прокси не найден — проверьте свои сайты в браузере"
  fi
fi

# ---------------------------------------------------------------------------
printf '\n\033[1mИтог:\033[0m \033[32m%d ✔\033[0m  \033[33m%d !\033[0m  \033[31m%d ✘\033[0m\n' "$PASS" "$WARN" "$FAIL"
[[ "$FAIL" -eq 0 ]] && echo "Серверная часть в порядке. Осталось проверить звонок в браузере с двух устройств."
exit $(( FAIL > 0 ))
