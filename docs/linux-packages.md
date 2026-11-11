# Пакеты GData SRK для Linux

Пакеты GData SRK 0.10.0 рассчитаны на x86_64 (amd64). Загрузите пакет своей
системы и `SHA256SUMS-linux-packages` из [релиза v0.10.0](https://github.com/rkislov/gdata/releases/tag/v0.10.0).

- ALT Linux p11 — `gdata-srk-0.10.0-alt1.p11.x86_64.rpm`
- РЕД ОС 8 — `gdata-srk-0.10.0-1.redos8.x86_64.rpm`
- Astra Linux Special Edition 1.8 — `gdata-srk_0.10.0-1.astra1.8_amd64.deb`
- Ubuntu 24.04 LTS — `gdata-srk_0.10.0-1.ubuntu24.04_amd64.deb`
- CentOS Stream 9 — `gdata-srk-0.10.0-1.el9.x86_64.rpm`

## Установка

В каталоге загрузки проверьте SHA-256. Продолжайте, если сумма выбранного пакета
совпала.

```sh
sha256sum --ignore-missing -c SHA256SUMS-linux-packages
```

Выполните команды для своей системы от root.

**ALT Linux p11**

```sh
apt-get update
apt-get install ./gdata-srk-0.10.0-alt1.p11.x86_64.rpm
```

**РЕД ОС 8**

```sh
dnf install ./gdata-srk-0.10.0-1.redos8.x86_64.rpm
```

**Astra Linux SE 1.8**

```sh
apt-get update
apt-get install ./gdata-srk_0.10.0-1.astra1.8_amd64.deb
```

**Ubuntu 24.04 LTS**

```sh
apt-get update
apt-get install ./gdata-srk_0.10.0-1.ubuntu24.04_amd64.deb
```

**CentOS Stream 9**

```sh
dnf install ./gdata-srk-0.10.0-1.el9.x86_64.rpm
```

Проверьте установленную версию командой `srk version`. Пакет создаёт системного
пользователя `srk` и каталоги `/etc/srk`, `/var/lib/srk`, `/var/log/srk`.
Настройте PostgreSQL, сертификаты и конфигурации ролей по
[руководству администратора](admin-guide.md#подготовка-рабочей-инсталляции).
Для каждой настроенной роли включите соответствующую службу
`srk@manager`, `srk@media`, `srk@agent` или `srk@proxy`.
Установка пакета оставляет запуск служб администратору.

## Локальная сборка

На машине с Go, Docker и поддержкой запуска контейнеров linux/amd64 выполните
`make linux-packages`. Результаты и общий файл контрольных сумм появятся
в `dist/linux-packages/`. Сборка устанавливает каждый пакет внутри контейнера
целевой системы и проверяет CLI, права каталогов и конфигурации четырёх ролей.

Можно собрать отдельный пакет, например
`sh scripts/build-linux-packages.sh astra18`. Имена целей:
`alt-p11`, `redos8`, `astra18`, `ubuntu2404`, `centos9`.
Для одной цели `SRK_PACKAGE_IMAGE` позволяет указать свой образ той же системы.
Сборка ALT Linux p10 по-прежнему доступна через `make altlinux`.
