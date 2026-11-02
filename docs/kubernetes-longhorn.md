# Резервное копирование Kubernetes и Longhorn

СРК выполняет копирование через HTTPS Kubernetes API из компонента `agent`; установка агентов внутрь Pod не требуется. Профили и токены хранятся локально на агенте. Между агентом и медиасервером используются существующие mTLS и разрешения на конкретное задание.

## Что сохраняется

Тип задания `kubernetes` сохраняет выбранные namespaces, ConfigMap, Service, ServiceAccount, PVC, Deployment, StatefulSet, DaemonSet, CronJob, Ingress, NetworkPolicy, Role и RoleBinding. JSON-объекты сохраняются в репозитории СРК с блочной дедупликацией и проверкой целостности. Secrets включаются только настройкой `include_secrets`; токены ServiceAccount исключаются и в этом режиме.

При указанном `longhorn_snapshot_class` для каждого PVC создаётся CSI VolumeSnapshot, связанный с резервной копией Longhorn. Тип задания `longhorn` требует такой класс и дополнительно сохраняет те же объекты Kubernetes. Класс должен использовать `driver.longhorn.io`, параметр `type: bak` и `deletionPolicy: Retain`. СРК проверяет Bound PVC, UID привязки к PV, CSI-драйвер, владение созданным снимком, готовность снимка и содержимого, ссылку на исходный том и handle `bak://...`.

**Граница хранения:** JSON-объекты и индекс томов находятся в репозитории СРК. Блоки PVC находятся в настроенном внешнем backupstore Longhorn, а не внутри копии СРК. Репликация или запись JSON-архива на ленту не переносит блоки Longhorn. Удаление backupstore делает восстановление PVC невозможным. СРК не удаляет эти резервные копии автоматически; срок хранения в СРК относится к его JSON-архиву. Администратор должен сохранять backupstore как минимум до окончания срока всех зависимых копий.

Без `longhorn_snapshot_class` сохраняются только API-объекты, **без данных PVC**. Это явно отражается в индексе и интерфейсе. При включённом Longhorn несовместимый или непривязанный PVC завершает задание ошибкой, а не пропускается.

Копия не является снимком etcd или всей управляющей плоскости. CRD, произвольные custom resources, ClusterRole/ClusterRoleBinding, Nodes, Pod, выполненные Job, локальные/эфемерные данные контейнеров и образы контейнеров не входят в этот набор. Чтение страниц одного API-списка использует `continue`; общей транзакции между разными ресурсами и томами нет. Приложения не останавливаются, hooks замораживания СУБД и согласованный групповой снимок не реализованы. Для СУБД используйте также нативные задания СРК.

## Подготовка

Нужны HTTPS API, доверенный CA, ServiceAccount с разрешениями, CSI snapshot controller/CRD и настроенный Longhorn backup target. Пример RBAC и класса: `packaging/kubernetes/srk-backup-rbac.yaml`. Замените namespace `demo`; для каждого выбранного namespace создайте Role/RoleBinding и разрешите чтение его Namespace. Secrets разрешаются отдельным правилом при необходимости. Пример не устанавливает сам Longhorn или snapshot controller.

Токен передаётся через приватный файл с правами 0600. Короткоживущие токены обновляются внешним механизмом до истечения: агент перечитывает файл перед каждым API-запросом. Отказ проверки TLS, авторизации или требуемого API завершает задание ошибкой. Перенаправления HTTP запрещены.

Фрагмент конфигурации агента:

```json
{
  "kubernetes": {
    "production": {
      "endpoint": "https://kubernetes.example.org:6443",
      "ca_file": "/etc/srk/kubernetes/ca.crt",
      "token_file": "/etc/srk/kubernetes/token",
      "staging_dir": "/var/lib/srk/kubernetes-staging",
      "namespaces": ["demo"],
      "include_secrets": false,
      "longhorn_snapshot_class": "lh-backup",
      "max_bytes": 268435456,
      "timeout_seconds": 3600
    }
  }
}
```

Каталог staging должен существовать, быть приватным (0700) и иметь достаточно места. `max_bytes` ограничивает общий JSON-архив, а не объём PVC в Longhorn; максимальное значение — 1 ГиБ. Дополнительно ограничены число объектов (10 000) и размер одного API-ответа (16 МиБ).

В интерфейсе создайте задание типа Kubernetes или Longhorn, выберите агент и медиасервер. Источник: `kubernetes://production` или `longhorn://production`; режим — `full`. Расписание, история и дедупликация используют общий механизм заданий. Профили отображаются в статусе агента, секреты в каталог менеджера не передаются. Через CLI используется обычное создание задания из JSON, например:

```json
{
  "name": "Kubernetes demo + Longhorn",
  "type": "longhorn",
  "agent_id": "agent-kubernetes",
  "media_id": "media-main",
  "source": "longhorn://production",
  "mode": "full",
  "retention_days": 30,
  "interval_seconds": 86400,
  "enabled": true
}
```

```sh
srk --config /etc/srk/admin.json manager jobs create --file /etc/srk/kubernetes-job.json
```

## Выгрузка и восстановление

В форме восстановления выберите целевой профиль, исходный namespace и имя нового namespace. СРК получает JSON-архив с медиасервера, проверяет SHA-256 блоков, затем создаёт namespace, CSI VolumeSnapshot/VolumeSnapshotContent со ссылками на сохранённые backup handles, новые PVC и ресурсы приложения. Проверяется готовность снимков, Bound PVC и UID привязки к новому PV Longhorn. Создание пустых PVC вместо отсутствующих резервных копий запрещено.

В целевом профиле дополнительно укажите:

```json
{
  "allow_restore": true,
  "restore_namespace_prefix": "restore-",
  "restore_storage_class": "longhorn",
  "restore_cluster_roles": [],
  "allow_ingress_restore": false
}
```

Список `namespaces` целевого профиля должен включать исходный namespace. Целевой StorageClass должен использовать Longhorn и Immediate binding. SnapshotClass должен иметь `type: bak` и `Retain`. Целевой Longhorn должен видеть исходный backupstore. Пример дополнительных прав: `packaging/kubernetes/srk-restore-rbac.yaml`. Правила Kubernetes RBAC не ограничивают создание namespace по префиксу: это проверяет агент СРК; используйте выделенную учётную запись и при необходимости admission policy кластера.

CLI использует обычную команду восстановления:

```sh
srk --config /etc/srk/admin.json manager runs restore RUN_ID --destination 'kubernetes://production/demo/restore-demo?start=false'
```

По умолчанию приложения подготовлены без запуска: Deployment/StatefulSet имеют ноль реплик, CronJob приостановлены, DaemonSet получает невыполнимую node affinity. Для последующего ручного запуска восстановите нужное число реплик, suspend и исходную affinity из JSON-экспорта. При `start=true` сохраняются исходные параметры запуска и ожидается готовность Deployment, StatefulSet и DaemonSet по API. CronJob проверяется по созданию; выполнение будущих заданий не ожидается.

Служебные UID, ownerReferences, status и старые привязки очищаются. Service NodePort/LoadBalancer переводятся в ClusterIP; старые IP и nodePort не переносятся. RoleBinding ServiceAccount перепривязывается к новому namespace. Ссылки на ClusterRole допускаются только из `restore_cluster_roles`; Ingress требует отдельного разрешения. Произвольные строки в ConfigMap/Secret, URL и ссылки на исходный namespace автоматически не переписываются. Отсутствующие Secrets и внешние зависимости нужно обеспечить перед запуском приложения. Копия API-объектов без данных PVC не позволяет восстановить приложение с PVC.

Существующие namespaces не используются. Перед записью и во время ожидания проверяется UID созданного namespace. При сбое частичные ресурсы остаются для диагностики; автоматического удаления нет. Журнал хранится на агенте в `staging_dir/restore-journals`: JSON-состояние и полный поток событий JSONL. Повтор завершённого запуска возвращает сохранённый результат, незавершённый запуск не повторяет изменения автоматически. После устранения причины выполните новый запуск с другим namespace.

Веб-интерфейс показывает процент обработки объектов, количество готовых томов, текущий ресурс и последние 200 событий, обновляя состояние выполняющейся операции. Процент отражает этапы/объекты, а не переданные байты внутри Longhorn. SHA-256 проверяет архив СРК; готовность восстановленных томов и приложений проверяется по API, без побайтового чтения PVC и проверки транзакций приложения.

Кнопка «Экспорт JSON» по-прежнему выгружает исходные объекты и индекс в новый каталог из `restore_roots` агента. Этот режим не изменяет кластер.

После ошибки или отмены уже созданные CSI-снимки/резервные копии остаются с меткой `srk.gdata/run`. Они не удаляются автоматически. Повтор с тем же Run ID проверяет существующий снимок и не создаёт новый; конфликт владения приводит к отказу. Новый запуск задания получает новый ID. Частичный JSON-архив в репозиторий не публикуется.

## Совместимость и проверка

Реализованы стабильные Kubernetes API `v1`, `apps/v1`, `batch/v1`, `networking.k8s.io/v1`, `rbac.authorization.k8s.io/v1`, `snapshot.storage.k8s.io/v1`. Longhorn проверяется по возможностям CSI-класса, а не по обещанию поддержки любой версии. Реальные версии кластера пока не предоставлены; сценарии проверены на HTTPS-имитаторе API и через медиасервер СРК.

Основание контракта: [Kubernetes Volume Snapshots](https://kubernetes.io/docs/concepts/storage/volume-snapshots/), [пагинация API PersistentVolume](https://kubernetes.io/docs/reference/kubernetes-api/core/persistent-volume-v1/), [Longhorn 1.10: CSI VolumeSnapshot для backup и восстановления](https://longhorn.io/docs/archives/1.10.0/snapshots-and-backups/csi-snapshot-support/csi-volume-snapshot-associated-with-longhorn-backup/).

## Автоматические проверки восстановления

В профиле агента включите `allow_verification: true` вместе с `allow_restore: true`. `restore_namespace_prefix` должен иметь длину до 24 символов: проверка использует namespace вида `<prefix>check-<RunID>`. Целевой StorageClass должен иметь `reclaimPolicy: Delete`; это позволяет CSI удалить только вновь созданные PV после удаления тестовых PVC. SnapshotClass и VolumeSnapshotContent сохраняют `Retain`, поэтому исходные backup handles и данные backupstore не удаляются.

В веб-интерфейсе откройте **«Проверки Kubernetes» → «Добавить проверку»**. Выберите задание, целевой профиль, исходный namespace, интервал и тайм-аут. Дополнительно задайте одну или несколько HTTP-проверок: Service, порт, путь, ожидаемый статус 2xx и необязательный фрагмент ответа. Включённая политика после сохранения запускается при следующем проходе планировщика. Интервал отсчитывается от постановки в очередь; минимальный интервал — 600 секунд. Пропущенные интервалы не создают очередь догоняющих запусков. Одновременно допускается только один ожидающий/выполняющийся запуск одной политики. Общие лимиты параллельности агента и медиасервера продолжают действовать. Приостановка политики отменяет ожидающие проверки; выполняющийся запуск завершает свою проверку и очистку.

Выбирается последняя успешно проверенная резервная копия исходного задания, содержащая нужный namespace, с активным сроком хранения. Для чтения используется агент и медиасервер этой копии. Отсутствие подходящей копии фиксируется как неуспешная проверка; старый результат не выдаётся за новый успех. Фактическую доступность архива дополнительно проверяет медиасервер при восстановлении.

После восстановления Deployment/StatefulSet/DaemonSet ожидается хотя бы один Running Pod с условием Ready=True и отсутствие других неготовых незавершённых Pod. Завершённые Pod Succeeded не учитываются; Failed приводит к ошибке. CronJob остаются приостановленными, Ingress не создаются. Пустой namespace или приложение только из приостановленных CronJob не проходит проверку готовности. HTTP-запросы выполняются через Kubernetes API Service proxy к Service с selector внутри того же временного namespace; ExternalName и Service без selector запрещены. Ответ ограничен 1 МиБ, его тело не записывается в отчёт. Сравнение фрагмента чувствительно к регистру; тайм-аут включает восстановление и все проверки, но не отдельное окно очистки. Проверка HTTP подтверждает только выбранный endpoint, а не все транзакции приложения.

Используйте выделенный тестовый кластер или заранее подготовленные сетевые политики: создание namespace само по себе не изолирует приложение от рабочих внешних систем. Произвольные адреса в ConfigMap/Secret не меняются. Проверки запрещают hostNetwork/hostPID/hostIPC, hostPath и privileged containers; это не заменяет Pod Security Admission. Образы и нужные Secrets должны быть доступны в тестовом кластере.

### Отчёт и очистка

В отчёте доступны ID исходной копии, namespace, время начала/окончания, общая длительность, число готовых Pod, результат каждого HTTP-запроса и итог очистки. Общая длительность включает очистку и не является измерением RTO рабочего сервиса. Журнал восстановления дополнен этапами проверки и очистки; итоговый локальный отчёт находится в `staging_dir/restore-journals/<RunID>.check.json`.

По умолчанию очистка запускается после успеха и после ошибки/отмены. Опция **«Сохранять ресурсы при ошибке»** оставляет их для диагностики. Очистка получает отдельный тайм-аут 180 секунд, проверяет UID namespace и метку RunID, привязку PVC/PV и Delete reclaim policy. Удаление namespace выполняется с UID/resourceVersion preconditions. Затем ожидается исчезновение namespace и известных PV, удаляются только принадлежащие запуску VolumeSnapshotContent с политикой Retain. Чужие ресурсы, изменённые привязки, отказ прав или зависшие finalizers дают ошибку очистки; принудительного снятия finalizers и удаления исходных backup данных нет. Неуспешная очистка делает весь запуск неуспешным.

При аварийном завершении процесса автоматическая очистка не гарантируется: менеджер помечает прерванный запуск ошибкой. Используйте локальный журнал, список `<RunID>.cleanup-pvs.json` и метки RunID для ручной проверки оставшихся ресурсов. Каталог проверки в БД подчиняется общим правилам хранения истории операций.

Дополнительные RBAC-права приведены в `packaging/kubernetes/srk-restore-rbac.yaml`: list/get Pod и PVC, get services/proxy, delete namespaces, list/get/delete VolumeSnapshotContent. Пример выдаёт права на уровне кластера и предназначен для выделенной доверенной тестовой учётной записи.

### API и CLI

`GET /v1/kubernetes-checks` возвращает политики и последние запуски. `POST /v1/kubernetes-checks` создаёт или обновляет политику по сочетанию задания, целевого профиля и исходного namespace. Сервер назначает ID и время следующего запуска; для отключения отправьте ту же политику с `enabled: false`.

Пример файла `check.json`:

```json
{
  "job_id": "ID_ЗАДАНИЯ_32_СИМВОЛА",
  "profile": "production",
  "source_namespace": "demo",
  "namespace_prefix": "restore-check-",
  "interval_seconds": 86400,
  "timeout_seconds": 600,
  "enabled": true,
  "keep_on_failure": false,
  "http": [
    {"service": "app", "port": 8080, "path": "/health", "expected_status": 200, "contains": "ok"}
  ]
}
```

```sh
srk --config /etc/srk/admin.json manager kubernetes-checks put --file check.json
srk --config /etc/srk/admin.json --json manager kubernetes-checks list
srk --config /etc/srk/admin.json --json manager runs get RUN_ID
```

Контракт HTTP-прокси: [Kubernetes Service API](https://kubernetes.io/docs/reference/kubernetes-api/core/service-v1/).
