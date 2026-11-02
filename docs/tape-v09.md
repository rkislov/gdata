# Ленточный контур 0.9.0

Реализован постоянный **эмулятор** библиотеки: слоты, приводы, кассеты, загрузка/выгрузка, логическая маркировка, каталог, запись, проверка чтением, восстановление, импорт неизвестного каталога, удержание, быстрое/полное стирание и подготовка к перезаписи. Физический SCSI-транспорт TS4300/LTO-9 пока не реализован и не квалифицирован; эмулятор не является отдельным носителем для правила 3-2-1.

В конфигурацию медиасервера с `development: true` добавьте:

```json
{"tape":{"backend":"emulator","library_id":"LAB","drives":["DRIVE1","DRIVE2"],"cartridges":[{"barcode":"SRK001L9","slot":1,"generation":9,"capacity_bytes":536870912},{"barcode":"SRK002L9","slot":2,"generation":9,"capacity_bytes":536870912}]}}
```

Состояние хранится в `repository/tape-emulator`. Каталог и журнал имеют контрольные суммы и атомарную публикацию. Не изменяйте топологию существующей библиотеки вручную. Все изменения доступны администратору через управление; прямые изменяющие запросы на media принимаются только с сертификатом manager. Аудитор имеет доступ к чтению.

## Интерфейс и CLI

«Хранилища» → «Кассеты и библиотека». Операции проходят через общую очередь, отображают прогресс и журнал. Разные приводы могут работать параллельно; занятая кассета не может быть выгружена. Штрихкод неизменяемый, логическая метка редактируется. Маркировка неизвестного записанного носителя запрещена до импорта.

```sh
srk --config .local/admin.json manager tape inventory media-local
srk --config .local/admin.json --wait manager tape load media-local SRK001L9 --drive DRIVE1
srk --config .local/admin.json --wait manager tape label media-local SRK001L9 --label 'Offsite 01' --reason 'Первичная маркировка' --confirm-barcode SRK001L9
srk --config .local/admin.json --wait manager tape write media-local SRK001L9 --run BACKUP_RUN_ID --retention-days 30
srk --config .local/admin.json --wait manager tape verify media-local SRK001L9
srk --config .local/admin.json --wait manager tape restore media-local SRK001L9 --copy COPY_ID --agent agent-local
srk --config .local/admin.json --wait manager tape import media-local SRK001L9
srk --config .local/admin.json --wait manager tape hold media-local SRK001L9 --copy COPY_ID
srk --config .local/admin.json --wait manager tape release-hold media-local SRK001L9 --copy COPY_ID --reason 'Согласовано' --confirm-barcode SRK001L9
srk --config .local/admin.json --wait manager tape erase media-local SRK001L9 --mode long --reason 'Сроки истекли' --confirm-barcode SRK001L9
srk --config .local/admin.json --wait manager tape recycle media-local SRK001L9 --reason 'Повторное использование' --confirm-barcode SRK001L9
srk --config .local/admin.json --wait manager tape unload media-local SRK001L9
```

`recycle` очищает кассету после проверки всех сроков и готовит её к новой записи. Запись сама по себе добавляет независимую копию и не перезаписывает предыдущую. Для стирания/маркировки/снятия удержания нужны причина и точный штрихкод. WORM, write-protect, неизвестный каталог, действующие сроки и удержание блокируют стирание. WORM-запись в эмуляторе также отключена. Снятие удержания не сокращает срок хранения.

Быстрое стирание удаляет файлы эмулятора; полное сначала записывает нули и синхронизирует файлы. Это не гарантия физического уничтожения данных SSD/COW. Незавершённая запись остаётся dirty; неизвестные файлы блокируют очистку. Прерванное стирание имеет постоянный признак erasing и может быть продолжено новой явно подтверждённой операцией после повторной проверки ограничений. Повтор завершённого operation ID возвращает сохранённый результат; повтор незавершённого требует проверки состояния.

## Автономный формат

Один файл `.srktape` содержит magic SRKTAPE1, ограниченный по размеру JSON-заголовок с полным манифестом, уникальные блоки в порядке SHA-256 и итоговый SHA-256 всего потока. Блоки проверяются отдельно. Дедупликация действует внутри каждой автономной копии; между копиями зависимости не создаются. После записи выполняется повторное чтение. Недостаток ёмкости обнаруживается до начала записи; продолжение копии на следующей кассете пока не поддерживается.

Логическая метка и изменения удержания сохраняются на эмулируемом носителе в `label.json`; импорт восстанавливает их после потери центрального каталога. При импорте проверяется весь архив. Восстановление создаёт новую полную копию в дисковом репозитории, проверяет блоки и регистрирует её с удержанием. После этого доступны обычные сценарии восстановления файлов, БД, диска или ВМ. Если регистрация в каталоге управления прервалась, используйте существующий импорт дискового репозитория. Непрерывный архив WAL отдельно на ленту в этой версии не переносится.

## API

- `GET /v1/media/{media}/tape` — инвентаризация и каталог.
- `POST /v1/media/{media}/tape/operations` — TapeRequest, ответ Run (202).
- `GET /v1/runs/{id}` и `/events` — результат, прогресс, журнал.
- media: `GET /v1/tape/capabilities`, `GET /v1/tape`, внутренние `POST/GET /v1/tape/operations/{id}`.

Поля TapeRequest: action, barcode, drive_id, label, confirm (точный штрихкод), reason, erase_mode (quick/long), snapshot_id (ID исходного запуска для write), copy_id, agent_id, retention_days, hold. Допустимые action: load, unload, label, write, verify, import, restore, erase, recycle, hold, release-hold.

## Переход к оборудованию

Для физического транспорта необходимо реализовать отдельные control/data paths, SCSI READ ELEMENT STATUS/MOVE MEDIUM, обработку sense/EOM/filemarks, устойчивую идентификацию приводов и испытания отказов. Основания: [IBM TS4300 User Guide](https://www.ibm.com/support/pages/system/files/inline-files/TS4300_user_guide_SC27-4629-08_0.pdf), [Linux SCSI tape driver](https://www.kernel.org/doc/html/latest/scsi/st.html). Программные операции эмулятора не выдаются за аппаратную совместимость.
