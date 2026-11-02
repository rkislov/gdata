# Источники и восстановление в 0.7

Автор: Кислов Роман Сергеевич. Apache-2.0. Разработка и проверки локальные; публикации на GitHub нет.

## Управление виртуальными машинами

Кнопка «Бэкап ВМ» показывает онлайн-прокси, его профили и инвентаризацию ВМ. Перед созданием задания выполняется проверка конфигурации источника. Полная копия передаётся в репозиторий СРК с дедупликацией SHA-256 по 4 МиБ. Очередь и общие лимиты параллельности действуют и для ВМ. Независимые ВМ могут копироваться одновременно; повторный запуск той же ВМ ограничен. Незавершённая очистка блокирует следующие операции профиля, живые операции других ВМ не считаются брошенными.

«Подробнее» показывает этап, процент передачи, объём новых и повторно использованных данных, диски и журнал. Из успешной копии доступны проверка целостности и восстановление в **новую** ВМ. В восстановлении явно задаются целевая платформа/прокси, имя и сети. Старые IP/MAC не назначаются. API подтверждает состояние ВМ, диски и сети; загрузка гостевой ОС и готовность приложений на стенде API не проверяются.

Политика `crash-consistent` не гарантирует транзакционную согласованность приложений или единую точку во времени между независимо снятыми дисками. Для `cold-only` исходная ВМ уже должна быть выключена; СРК не выключает её. Изменение исходной конфигурации во время копирования приводит к отказу. Не меняйте конфигурацию и состояние ВМ другим инструментом во время операции.

Профили и секреты задаются на прокси; веб-интерфейс не принимает API-токены. Управление СРК использует TLS 1.3/mTLS. Соединения с платформами — HTTPS с проверкой сертификатов. Перенаправления запрещены. Файлы секретов — 0600/0400, каталоги журнала и staging — 0700.

### OpenStack / Tionix

Базовый API Nova 2.1 совместим с Queens. Нужны явно настроенные Nova, Glance v2, Cinder v3 и Neutron v2.0. Учётная запись должна иметь права чтения исходной ВМ/томов, создания снимков, временных томов и образов, экспорта/загрузки данных и создания/удаления собственных ресурсов. Точные политики зависят от инсталляции; необходимые права проверяются операциями API.

```json
{
  "virtualization": {
    "tionix": {
      "kind": "openstack",
      "backup_enabled": true,
      "endpoint": "https://nova.example/v2.1/PROJECT_UUID",
      "glance_endpoint": "https://glance.example/v2",
      "cinder_endpoint": "https://cinder.example/v3/PROJECT_UUID",
      "neutron_endpoint": "https://neutron.example/v2.0",
      "project_id": "PROJECT_UUID",
      "credential_file": "/etc/srk/openstack-credentials.json",
      "ca_file": "/etc/srk/cloud-ca.pem",
      "state_dir": "/var/lib/srk/openstack-journal",
      "timeout_seconds": 7200
    }
  }
}
```

Файл credentials: `{"token":"..."}` либо `{"application_credential_id":"...","application_credential_secret":"..."}` с дополнительным `auth_url`, например `https://identity.example/v3`. Во втором случае Keystone-токен обновляется перед истечением срока, а project ID проверяется.

Для image-backed ВМ копируются корневой образ и все подключённые Cinder-тома. Для volume-backed ВМ нулевой Glance-образ Nova является метаданными: СРК отдельно экспортирует **каждый** Cinder-снимок через временный том и raw-образ. Проверяется полнота набора дисков. Ephemeral/swap, multiattach и зашифрованные тома отклоняются. Поддерживаются raw и самостоятельный qcow2 без backing/external-data; архив до 4 ТиБ. User-data/cloud-init, floating IP, старые порты/MAC и внешние политики не воссоздаются автоматически.

Данные передаются потоками без локальной полной копии диска. Перед восстановлением проверяется репозиторий, при чтении — каждый блок и полный SHA-256 диска. Восстановление создаёт приватные Glance-образы, необходимые Cinder-тома и новую Nova-ВМ. Сохранившиеся образы и тома являются её зависимостями и автоматически не удаляются после успеха.

### oVirt

Используется полный Backup API (целевой профиль — oVirt 4.4+ с поддержкой backend backup) и imageio с `format=raw`. Копируются все активные image-диски. Общие/shareable, LUN, locked-диски и host devices отклоняются. Сохраняются CPU, память, тип ОС, BIOS, интерфейс/порядок загрузочных дисков и ссылки на vNIC-профили. Не копируются память ВМ, ISO/CD-ROM и passthrough-устройства; восстановление запускает ОС с диска. Совместимость с реальным гипервизором требует отдельной приёмки.

```json
{
  "virtualization": {
    "ovirt": {
      "kind": "ovirt",
      "backup_enabled": true,
      "endpoint": "https://engine.example/ovirt-engine/api",
      "credential_file": "/etc/srk/ovirt-token.json",
      "ca_file": "/etc/srk/ovirt-ca.pem",
      "state_dir": "/var/lib/srk/ovirt-journal",
      "staging_dir": "/var/lib/srk/ovirt-staging",
      "staging_max_bytes": 1099511627776,
      "transfer_origins": ["https://imageio.example:54322"],
      "timeout_seconds": 7200
    }
  }
}
```

Файл токена: `{"token":"..."}`. Каталоги создайте заранее с режимом 0700. Endpoint imageio должен иметь доверенный сертификат и входить в `transfer_origins`; origin Engine разрешён для его imageio proxy. Engine bearer-токен в imageio не передаётся. Ticket URL не сохраняется в журнале/отчётах.

Полная raw-копия сначала размещается в закрытом staging-каталоге (нужно место на все диски в пределах `staging_max_bytes`), затем попадает на медиасервер с дедупликацией. После операции локальные временные файлы удаляются. При восстановлении укажите UUID кластера (`flavor_id`), `storage_domain_id` и UUID vNIC-профилей в `networks`. Диски загружаются в imageio и **перечитываются** для проверки SHA-256 до подключения к новой ВМ. Новые диски сохраняются как зависимости восстановленной ВМ.

### Журнал и очистка

До создания ресурса сохраняется intent, затем ID. Имена/метаданные ресурсов связаны с run ID и proxy ID. Ошибка/отмена запускает очистку в отдельном контексте. Чужие ресурсы и исходная ВМ не удаляются. Неоднозначный результат создания не считается отсутствием ресурса: журнал остаётся pending до сверки. При перезапуске прокси пробует восстановить очистку; недоступность платформы оставляет диагностический API доступным.

В форме бэкапа есть «Журнал ресурсов платформы» и повторная безопасная очистка. Для активного задания повторная очистка получает конфликт блокировки. CLI:

```sh
srk --config /etc/srk/admin.json --endpoint https://proxy.example:8446 proxy virtualization journal PROFILE
srk --config /etc/srk/admin.json --endpoint https://proxy.example:8446 proxy virtualization cleanup PROFILE RUN_ID
srk --config /etc/srk/admin.json manager runs restore RUN_ID --vm-request /path/new-vm.json
```

`new-vm.json` содержит непосредственно `VMRestoreRequest`: `name`, `profile`, `proxy_id` (необязательно), `flavor_id`, `networks`; для oVirt — также `storage_domain_id`. Файл не должен содержать credentials.

### Proxmox и OpenNebula

Добавлены полная копия в репозиторий СРК и восстановление в новую ВМ по HTTPS: Proxmox без PBS через WebDAV СРК, OpenNebula 7.4 через интерактивный OneBEX за HTTPS-прокси и импорт образов. Требуется однократная настройка инфраструктуры, включая TLS и storage. [Полная инструкция и результаты стендовых проверок](proxmox-opennebula.md).

## S3-источник: MinIO / Ceph RGW

Профиль на **агенте**:

```json
{
  "s3": {
    "source": {
      "endpoint": "https://s3.example",
      "region": "us-east-1",
      "bucket": "production",
      "prefix": "",
      "credential_file": "/etc/srk/s3-source.json",
      "ca_file": "/etc/srk/s3-ca.pem",
      "staging_dir": "/var/lib/srk/s3-staging",
      "max_bytes": 107374182400,
      "allow_restore": false
    }
  }
}
```

Credentials: `{"access_key":"...","secret_key":"...","session_token":"..."}`; session token необязателен. Используются HTTPS, SigV4, path-style addressing; права на ListBucket, GetObject/HeadObject, GetObjectTagging и чтение конкретной версии, если включено versioning. Источник задания `s3://source`, режим full. Локальный staging должен вместить выбранные объекты; `max_bytes` ограничивает суммарный объём данных. Сейчас предел — 10 000 текущих объектов и 5 ГиБ на объект; большие объекты отклоняются до фиксации копии. Для них нужен следующий этап — multipart restore.

Ключи объектов сохраняются в индексе, файлы staging имеют внутренние имена и не строятся из S3-ключей. Сохраняются текущие данные, ETag/version ID, content type/encoding, cache control, пользовательские метаданные и теги. Вторая инвентаризация выявляет изменение состава/ETag/размеров; GET закреплён за версией/If-Match. Это **не атомарный снимок бакета**: для единого состояния исключите изменения на время прохода. История версий, delete markers, ACL, lifecycle, IAM, Object Lock и настройки бакета не переносятся; время Last-Modified на целевом S3 задаёт сервер.

Восстановление: `s3://TARGET_PROFILE/new-prefix/`, где целевой профиль имеет `allow_restore: true` и нацелен на нужный бакет. Новый непустой префикс обязателен. Существование всех целевых ключей проверяется до записи, PUT использует `If-None-Match: *`. Если backend не поддерживает условную запись, восстановление не должно считаться совместимым без его приёмки. Объекты перечитываются после записи и сверяются по SHA-256. При ошибке уже созданные объекты сохраняются для диагностики, исходный бакет не очищается. Автоматическое продолжение прерванного восстановления в тот же префикс пока не реализовано.

Проверено на настоящем MinIO по HTTPS; Ceph RGW использует тот же API, но на реальном RGW этот релиз ещё не проверялся.

## MySQL / MariaDB

На агенте нужны родные утилиты соответствующего семейства: `mysql`+`mysqldump` либо `mariadb`+`mariadb-dump`.

```json
{
  "mysql": {
    "production": {
      "engine": "mariadb",
      "database": "production",
      "defaults_file": "/etc/srk/mariadb-client.cnf",
      "bin_dir": "/usr/bin",
      "staging_dir": "/var/lib/srk/mysql-staging",
      "max_bytes": 107374182400,
      "sslmode": "verify-full",
      "ca_file": "/etc/srk/mysql-ca.pem",
      "timeout_seconds": 7200,
      "allow_restore": false
    }
  }
}
```

`defaults_file` — закрытый файл 0600/0400 с секцией `[client]` и параметрами host, port, user, password. Пароль не передаётся в argv или веб-форму. Для продакшена утилитам принудительно задаётся проверка сертификата и имени сервера. `sslmode=disable` допустим только в явном development-режиме с локальным host/socket. Рекомендуются отдельные учётные записи для чтения источника и восстановления в изолированную целевую СУБД с минимальными правами.

Источник задания для обоих семейств — `mysql://production`. Копируется одна база: таблицы/данные InnoDB, представления, процедуры, события, триггеры. Используются single-transaction, quick, skip-lock-tables, hex-blob; у MySQL отключается перенос GTID. Предварительная проверка отклоняет нетранзакционные base tables. На время дампа необходимо исключить DDL; SQL users/grants, глобальные настройки, binlog и PITR не копируются. Общая согласованность нескольких баз не заявляется.

Восстановление: `mysql://TARGET_PROFILE/new_database`, целевой профиль с `allow_restore: true` и тем же engine. Существующая база не заменяется: CREATE DATABASE выполняется без IF NOT EXISTS. До импорта SQL-идентификаторы, квалифицированные исходной схемой, перенаправляются на новое имя; строковые значения и комментарии сохраняются, SQL в версионных комментариях обрабатывается. Динамический SQL в строках и внешние зависимости на другие базы не переписываются — такие приложения требуют отдельной проверки и изолированной цели. После ошибки частично созданная новая база сохраняется для диагностики. Не включайте event scheduler на целевом сервере до проверки восстановленных событий.

Нативный сценарий проверен на MySQL 8.4 и MariaDB 11.8: бинарные данные, триггер с исходным именем схемы, дедупликация, запрет восстановления поверх существующей базы и отказ для MyISAM. Staging закрыт правами 0700 и очищается после операции; данные в нём не шифруются отдельно.

## Основания интеграции

- [Nova API](https://docs.openstack.org/api-ref/compute/), [Cinder API](https://docs.openstack.org/api-ref/block-storage/v3/), [Glance API](https://docs.openstack.org/api-ref/image/v2/).
- [oVirt Backup SDK example](https://github.com/oVirt/python-ovirt-engine-sdk4/blob/main/examples/backup_vm.py), [imageio Images API](https://ovirt.github.io/ovirt-imageio/images.html).
- [Proxmox API](https://pve.proxmox.com/pve-docs/api-viewer/), [OpenNebula XML-RPC](https://docs.opennebula.io/7.2/product/integration_references/system_interfaces/api/), [OneBEX](https://docs.opennebula.io/7.4/product/integration_references/infrastructure_drivers_development/interactive_backup/).
- [S3 PutObject / conditional writes](https://docs.aws.amazon.com/AmazonS3/latest/API/API_PutObject.html), [MinIO Go SDK](https://github.com/minio/minio-go).
- [mysqldump](https://dev.mysql.com/doc/refman/8.4/en/mysqldump.html), [mariadb-dump](https://mariadb.com/docs/server/clients-and-utilities/backup-restore-and-import-clients/mariadb-dump).
