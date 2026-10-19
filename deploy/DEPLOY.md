# Деплой на сервер, где 80/443 уже заняты

Сервис не занимает порты 80 и 443. Он слушает только `127.0.0.1:8080`, а HTTPS обеспечивает
прокси, который уже работает на сервере. В прокси добавляется **отдельный сайт на отдельном
поддомене**. Существующие сайты остаются как есть: прокси различает их по имени домена.

```
Браузер ──443──► ваш nginx ─┬─ ваш-проект.ru    → ваш проект (без изменений)
                            └─ meet.ваш-домен.ru → 127.0.0.1:8080 (наш сервис)
Браузер ──UDP 50000───────────────────────────► наш сервис (звук и видео)
Браузер ──3478, 49160–49200──► coturn (только если прямой UDP недоступен)
```

## 0. Проверить, что нужные порты свободны

```bash
# Кто держит 80/443 (чтобы понять, какой у вас прокси)
sudo ss -ltnp '( sport = :80 or sport = :443 )'

# Эти порты должны быть свободны
sudo ss -lntup | grep -E ':(8080|50000|3478)\b' || echo "свободно"
```

Если какой-то порт занят, поменяйте его в `.env`: `APP_PORT`, `RTC_UDP_PORT`. Порт TURN 3478
меняется в `docker-compose.yml` в двух местах: `--listening-port` и `TURN_URLS`.

## 1. DNS

Создайте A-запись для поддомена, например `meet.example.com` → IP сервера.

## 2. Docker

Если Docker ещё не установлен:

```bash
curl -fsSL https://get.docker.com | sudo sh
```

## 3. Сервис

```bash
git clone <адрес репозитория> meet && cd meet/deploy
cp .env.example .env
nano .env
```

Заполните в `.env`:

| Параметр | Значение |
|---|---|
| `DOMAIN` | `meet.example.com` |
| `PUBLIC_IP` | публичный IP сервера (`curl -4 ifconfig.me`) |
| `POSTGRES_PASSWORD` | `openssl rand -hex 24` |
| `JWT_SECRET` | `openssl rand -hex 32` |
| `TURN_SECRET` | `openssl rand -hex 32` |
| `ADMIN_USERNAME`, `ADMIN_PASSWORD` | логин и пароль первого администратора |

```bash
docker compose up -d --build
docker compose ps                     # app, postgres, coturn в состоянии Up
curl -s http://127.0.0.1:8080/healthz # ok
```

Собственный Caddy (профиль `caddy`) **не запускается**, ваши 80/443 он не трогает.

## 4. Сайт в вашем прокси

### nginx

```bash
sudo cp nginx/meet.conf /etc/nginx/sites-available/meet.conf
sudo sed -i 's/meet.example.com/ВАШ_ПОДДОМЕН/' /etc/nginx/sites-available/meet.conf
sudo ln -s /etc/nginx/sites-available/meet.conf /etc/nginx/sites-enabled/
#  если папки sites-enabled нет: sudo cp nginx/meet.conf /etc/nginx/conf.d/

sudo nginx -t                 # проверка: при ошибке ничего не применяется
sudo systemctl reload nginx   # reload, не restart: текущие соединения не рвутся
sudo certbot --nginx -d ВАШ_ПОДДОМЕН
```

`certbot --nginx -d` выпускает сертификат только для указанного домена и правит только
блок с этим `server_name`.

Если `APP_PORT` не 8080, поправьте `proxy_pass` в `meet.conf`.

### Caddy (если ваш прокси Caddy)

Добавьте в свой Caddyfile отдельный блок и выполните `caddy reload`:

```
meet.example.com {
	reverse_proxy 127.0.0.1:8080
}
```

Если ваш Caddy работает в Docker, адрес `127.0.0.1` указывает на сам контейнер. Тогда
укажите `host.docker.internal:8080` (с `extra_hosts: ["host.docker.internal:host-gateway"]`)
или подключите контейнер к сети `deploy_default` и укажите `app:8080`.

### Traefik

Добавьте сервису `app` метки Traefik с правилом ``Host(`meet.example.com`)`` и подключите его к
сети Traefik. WebSocket Traefik проксирует без дополнительной настройки.

## 5. Файрвол

```bash
sudo ufw allow 50000/udp          # звук и видео
sudo ufw allow 3478               # TURN tcp+udp
sudo ufw allow 49160:49200/udp    # ретрансляция TURN
```

80/443 уже открыты для вашего проекта. Если у провайдера есть свой файрвол (группы
безопасности), откройте эти порты и там.

## 6. Проверка

1. Откройте `https://ВАШ_ПОДДОМЕН` и войдите администратором.
2. Создайте пользователя, войдите им в другом браузере, создайте комнату.
3. Откройте ссылку-приглашение с телефона через мобильный интернет (другая сеть) и проверьте звук и видео.
4. Проверьте, что ваш основной проект открывается как раньше.

## Обновление

```bash
cd meet && git pull && cd deploy && docker compose up -d --build
```

## Откат

Сервис полностью отдельный. Чтобы убрать его:

```bash
sudo rm /etc/nginx/sites-enabled/meet.conf && sudo nginx -t && sudo systemctl reload nginx
cd meet/deploy && docker compose down   # с флагом -v удалится и база
```
