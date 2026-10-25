# Деплой

## Отдельный сервер (порты 80/443 свободны) — рекомендуемый вариант

```bash
git clone <репозиторий> vcos && cd vcos/deploy
./setup-standalone.sh vcos.example.ru
```

Скрипт ничего не удаляет. Он проверяет Docker, свободные порты и DNS (A и AAAA), создаёт `.env`
с секретами, собирает образ, запускает сервис со встроенным Caddy (сертификат Let's Encrypt
выпускается автоматически) и в конце запускает `check.sh`. Пароль администратора он показывает
один раз. Повторный запуск обновляет сервис и сохраняет `.env` и базу.

У провайдера откройте входящие порты: TCP 22, 80, 443 · UDP 443 · UDP 50000 · TCP+UDP 3478 ·
UDP 49160–49200.

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

## Быстрый путь: скрипт подготовки

Если 80/443 держит nginx в Docker-контейнере:

```bash
cd deploy && ./prepare-proxy.sh vcos.example.ru
```

Скрипт только читает (docker ps / inspect / `nginx -T`, DNS) и ничего не меняет в вашем прокси.
Он проверяет DNS и свободные порты, находит сеть, папку конфигов и webroot для Let's Encrypt,
создаёт в `deploy/generated/` файл `.env` с секретами и два конфига nginx (фаза 1 — только 80 для
проверки домена, фаза 2 — HTTPS) и печатает команды по порядку, включая откат.

Ниже — то же самое вручную.

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

> **Почему файл называется `zz-meet.conf`.** Если ни один сайт в вашем nginx не помечен
> `default_server`, nginx отдаёт запросы по IP и по незнакомым доменам **первому** сайту по
> алфавиту имён файлов. Файл `meet.conf` мог бы встать раньше вашего (например `site.conf`) и
> перехватить такие запросы. С именем `zz-...` он всегда последний. Это проверено на стенде.

Если `ss` показывает на 80/443 процесс `docker-proxy`, ваш прокси работает в Docker:
переходите к разделу **«Прокси в Docker-контейнере»**. Если там `nginx`, `caddy` и т.п. без
Docker, подойдут разделы ниже.

### Прокси в Docker-контейнере

Для прокси в контейнере `127.0.0.1` указывает на сам контейнер, поэтому сервис подключается к
**существующей Docker-сети прокси** и доступен в ней по имени `meet-app`. Compose-файлы вашего
проекта при этом не меняются.

1. Найдите контейнер, сеть и папку с конфигами прокси:

   ```bash
   docker ps --format 'table {{.Names}}\t{{.Image}}\t{{.Ports}}'
   docker inspect <контейнер> --format '{{range $k, $v := .NetworkSettings.Networks}}{{$k}} {{end}}'
   docker inspect <контейнер> --format '{{range .Mounts}}{{.Source}} -> {{.Destination}}{{println}}{{end}}'
   ```

2. Добавьте в `deploy/.env` строку `PROXY_NETWORK=<сеть из второй команды>` и запустите сервис с
   подключением к этой сети:

   ```bash
   docker compose -f docker-compose.yml -f docker-compose.shared-proxy.yml up -d --build
   ```

3. **nginx в контейнере.** Положите `nginx/meet-docker.conf` под именем **`zz-meet.conf`** (с вашим
   поддоменом и путями к сертификату) в папку, которая смонтирована в `/etc/nginx/conf.d`, затем:

   ```bash
   docker exec <контейнер> nginx -t && docker exec <контейнер> nginx -s reload
   ```

   В этом конфиге адрес `meet-app` определяется при каждом запросе, а не при старте nginx.
   Поэтому если сервис конференций остановлен, ваш nginx всё равно запустится, а поддомен
   конференций будет отвечать 502. С обычным `proxy_pass http://meet-app:8080` nginx бы не
   прошёл проверку конфигурации и не поднялся после перезапуска контейнера.

   Сертификат для поддомена выпускается тем же способом, что и для ваших сайтов (certbot с
   webroot, отдельный контейнер certbot и т.п.).

4. **Traefik.** Добавьте сервису `app` метки (например в `docker-compose.shared-proxy.yml`):

   ```yaml
   labels:
     - traefik.enable=true
     - traefik.docker.network=<сеть прокси>
     - traefik.http.routers.meet.rule=Host(`meet.example.com`)
     - traefik.http.routers.meet.entrypoints=websecure
     - traefik.http.routers.meet.tls.certresolver=<ваш resolver>
     - traefik.http.services.meet.loadbalancer.server.port=8080
   ```

5. **Caddy в контейнере.** `./prepare-proxy.sh <поддомен>` распознаёт Caddy и выводит готовые
   команды. Суть:
   * в **конец** вашего Caddyfile дописывается блок `<поддомен> { reverse_proxy meet-app:8080 }`,
     предварительно делается резервная копия;
   * дописывать нужно через `tee -a`: редактор или `sed -i` создают новый файл, и контейнер, к
     которому Caddyfile подключён отдельным файлом, продолжит видеть старый;
   * `caddy validate` и `caddy reload` применяют конфиг без перезапуска, при ошибке Caddy
     остаётся на старом конфиге;
   * сертификат для поддомена Caddy выпускает сам;
   * адрес `meet-app` Caddy ищет при каждом запросе: если сервис конференций остановлен, Caddy
     всё равно стартует, а поддомен отвечает 502.

   Проверено на стенде с `caddy:2.10-alpine`: если в Caddy уже есть HTTPS-сайты, их поведение не
   меняется. Если были только `http://`-сайты, после первого HTTPS-сайта Caddy начинает
   перенаправлять запросы на порт 80 по IP и чужим доменам на HTTPS. Скрипт проверяет это и
   предупреждает.

### nginx

```bash
sudo cp nginx/meet.conf /etc/nginx/sites-available/zz-meet.conf
sudo sed -i 's/meet.example.com/ВАШ_ПОДДОМЕН/' /etc/nginx/sites-available/zz-meet.conf
sudo ln -s /etc/nginx/sites-available/zz-meet.conf /etc/nginx/sites-enabled/
#  если папки sites-enabled нет: sudo cp nginx/meet.conf /etc/nginx/conf.d/zz-meet.conf

sudo nginx -t                 # проверка: при ошибке ничего не применяется
sudo systemctl reload nginx   # reload, не restart: текущие соединения не рвутся
sudo certbot --nginx -d ВАШ_ПОДДОМЕН
```

`certbot --nginx -d` выпускает сертификат только для указанного домена и правит только
блок с этим `server_name`.

Если `APP_PORT` не 8080, поправьте `proxy_pass` в `zz-meet.conf`.

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

### Вариант без прокси: отдельный порт (например 8443)

Сервис сам отдаёт HTTPS на своём порту, адрес будет `https://домен:8443`. Ни порты 80/443,
ни прокси вашего проекта не используются.

Браузер даёт доступ к камере и микрофону только по HTTPS с настоящим сертификатом. Let's Encrypt
обычно проверяет домен через порты 80/443, поэтому сертификат берётся одним из способов:

* **Уже выпущенный сертификат вашего домена.** Если у проекта есть сертификат для `example.com`,
  сервис можно открыть на `https://example.com:8443` с тем же сертификатом: сертификат привязан к
  имени, а не к порту. Продлевает его ваш проект как раньше, сервис подхватывает новый файл
  сам, без перезапуска (проверка раз в минуту).
* **Выпуск через DNS** (DNS-01, например acme.sh или certbot с плагином вашего DNS-провайдера).
  Порты 80/443 для него не нужны, но нужен API-ключ DNS-провайдера.

Настройка в `deploy/.env`:

```
DOMAIN=example.com              # имя, на которое выписан сертификат
HTTPS_PORT=8443
TLS_CERT_DIR=/etc/letsencrypt   # папка целиком: в live/ лежат ссылки на ../../archive
TLS_CERT_PATH=live/example.com/fullchain.pem
TLS_KEY_PATH=live/example.com/privkey.pem
APP_USER=0:0                    # ключи в /etc/letsencrypt читает только root
```

Если certbot вашего проекта работает в Docker, `TLS_CERT_DIR` — это папка на сервере, которая
смонтирована в его контейнер как `/etc/letsencrypt` (её покажет `docker inspect`).

Чтобы не запускать сервис от root, копируйте сертификат в отдельную папку после каждого продления
(`--deploy-hook` у certbot) и выдайте права пользователю `65532`. Тогда `APP_USER` не нужен.

```bash
docker compose -f docker-compose.yml -f docker-compose.own-port.yml up -d --build
sudo ufw allow 8443/tcp
```

Минус этого варианта: в адресе виден порт, а в некоторых корпоративных и гостевых сетях
нестандартные порты закрыты. Звук и видео идут по UDP 50000 и через TURN 3478 в любом варианте.

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

## Проверка работоспособности

```bash
cd deploy && ./check.sh
```

Скрипт только читает и ничего не меняет. Он проверяет контейнеры, приложение и базу, связь прокси
с сервисом, HTTPS и сертификат, WebSocket, DNS (A и AAAA), UDP-порт медиа, TURN (UDP и TCP, а
также что чужой пароль не принимается), файрвол и ваши остальные сайты на том же прокси. Для
каждой ошибки он подсказывает, что делать дальше.

## Обновление

```bash
cd meet && git pull && cd deploy && docker compose up -d --build
```

## Откат

Сервис полностью отдельный. Чтобы убрать его:

```bash
sudo rm /etc/nginx/sites-enabled/zz-meet.conf && sudo nginx -t && sudo systemctl reload nginx
cd meet/deploy && docker compose down   # с флагом -v удалится и база
```
