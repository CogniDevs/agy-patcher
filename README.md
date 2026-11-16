# agy-patcher

<details open>
<summary><b>English</b></summary>

Utility for configuring Antigravity (Desktop, CLI `agy`, VS Code editor extensions, and JetBrains plugin) to route through a local bypass proxy on Windows, Linux, and macOS.

## Features

- **Automatic Installation Discovery**: Scans and discovers Antigravity clients and plugins across the OS; custom paths can be added via the TUI panel or `--path` flag.
- **Patch and Rollback**: Enables network routing for selected installations and supports full rollback from automated backup archives.
- **Local Proxy & Smart DNS**: Spawns a local HTTP CONNECT proxy with dynamic port allocation, customizable DNS resolvers (`DoH` / `DoT` / `UDP`), embedded catalog in `dns/providers.json`, and built-in TLS anti-spoofing verification.
- **System Tray & Autostart**: Runs silently in the system tray with optional launch-on-login support.
- **Desktop Localization**: Downloads, updates, and rolls back the Russian language pack (SPIDIKSY/antigravity-ru) for Antigravity Desktop.
- **Real-Time Logging**: Live streaming logs and diagnostic `.log` export.

## Quick Start

Download the executable for your OS from the [Releases](https://github.com/CogniDevs/agy-patcher/releases) page:

- **Windows**: Run `agpatcher-windows-amd64.exe`.
- **Linux / macOS**:
  ```bash
  chmod +x agpatcher-linux-amd64
  ./agpatcher-linux-amd64
  ```

## TUI Keybindings

| Key | Action |
|---|---|
| `↑` / `↓`, `Enter` / `→`, `←` / `Esc` | Navigate menus and tabs |
| `Tab` / `S` | Switch panels and focus targets (`S`) |
| `Enter` / `A` | Toggle `[✓]` / run item / select all (`A`) |
| `Space` | Configure target path or DNS server parameters |
| `Delete` | Remove custom path or DNS provider |
| `D` / `+` / `[` `]` / `M` / `V` / `U` | DNS section: add (`+`), reorder (`[`/`]`), mode (`M`), verify (`V`), update catalog (`U`) |
| `1` / `3` | Apply / rollback patches for selected targets |
| `2` / `P` | Start / stop local proxy |
| `4` | Clean all changes, configurations, and autostart |
| `5` | Add path manually |
| `6` / `7` | Install / remove Desktop localization pack |
| `T` / `B` | Minimize to tray / toggle autostart |
| `R` / `L` | Rescan installations / toggle UI language (`RU` / `EN`) |
| `W` / `C` | Save diagnostic `.log` / clear logs |
| `Q` / `Ctrl+C` | Quit and shutdown proxy and tray |

## CLI Commands

```text
agpatcher [--lang en|ru] [--path <path>] [--force] [command]
```

### Commands

- `patch` — Apply patches to discovered targets
- `unpatch` — Rollback patches and restore original files
- `proxy` — Run proxy in foreground mode
- `start` / `stop` — Start or stop background daemon
- `status` — Check status of installations and proxy daemon
- `dns-check` — Probe and verify configured DNS resolvers
- `rupack` — Install Russian localization pack for Desktop

</details>

<details>
<summary><b>Русский</b></summary>

Утилита для настройки работы Antigravity (Desktop, CLI `agy`, расширения для редакторов на базе VS Code и плагина JetBrains) через локальный обходной прокси на Windows, Linux и macOS.

## Функционал

- **Автопоиск установок**: находит клиенты и плагины Antigravity в системе; дополнительные пути добавляются через панель или флаг `--path`.
- **Патч и откат**: включает сетевую маршрутизацию для выбранных установок и поддерживает откат изменений из резервных копий.
- **Локальный прокси и DNS**: поднимает HTTP CONNECT-прокси с автоматическим подбором свободного порта, настраиваемыми серверами DNS (`DoH` / `DoT` / `UDP`), каталогом в [`dns/providers.json`](dns/providers.json) (поддержка отдельного `user_dns_providers`, временного скрытия нерабочих каталожных DNS до следующей синхронизации `U` и подключения внешнего фида через `dns_catalog_url` / `feed_url`) и встроенной проверкой подмены/TLS (`V` в панели или `dns-check`).
- **Трей и автозапуск**: обеспечивает фоновую работу прокси через системный трей и опциональный запуск при входе в систему.
- **Русификатор Desktop**: загрузка, обновление и откат русского языкового пакета ([SPIDIKSY/antigravity-ru](https://github.com/SPIDIKSY/antigravity-ru)) для Antigravity Desktop.
- **Логи и диагностика**: вывод событий в реальном времени и сохранение диагностического `.log`-отчёта в файл.

## Быстрый старт

Скачайте сборку для вашей ОС из раздела [Releases](https://github.com/CogniDevs/agy-patcher/releases):

- **Windows**: запустите `agpatcher-windows-amd64.exe`.
- **Linux / macOS**:
  ```bash
  chmod +x agpatcher-linux-amd64
  ./agpatcher-linux-amd64
  ```

## Управление в панели

| Клавиша | Действие |
|---|---|
| `↑` / `↓`, `Enter` / `→`, `←` / `Esc` | Навигация по меню и разделам |
| `Tab` / `S` | Переключение панелей и быстрый переход к целям (`S`) |
| `Enter` / `A` | Переключить `[✓]` / запустить пункт / отметить или снять все (`A`) |
| `Space` | Настроить элемент: изменить путь цели или параметры DNS-сервера |
| `Delete` | Удалить ручной путь / сбросить заменённый путь / удалить DNS |
| `D` / `+` / `[` `]` / `M` / `V` / `U` | Раздел DNS: добавить (`+`), порядок (`[`/`]`), режим (`M`), проверить (`V`), каталог (`U`) |
| `1` / `3` | Применить / откатить патч для выбранных целей |
| `2` / `P` | Запустить / остановить локальный прокси |
| `4` | Полная очистка изменений, настроек и автозапуска |
| `5` | Добавить путь вручную |
| `6` / `7` | Установить / удалить русификатор Desktop |
| `T` / `B` | Свернуть в трей / переключить автозапуск |
| `R` / `L` | Пересканировать установки / сменить язык (`RU` / `EN`) |
| `W` / `C` | Сохранить логи и диагностику в `.log` / очистить логи |
| `Q` / `Ctrl+C` | Выйти и остановить прокси и трей |

## Команды CLI

```text
agpatcher [--lang en|ru] [--path <путь>] [--force] [команда]
```

| Команда | Описание |
|---|---|
| `tui` | Открыть интерактивную панель |
| `open-tui` | Открыть панель для активного фонового сервиса |
| `tray` | Запустить в трее (`--no-proxy` отключает автостарт прокси) |
| `autostart` | Управление автозапуском (`on` / `off` / `status`) |
| `patch` / `restore` | Применить / откатить патч |
| `install` | *(Linux)* Установить Antigravity из архива `.tar.gz` (`--system` для `/opt`, `--no-sandbox`) |
| `ru-install` / `ru-restore` | Установить / удалить русификатор Desktop |
| `proxy` | Запустить локальный прокси в консоли |
| `dns-check` | Проверить DNS-серверы на реальную подмену и TLS (`--force` — подтянуть из GitHub) |
| `status` | Показать статус установок и прокси |
| `clean` | Полностью очистить патчи, настройки и автозапуск |
| `version` / `help` | Версия / справка |

Флаги: `--lang en|ru` (`-l`), `--path <путь>` (`-p`), `--force` (`-f`), `--tray`, `--no-proxy`.

## Примечания по ОС

- **Linux**: на Linux в разделе «Патчинг и цели» (`[I]`) или командой `agpatcher install` можно установить Antigravity из `.tar.gz` (в `~/.local/share/Antigravity` без `sudo` или в `/opt/Antigravity` с `sudo`), сразу создать `.desktop`-ярлык и пропатчить; для уже установленной системной сборки в `/opt` или `/usr/share` запускайте `patch`/`restore` с `sudo`, а прокси (`tray`/`proxy`) — от обычного пользователя.
- **macOS**: при ограничении записи в `.app` разрешите Терминалу доступ в *Системные настройки → Конфиденциальность и безопасность → Управление приложениями*.

## Сборка из исходников

```bash
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o agpatcher ./cmd/agpatcher
go test -count=1 ./...
```

## Лицензия

[MIT](LICENSE)
</details>
