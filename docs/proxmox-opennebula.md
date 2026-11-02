# Proxmox и OpenNebula: HTTPS-бэкап и восстановление

Copyright 2026 Кислов Роман Сергеевич. Apache-2.0.

Реализованы полная копия дисков, блочная дедупликация в репозитории медиасервера, восстановление в новую ВМ, прогресс, события и журнал временных ресурсов. Управление доступно в общей форме «Бэкап ВМ», API и CLI `srk proxy virtualization`. Проверки выполнены на HTTPS-стендах API; приёмка на настоящих гипервизорах ещё необходима. Снимок работающей ВМ имеет уровень crash-consistent; согласованность СУБД внутри гостя не гарантируется. Исходная ВМ не выключается.

## Proxmox без PBS

Основной путь: PVE 8+ `vzdump` → davfs2 HTTPS-монтирование → временная область WebDAV медиасервера → распаковка и проверка VMA на прокси → дедупликация СРК. При восстановлении медиасервер предоставляет виртуальный VMA-файл из проверенных блоков репозитория. PVE читает его через тот же HTTPS-монт и штатный `qmrestore`, создаёт новую остановленную ВМ; прокси меняет имя, UUID и сети до запуска. Старые MAC не назначаются.

PBS для этого пути не нужен. Внутри гостевых ВМ агенты не нужны. Однократную настройку davfs2 на узлах PVE выполняет администратор. СРК не использует SSH и не изменяет конфигурацию узлов автоматически.

### Медиасервер

В конфигурацию media добавьте блок (пути абсолютные):

```json
"webdav": {
  "listen": "0.0.0.0:8447",
  "url": "https://media.example.org:8447",
  "root": "/var/lib/srk/webdav",
  "credentials_file": "/etc/srk/webdav-users.json"
}
```

Каталог root — отдельный от repository, режим 0700. Файл пользователей — 0600, JSON вида `{"pve-cluster":"REPLACE_WITH_RANDOM_PASSWORD_32_CHARS"}`. Используйте случайный пароль, минимум 16 символов. Публичный WebDAV-порт использует серверный сертификат media, TLS 1.3 и Basic-аутентификацию внутри TLS. Сертификат должен содержать DNS/SAN имени media. Внутренние API сессий остаются под mTLS и разрешениями конкретного задания; WebDAV-пароль не даёт доступа к управлению репозиторием.

### Узлы PVE

Установите davfs2 и доверенный CA медиасервера. Настройте системный HTTPS-монт в `/mnt/srk`, секрет в `/etc/davfs2/secrets` (0600), проверку CA/имени сервера; не используйте принятие недоверенного сертификата. Настройте `delay_upload 0` для этого монтирования: закрытый архив должен сразу отправляться на сервер. Пример опций fstab: `rw,_netdev,noexec,nosuid,nodev`; проверьте восстановление подключения и поведение кеша davfs2 на используемом дистрибутиве.

Добавьте Directory storage PVE `srk-dav`: `path /mnt/srk`, `content backup`, `is_mountpoint 1`. Эта защита обязательна: при отсутствии HTTPS-монта PVE не должен писать копии на локальный корневой диск. Такой же монт и storage должны существовать на целевом узле восстановления.

Учётная запись API PVE должна иметь права чтения конфигурации/состояния, vzdump, создания и изменения новой ВМ, управления её дисками, сетями и задачами. Нативные операции с абсолютными dumpdir/archive требуют соответствующих привилегий PVE (в частности Sys.Modify); конкретную роль проверяйте на установленной версии PVE. Токен хранится в приватном файле `{"token_id":"srk@pve!backup","secret":"REPLACE"}`. Не включайте токены в задания, URL или журналы.

Профиль в `virtualization` конфигурации proxy:

```json
"pve": {
  "kind": "proxmox",
  "endpoint": "https://pve.example.org:8006/api2/json",
  "ca_file": "/etc/srk/pve-ca.pem",
  "credential_file": "/etc/srk/pve-credentials.json",
  "backup_enabled": true,
  "backup_storage": "srk-dav",
  "dav_mount_path": "/mnt/srk",
  "dav_user": "pve-cluster",
  "state_dir": "/var/lib/srk/pve-state",
  "staging_dir": "/var/lib/srk/pve-staging",
  "staging_max_bytes": 1099511627776,
  "timeout_seconds": 21600
}
```

state/staging — существующие каталоги 0700. Лимит staging применяется и к сжатому архиву на media, и к распакованному VMA на proxy. Заранее предусмотрите место на обоих серверах, включая дедуп-репозиторий и кеш davfs2 на PVE. Установленный reserve_bytes медиасервера сохраняется при записи WebDAV.

Источник задания: `proxmox://pve/100`. Восстановление: `provider=proxmox`, `flavor_id` — имя узла, `storage_domain_id` — хранилище дисков с content=images, `networks` — имена активных мостов. Создаётся свободный VMID; исходный и существующий VMID не используются.

При бэкапе проверяются контрольные суммы заголовков VMA, полнота диапазонов всех дисков, отсутствие дубликатов/обрезанных экстентов. СРК дополнительно хранит SHA-256 всего архива и блоков. Сохраняются конфигурация, разделы и загрузчик внутри дисков, EFI/TPM-диски. Не поддерживаются passthrough, hookscript, произвольные QEMU args, virtiofs, host block devices, исключённые из backup диски и cloud-init media. ВМ с такими ресурсами отклоняется до копирования. CD-ROM не считается диском данных.

WebDAV-сессии изолированы по пользователю и заданию, ограничены сроком и квотой. После seal загрузка становится неизменяемой; restore-сессия только для чтения. Временные файлы удаляются при завершении/откате; состояние сессий сохраняется для восстановления после перезапуска. При неоднозначном исходе нативной операции автоматическое удаление прекращается, журнал показывает необходимую проверку.

В коде сохранён отдельный адаптер PBS/PVE 9+ для декодированного экспорта и API import; он не используется этим профилем и не прошёл текущую проверку полного цикла. Для данного развёртывания используйте WebDAV.

## OpenNebula 7.4

Путь: `one.vm.backup` с interactive datastore → OneBEX за проверяемым HTTPS-прокси → чтение логических raw-диапазонов дисков → репозиторий СРК. Восстановление: временный HTTPS-export на proxy → штатный импорт образов OpenNebula → новая ВМ в HOLD → проверка и release.

На стороне OpenNebula заранее настройте backup datastore с `DS_MAD=interactive` и BACKUP_CONFIG исходной ВМ: `MODE=FULL`, `INTERACTIVE=YES`, `KEEP_LAST=0`, `FS_FREEZE=NONE` или `AGENT`, все DISK_IDS. СРК не переписывает эту конфигурацию и не выключает источник. На каждом соответствующем hypervisor/backup bridge настройте HTTPS reverse proxy перед локальным OneBEX, предпочтительно с mTLS; внешний plaintext-порт должен быть закрыт. Разрешите GET с JSON body для `/status`, HTTP Range и POST finalize/finish/cancel.

Профиль proxy содержит `kind=opennebula`, HTTPS XML-RPC `endpoint`, `ca_file`, приватный `credential_file` вида `{"session":"srk:REPLACE"}`, `backup_enabled=true`, `backup_datastore` (числовой ID), `onebex_endpoints` (hostname → HTTPS URL), приватные state/staging, лимит и timeout, как в примере PVE. Дополнительно:

```json
"onebex_endpoints": {"hypervisor-01": "https://hypervisor-01.example.org:8448"},
"backup_datastore": "100",
"restore_export_url": "https://proxy.example.org:8449",
"restore_export_listen": "0.0.0.0:8449",
"restore_cert_file": "/etc/srk/proxy.crt",
"restore_key_file": "/etc/srk/proxy.key"
```

Для mTLS OneBEX задайте `client_cert_file` и `client_key_file`. Сертификаты и ключи имеют абсолютные пути, ключи — 0600. CA должен доверять XML-RPC и всем OneBEX endpoints. HTTPS-export использует случайные ограниченные URL каждого диска; слушатель закрывается после операции. Откройте порт только для image datastore bridge, установите там CA прокси.

**Проверка TLS при импорте:** штатный OpenNebula downloader может содержать curl `-k`. Администратор должен удалить `-k/--insecure` из GLOBAL_CURL_ARGS для используемого downloader, оставить `--fail`, запретить downgrade редиректов (`--proto =https --proto-redir =https`) и установить доверенный CA. Убедитесь отрицательным тестом, что неправильный сертификат и перенаправление в HTTP отвергаются. После этой настройки пометьте целевой image datastore `SRK_HTTPS_VERIFY=YES`; без этой явной отметки адаптер откажет в восстановлении. Одна отметка без настройки downloader не обеспечивает проверку TLS.

Источник: `opennebula://one/100`. Восстановление: `provider=opennebula`, `flavor_id` — числовой ID кластера, `storage_domain_id` — ID image datastore, `networks` — числовые ID сетей. СРК проверяет потребление HTTPS-потока, ID образов, CPU/RAM, сети и состояние новой ВМ. Готовность ОС и приложений требует отдельной проверки. PCI/shared/RBD-диски отклоняются.

## Проверки и CLI

`srk --config /etc/srk/admin.json proxy virtualization profiles|list PROFILE|plan PROFILE VM_ID`, `journal PROFILE`, `cleanup PROFILE RUN_ID`. Создание и запуск задания — общие `manager jobs create` / `manager jobs run`; точный синтаксис смотрите `srk help`. Все компоненты сохраняют свои CLI.

Стендовые тесты: Proxmox DAV backup → dedup → новая ВМ, повреждённый архив, откат после частичного restore; OpenNebula HTTPS backup → dedup → новая ВМ с побайтовым сравнением диска, откат неудачного запуска; ограничения WebDAV (пользователь, traversal, quota, seal, TLS, durable state); VMA truncation/coverage/checksum/duplicate и переписывание ownership. Эти тесты не заменяют приёмку PVE/davfs2/OneBEX на реальных версиях.

Источники: [PVE API](https://pve.proxmox.com/pve-docs/api-viewer/), [исходный API QEMU](https://git.proxmox.com/?p=qemu-server.git;a=blob;f=src/PVE/API2/Qemu.pm), [формат VMA](https://lists.proxmox.com/pipermail/pbs-devel/2024-March/008179.html), [OpenNebula Interactive Backup](https://docs.opennebula.io/7.4/product/integration_references/infrastructure_drivers_development/interactive_backup/), [OpenNebula downloader](https://github.com/OpenNebula/one/blob/master/src/datastore_mad/remotes/downloader.sh).
