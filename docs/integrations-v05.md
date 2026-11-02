# Интеграции виртуализации и ленточных устройств — основы 0.5

## API виртуализации

Прокси имеет реальные HTTPS-клиенты инвентаризации четырёх платформ. Контрактные тесты проверяют путь запроса, аутентификацию и разбор ответа. Испытаний с настоящими гипервизорами пока не было. Полный перенос дисков в SRK и восстановление ВМ ещё не реализованы: `plan.executable=false`, запуск backup на proxy по-прежнему возвращает 501. Создание snapshot само по себе не считается резервной копией.

На proxy добавляется `virtualization` в JSON-конфигурацию, например:

```json
{"virtualization":{"lab":{"kind":"proxmox","endpoint":"https://pve.example:8006/api2/json","ca_file":"/etc/srk/pve-ca.pem","credential_file":"/etc/srk/pve-token.json"}}}
```

Файл секрета — абсолютный путь, обычный файл 0600/0400. HTTPS обязательно, CA проверяется, HTTP redirects запрещены, размер ответа ограничен. Профили не передают секреты менеджеру. CLI:

```
srk --config admin.json --endpoint https://proxy.example:8446 proxy virtualization profiles
srk --config admin.json --endpoint https://proxy.example:8446 proxy virtualization list lab
srk --config admin.json --endpoint https://proxy.example:8446 proxy virtualization plan lab VM_ID
```

- OpenStack: endpoint Nova Compute с project ID, секрет `{"token":"…"}`, `X-Auth-Token`, `GET /servers/detail`, постраничный marker. Дальнейший backup требует различать диски из образов и Cinder volumes, получать согласованные снимки и выгружать данные через Glance/Cinder. Tionix/OpenStack Queens требует отдельной проверки поддерживаемых микроверсий. [Nova Compute API](https://docs.openstack.org/api-ref/compute/).
- oVirt: endpoint `https://engine.example/ovirt-engine/api`, секрет `{"token":"…"}`, OAuth bearer, `GET /vms?max=10000`. План учитывает native backup API и image transfers; наличие incremental backup зависит от версии и хранилища. [oVirt Administration Guide](https://ovirt.org/documentation/administration_guide/).
- Proxmox: endpoint `https://host:8006/api2/json`, секрет `{"token_id":"user@pve!srk","secret":"…"}`, `PVEAPIToken`, `GET /cluster/resources?type=vm`; выдаются QEMU ВМ, LXC исключены. Следующий этап: vzdump, контроль UPID, перенос архива и проверенное восстановление. [официальный API viewer](https://pve.proxmox.com/pve-docs/api-viewer/).
- OpenNebula: HTTPS XML-RPC endpoint, секрет `{"session":"user:token"}`, `one.vmpool.info` с шестью аргументами интерфейса 7.2. Старые версии с иным числом аргументов требуют отдельного профиля совместимости. Выдаётся область пользователя и групп. [OpenNebula XML-RPC API](https://docs.opennebula.io/7.2/product/integration_references/system_interfaces/api/).

Ни один текущий read-only адаптер не вызывает удалённое создание/удаление снимков. Рабочие credentials следует ограничить необходимыми правами; в 0.5 токены задаются заранее, автоматический выпуск и обновление OAuth/Keystone токенов не реализованы.

## IBM TS4300 / LTO-9

Пакет `internal/tape` определяет инвентаризацию, приводы, кассеты, сроки хранения и fail-closed правила планирования загрузки/перезаписи. `media tape capabilities` и `GET /v1/tape/capabilities` явно сообщают `planning_only`, `writes=false`, `inventory=false`. SCSI transport пока отсутствует, кассеты с этого интерфейса не изменяются. Совместимость с конкретной TS4300 не сертифицирована.

Из [руководства IBM TS4300](https://www.ibm.com/support/pages/system/files/inline-files/TS4300_user_guide_SC27-4629-08_0.pdf) следует раздельная организация управления библиотекой/логической библиотекой и потоковой работы приводов. План реализации:

1. Linux `/dev/tape/by-id` для устойчивой идентификации, non-rewind streaming device для данных, отдельный control path роботики. Проверка INQUIRY/VPD, serial, поколения, logical-library partition. Учет настроенной роли control-path drive; адреса элементов узнаются у библиотеки.
2. SMC READ ELEMENT STATUS для слотов, mail slots и приводов; MOVE MEDIUM только по проверенному плану и эксклюзивной аренде роботики. SSC/TUR/REQUEST SENSE для состояния привода и ошибок; SCSI-команды и сетевой интерфейс IBM не смешиваются в один универсальный адаптер.
3. Независимая очередь каждого привода и отдельная очередь роботики. Никаких одновременных записей на кассету. Политика удержания всех копий, WORM, write-protect, cleaning tape, unknown catalog и active leases блокируют перезапись.
4. Поддержка LTO-9 требует чтения/записи подходящих LTO-8/9 data media и квалификации firmware/драйверов на реальном оборудовании. Текущий планировщик намеренно не принимает другие поколения.
5. Поток записи должен иметь самодостаточный заголовок тома, уникальный media-set ID, манифест, контрольные суммы, filemarks, учет block mode и лимитов привода. Обработка EOM, spanning, частичной записи, извлечения и повторов требует журнала. Формат данных на ленте пока не утверждён.
6. После записи — закрытие потока, filemarks/синхронизация, независимое контрольное чтение. Импорт незнакомой кассеты строит каталог по её данным, а не по старым barcode. Логический expiry не равен безопасному физическому стиранию и не позволяет стирать WORM.
7. Вывоз/off-site и физическая offline-изоляция отражаются в каталоге. Лента считается доступной для восстановления только после подтверждённой загрузки; RTO включает ручной возврат и позиционирование.

Для Linux transport будут применяться документированные SCSI primitives; набор диагностических утилит описан в [sg3_utils](https://sg.danny.cz/sg/sg3_utils.html). Автоматически запускать `mt erase`, форматирование или MOVE MEDIUM без аппаратных испытаний и проверенного плана система пока не умеет.
