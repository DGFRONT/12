#!/usr/bin/env bash
# Установка сервиса конференций на чистый сервер из архива vcos-deploy.tar.gz одной командой:
#
#   tar xzf vcos-deploy.tar.gz && cd vcos && ./install.sh vcos.example.ru
#
# Делает: ставит Docker (если нет), добавляет 2 ГБ подкачки (если памяти меньше 2 ГБ и
# подкачки нет), загружает готовый образ из архива и запускает deploy/setup-standalone.sh.
# Ничего не удаляет.
set -euo pipefail

DOMAIN="${1:-}"
if [[ -z "$DOMAIN" ]]; then
  echo "Использование: $0 <домен>, например: $0 vcos.rsocit.ru" >&2
  exit 1
fi
cd "$(dirname "$0")"
[[ "$(id -u)" == 0 ]] || exec sudo bash "$0" "$@"

say() { printf '\n\033[1m%s\033[0m\n' "$*"; }

say "Docker"
if command -v docker >/dev/null; then
  echo "  уже установлен: $(docker --version)"
else
  curl -fsSL https://get.docker.com | sh
fi

say "Память"
mem=$(awk '/MemTotal/ {print int($2/1024)}' /proc/meminfo)
if [[ "$mem" -lt 2000 && -z "$(swapon --noheadings 2>/dev/null)" ]]; then
  fallocate -l 2G /swapfile && chmod 600 /swapfile && mkswap /swapfile >/dev/null && swapon /swapfile
  grep -q '^/swapfile ' /etc/fstab || echo '/swapfile none swap sw 0 0' >> /etc/fstab
  echo "  памяти ${mem} МБ — добавлена подкачка 2 ГБ"
else
  echo "  памяти ${mem} МБ, подкачка: $(swapon --noheadings --show=SIZE 2>/dev/null | head -1 || echo нет)"
fi

if [[ -f vcos-app.tar.gz ]]; then
  say "Образ приложения"
  docker load -i vcos-app.tar.gz
  export NO_BUILD=1
fi

# In the archive this script sits next to deploy/, in the repository inside it.
if [[ -f deploy/setup-standalone.sh ]]; then exec ./deploy/setup-standalone.sh "$DOMAIN"; fi
exec ./setup-standalone.sh "$DOMAIN"
