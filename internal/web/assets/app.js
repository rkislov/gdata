// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
"use strict";
const $ = (s) => document.querySelector(s),
  esc = (v) =>
    String(v ?? "").replace(
      /[&<>"']/g,
      (c) =>
        ({
          "&": "&amp;",
          "<": "&lt;",
          ">": "&gt;",
          '"': "&quot;",
          "'": "&#39;",
        })[c],
    );
const state = {
  page: "overview",
  tab: "now",
  jobs: [],
  runs: [],
  nodes: [],
  audit: [],
  status: {},
  search: "",
  filter: "all",
  from: "",
  to: "",
  last: null,
  logged: false,
  offset: 0,
};
const date = (v) =>
  v
    ? new Intl.DateTimeFormat("ru-RU", {
        day: "2-digit",
        month: "short",
        hour: "2-digit",
        minute: "2-digit",
      }).format(new Date(v))
    : "—";
const size = (n) => {
  n = Number(n) || 0;
  if (!n) return "0 Б";
  const i = Math.min(4, Math.floor(Math.log(n) / Math.log(1024)));
  return `${(n / 1024 ** i).toFixed(i ? 1 : 0)} ${["Б", "КиБ", "МиБ", "ГиБ", "ТиБ"][i]}`;
};
const statusText = {
  queued: "В очереди",
  running: "Выполняется",
  success: "Успешно",
  error: "Ошибка",
  cancelled: "Отменено",
};
const kindText = {
  backup: "Резервное копирование",
  replicate: "Репликация",
  "tape-write": "Запись на кассету",
  "tape-restore": "Восстановление с кассеты",
  "tape-erase": "Стирание кассеты",
  "tape-recycle": "Подготовка кассеты к перезаписи",
  "tape-label": "Маркировка кассеты",
  "tape-load": "Загрузка кассеты",
  "tape-unload": "Выгрузка кассеты",
  "tape-import": "Импорт кассеты",
  "tape-verify": "Проверка кассеты",
  "tape-hold": "Удержание копии на ленте",
  "tape-release-hold": "Снятие удержания на ленте",
  "wal-replicate": "Репликация архива WAL",
  restore: "Восстановление",
  "wal-gc": "Очистка WAL",
  verify: "Проверка целостности",
  gc: "Очистка хранилища",
  import: "Импорт репозитория",
  policy: "Изменение хранения",
};
const badge = (s) =>
  `<span class="badge ${s === "success" ? "bg-emerald-50 text-emerald-600" : s === "running" ? "bg-brand-50 text-brand-600" : s === "error" ? "bg-rose-50 text-rose-600" : "bg-slate-100 text-slate-500"}"><span aria-hidden="true">${s === "success" ? "✓" : s === "error" ? "!" : s === "running" ? "◉" : "◷"}</span>${esc(statusText[s] || s)}</span>`;
const empty = (title, desc) =>
  `<div class="tile col-span-full flex min-h-56 flex-col items-center justify-center text-center"><span class="mb-4 flex size-14 items-center justify-center rounded-2xl bg-brand-50 text-2xl text-brand-400" aria-hidden="true">◫</span><h3 class="font-semibold text-slate-700">${title}</h3><p class="mt-2 max-w-md text-sm leading-6 text-slate-400">${desc}</p></div>`;
let accessEpoch=0;
function clearAccessView(){
 accessEpoch++;state.storageView=null;state.nodes=[];state.jobs=[];state.runs=[];state.audit=[];state.status={};state.accessKey=null;
 document.querySelectorAll("dialog[open]").forEach(d=>{d.close();d.innerHTML="";});
 $("#content").innerHTML="";
}
async function api(url, options = {}) {
  const epoch=accessEpoch;
  const res = await fetch(url, {
    credentials: "same-origin",
    ...options,
    headers: { "Content-Type": "application/json", ...options.headers },
  });
  let data;
  try {
    data = await res.json();
  } catch {
    throw Error("Неверный ответ сервиса");
  }
  if(epoch!==accessEpoch)throw Error("Учётная запись изменилась; обновите данные");
  if (!res.ok) {
    if (res.status === 401 && url != "/v1/login") {
      clearAccessView();
      state.logged = false;
      $("#login").hidden = false;
    }
    throw Error(data.error || `Ошибка ${res.status}`);
  }
  return data;
}
const post = (url, body) =>
  api(url, {
    method: "POST",
    body: body ? JSON.stringify(body) : undefined,
    headers: { "Idempotency-Key": crypto.randomUUID() },
  });
function notice(message) {
  $("#notice").textContent = message;
  $("#notice").hidden = !message;
}
function matches(r) {
  return (
    (!state.search ||
      `${r.job?.name} ${r.id} ${r.job?.source} ${r.error || ""}`
        .toLowerCase()
        .includes(state.search.toLowerCase())) &&
    (state.filter === "all" || r.status === state.filter) &&
    (!state.from || new Date(r.created_at) >= new Date(state.from)) &&
    (!state.to || new Date(r.created_at) < new Date(state.to))
  );
}
function runCard(r) {
  const result = r.result;
  const agent = state.nodes.find((n) => n.id === r.job?.agent_id);
  const progress = agent?.status?.operations?.[r.id];
  const live = result || progress;
  const phase =
    {
      "postgresql-basebackup": "PostgreSQL: получение базы и WAL",
      "disk-upload": "Физический диск: передача блоков",
      "disk-verification": "Физический диск: проверка всех блоков",
      "postgresql-validation": "PostgreSQL: проверка файлов и WAL",
      "postgresql-unpack": "PostgreSQL: подготовка файлов базы",
      "postgresql-wal-download": "PostgreSQL: загрузка WAL",
      "postgresql-pitr-recovery":
        "PostgreSQL: восстановление до выбранного момента",
      "postgresql-upload": "PostgreSQL: передача в хранилище",
    }[live?.phase] ||
    (["openstack", "ovirt", "proxmox", "opennebula"].includes(r.job?.type) &&
    typeof vmPhase === "function"
      ? vmPhase(live?.phase)
      : "");
  return `<article class="tile flex flex-col gap-4"><div class="flex items-start justify-between gap-3"><div class="flex size-11 shrink-0 items-center justify-center rounded-2xl ${r.kind === "restore" ? "bg-sky-50 text-sky-500" : "bg-brand-50 text-brand-500"} text-xl" aria-hidden="true">${r.kind === "restore" ? "↙" : "↗"}</div>${badge(r.status)}</div><div><p class="text-xs text-slate-400">${esc(kindText[r.kind] || r.kind)}</p><h3 class="mt-1 truncate font-semibold" title="${esc(r.job?.name)}">${esc(r.job?.name)}</h3><p class="mt-1 truncate text-xs text-slate-400">${esc(r.job?.source)}</p></div><div class="grid grid-cols-2 gap-3 border-y border-slate-100 py-4"><div><p class="text-[10px] uppercase tracking-wider text-slate-400">${r.maintenance ? "Удалено данных" : "Прочитано"}</p><p class="mt-1 text-lg font-semibold tracking-tight">${r.maintenance ? size(r.maintenance.bytes_removed) : live ? size(live.logical_bytes) : "—"}</p></div><div><p class="text-[10px] uppercase tracking-wider text-slate-400">${r.maintenance ? (r.kind === "import" ? "Импортировано" : r.kind === "wal-gc" ? "Удалено WAL" : "Удалено копий") : "Новых данных"}</p><p class="mt-1 text-lg font-semibold tracking-tight">${r.maintenance ? (r.kind === "import" ? r.maintenance.imported : r.kind === "wal-gc" ? r.maintenance.wal_removed : r.maintenance.snapshots_removed) : live ? size(live.uploaded_bytes) : "—"}</p></div></div>${operationProgress(r, live)}${dedupPanel(live, r.kind)}${kubernetesPanel(live)}${phase ? `<p class="text-xs text-brand-600">${esc(phase)}</p>` : ""}${r.error ? `<p class="line-clamp-2 text-xs leading-5 text-rose-500">${esc(r.error)}</p>` : ""}<div class="mt-auto flex items-center justify-between"><span class="text-xs text-slate-400">${date(r.created_at)}</span><button data-detail="${esc(r.id)}" class="text-xs font-semibold text-brand-600">Подробнее ↗</button></div></article>`;
}
function stats() {
  const active = state.status.counts?.running ?? 0,
    queued = state.status.counts?.queued ?? 0,
    good = state.status.counts?.successful_backups ?? 0,
    errors = state.status.counts?.errors ?? 0;
  return `<div class="mb-7 grid grid-cols-2 gap-5 xl:grid-cols-4">${[
    [
      "Выполняется",
      active,
      `${queued} в очереди`,
      "bg-brand-50 text-brand-500",
      "◉",
    ],
    [
      "Успешные копии",
      good,
      "В сохраненной истории",
      "bg-emerald-50 text-emerald-500",
      "✓",
    ],
    [
      "Требуют внимания",
      errors,
      "Ошибки операций",
      "bg-amber-50 text-amber-500",
      "!",
    ],
    [
      "Компоненты",
      state.nodes.filter((n) => Date.now() - new Date(n.last_seen) < 30000)
        .length,
      `Из ${state.nodes.length} зарегистрированных`,
      "bg-sky-50 text-sky-500",
      "⌘",
    ],
  ]
    .map(
      ([label, n, sub, color, icon]) =>
        `<div class="tile"><div class="flex items-center justify-between"><p class="text-xs font-medium text-slate-500">${label}</p><span class="flex size-8 items-center justify-center rounded-xl ${color}">${icon}</span></div><p class="mt-4 text-4xl font-semibold tracking-tight">${n}</p><p class="mt-2 text-xs text-slate-400">${sub}</p></div>`,
    )
    .join("")}</div>`;
}
function upcoming() {
  const jobs = state.jobs
    .filter(
      (j) =>
        j.enabled &&
        (!state.search ||
          `${j.name} ${j.source}`
            .toLowerCase()
            .includes(state.search.toLowerCase())) &&
        (!state.from || new Date(j.next_run) >= new Date(state.from)) &&
        (!state.to || new Date(j.next_run) < new Date(state.to)),
    )
    .sort((a, b) => new Date(a.next_run) - new Date(b.next_run));
  return jobs.length
    ? `<div class="grid gap-5 md:grid-cols-2 xl:grid-cols-3">${jobs.map((j) => `<article class="tile"><div class="flex items-center justify-between"><span class="badge bg-sky-50 text-sky-600">◷ Запланировано</span><span class="text-xs text-slate-400">${esc(j.mode)}</span></div><h3 class="mb-1 mt-5 font-semibold">${esc(j.name)}</h3><p class="truncate text-xs text-slate-400">${esc(j.source)}</p><p class="mb-4 mt-6 text-xl font-medium">${date(j.next_run)}</p><p class="border-t border-slate-100 pt-3 text-xs leading-5 text-slate-400">Старт зависит от доступности ресурсов.<br>Агент: ${esc(j.agent_id)} · Хранение ${j.retention_days} дн.</p></article>`).join("")}</div>`
    : empty(
        "Предстоящих запусков нет",
        "Создайте задание с расписанием или включите существующее.",
      );
}
function activity() {
  let rows = state.runs.filter(matches);
  if (state.tab === "now")
    rows = rows.filter((r) => ["running", "queued"].includes(r.status));
  else rows = rows.filter((r) => !["running", "queued"].includes(r.status));
  return `<section><div class="mb-5 flex flex-wrap items-center justify-between gap-4"><div class="flex rounded-2xl bg-slate-200/50 p-1" role="group" aria-label="Период активности">${[
    ["now", "Сейчас"],
    ["history", "История"],
    ["upcoming", "Предстоящие"],
  ]
    .map(
      ([k, t]) =>
        `<button class="segment ${state.tab === k ? "active" : ""}" data-tab="${k}" aria-pressed="${state.tab === k}">${t}</button>`,
    )
    .join(
      "",
    )}</div><span class="text-xs text-slate-400">${state.tab === "upcoming" ? "План ближайшего запуска каждого задания" : `${rows.length} операций · обновление каждые 2 с`}</span></div><div class="mb-5 flex flex-wrap gap-3"><input id="search" class="field max-w-xs bg-white" value="${esc(state.search)}" placeholder="Поиск по заданию, пути или ID" aria-label="Поиск активности"><select id="status-filter" class="field max-w-44 bg-white" aria-label="Статус"><option value="all">Все статусы</option>${Object.entries(
    statusText,
  )
    .map(
      ([v, k]) =>
        `<option value="${v}" ${state.filter === v ? "selected" : ""}>${k}</option>`,
    )
    .join(
      "",
    )}</select><input id="date-from" type="datetime-local" class="field max-w-56 bg-white" value="${esc(state.from)}" aria-label="Начало периода"><input id="date-to" type="datetime-local" class="field max-w-56 bg-white" value="${esc(state.to)}" aria-label="Конец периода"></div>${state.tab === "upcoming" ? upcoming() : `<div class="grid gap-5 md:grid-cols-2 xl:grid-cols-3">${rows.length ? rows.map(runCard).join("") : empty(state.tab === "now" ? "Все спокойно. Активных операций нет." : "Операции не найдены", state.tab === "now" ? "Запустите задание или дождитесь ближайшего запланированного копирования." : "Измените фильтры или создайте первую резервную копию.")}</div>`}</section>`;
}
function nodes() {
  const allNodes = componentNodes();
  return `<div class="grid gap-5 md:grid-cols-2">${
    allNodes.length
      ? allNodes
          .map((n) => {
            const online = Date.now() - new Date(n.last_seen) < 30000;
            return `<article class="tile"><div class="mb-5 flex justify-between"><span class="text-xs uppercase tracking-widest text-slate-400">${esc(n.role)}</span><span class="badge ${online ? "bg-emerald-50 text-emerald-600" : "bg-rose-50 text-rose-600"}">${online ? "● Доступен" : "! Нет связи"}</span></div><h3 class="text-xl font-semibold">${esc(n.id)}</h3><p class="mt-2 text-sm text-slate-500">${esc(n.url)}</p>${hostPanel(n.status?.host, n.last_seen, false, n.id)}${walArchivePanel(n.status?.wal_archives)}<p class="mt-5 text-xs text-slate-400">Последний сигнал: ${date(n.last_seen)}</p>${n.role === "proxy" ? '<p class="mt-4 rounded-xl bg-amber-50 px-3 py-2 text-xs text-amber-700">Профили виртуализации доступны через «Бэкап ВМ»</p>' : ""}</article>`;
          })
          .join("")
      : empty(
          "Нет подключенных компонентов",
          "Запустите медиасервер и агент с сертификатами этой инсталляции.",
        )
  }</div>`;
}
function storage() {
  if(state.status.restricted){
    const d=state.storageView;
    if(!d)return empty("Доступные хранилища","Загружается разрешённый инвентарь…");
    const copies=d.cards.flatMap(c=>c.copies.map(cp=>({...cp,job_name:c.job.name})));
    return `<p class="mb-5 text-sm text-slate-500">Только разрешённые задания и площадки. Общая ёмкость, дедупликация, чужие копии и журналы носителей не раскрываются.</p><div class="grid gap-5 md:grid-cols-2">${d.settings.storages.map(s=>{const points=copies.filter(cp=>cp.storage_id===s.id);return `<article class="tile"><h3 class="text-xl font-semibold">${esc(s.name)}</h3><p class="text-sm my-3">${esc(s.kind)} · ${esc(d.settings.sites.find(x=>x.id===s.site_id)?.name||s.site_id)}</p><p class="text-sm">Доступных точек в карточках: ${points.length}</p>${points.map(cp=>`<p class="text-xs my-3">${esc(cp.job_name)} · ${date(cp.data_at)} · ${esc(cp.snapshot_id)} · ${cp.available&&cp.verified?'Подтверждена':'Не подтверждена'}</p>`).join('')}${s.kind==='repository'?`<button class="btn-secondary mt-4" data-repository="${esc(s.media_id)}">Доступные копии</button>`:''}</article>`;}).join('')||empty("Нет доступных хранилищ","Нет копий, сопоставленных с разрешёнными заданиями и площадками.")}</div>`;
  }
  const media = state.nodes.filter((n) => n.role === "media").map(n=>({...n,status:n.status||{}}));
  return `<div class="grid gap-5 md:grid-cols-2">${media.length ? media.map((n) => `<article class="tile"><div class="flex items-start justify-between"><span class="flex size-12 items-center justify-center rounded-2xl bg-brand-50 text-2xl text-brand-500">▱</span><span class="badge bg-slate-100 text-slate-500">Дисковый репозиторий</span></div><h3 class="mt-6 text-xl font-semibold">${esc(n.id)}</h3><p class="mt-1 text-xs text-slate-400">Блочная дедупликация · SHA-256 · 4 МиБ</p><div class="my-6 grid grid-cols-3 gap-3"><div><p class="muted">Занято</p><p class="mt-2 text-xl font-semibold">${size(n.status.physical_bytes)}</p></div><div><p class="muted">Блоков</p><p class="mt-2 text-xl font-semibold">${n.status.chunks ?? 0}</p></div><div><p class="muted">Копий</p><p class="mt-2 text-xl font-semibold">${n.status.snapshots ?? 0}</p></div></div>${repositoryDedup(n.status)}<div class="mb-5"><div class="mb-2 flex justify-between text-xs text-slate-500"><span>Файловая система медиасервера</span><span>Свободно ${size(n.status.filesystem_available_bytes)} / ${size(n.status.filesystem_total_bytes)}</span></div><progress class="h-2 w-full accent-brand-500" max="100" value="${n.status.filesystem_total_bytes ? Math.round((1 - n.status.filesystem_available_bytes / n.status.filesystem_total_bytes) * 100) : 0}" aria-label="Занято на файловой системе"></progress></div><div class="mb-5 flex flex-wrap gap-2"><button class="btn-secondary" data-repository="${esc(n.id)}">Копии и импорт</button><button class="btn-secondary" data-gc-preview="${esc(n.id)}">Очистка</button><button class="btn-secondary" data-tape-library="${esc(n.id)}">Кассеты и библиотека</button><button class="btn-secondary" data-removable="${esc(n.id)}">Flash / USB</button></div><p class="border-t border-slate-100 pt-4 text-xs leading-5 text-slate-400">Обновлено ${date(n.last_seen)}. Емкость ФС включает данные других приложений. Резерв: ${size(n.status.reserve_bytes)}. Правило 3-2-1 пока не оценивается.</p></article>`).join("") : empty("Хранилища не подключены", "Запустите медиасервер для регистрации дискового репозитория.")}<article class="tile border-dashed border-slate-200 bg-transparent shadow-none"><span class="badge bg-amber-50 text-amber-600">Следующий этап</span><h3 class="mt-5 text-lg font-semibold">Ленточный контур 0.9</h3><p class="mt-3 text-sm leading-6 text-slate-500">Каталог, маркировка, стирание и автономные копии проверяются на эмуляторе. Linux SCSI поддерживает параллельные приводы, очередь, импорт и восстановление. Для IBM TS4300/LTO-9 требуется аппаратная квалификация.</p></article></div>`;
}
function jobs() {
  return `<div class="grid gap-5 md:grid-cols-2 xl:grid-cols-3">${state.jobs.length ? state.jobs.map((j) => `<article class="tile"><div class="flex justify-between"><span class="badge ${j.enabled ? "bg-emerald-50 text-emerald-600" : "bg-slate-100 text-slate-500"}">${j.enabled ? "◷ По расписанию" : "Вручную"}</span><span class="text-xs text-slate-400">v${j.version}</span></div><h3 class="mt-5 font-semibold">${esc(j.name)}</h3><p class="mt-2 truncate text-xs text-slate-500">${esc(j.source)}</p><dl class="my-5 space-y-2 text-xs text-slate-500"><div class="flex justify-between"><dt>Тип</dt><dd>${esc(j.mode)}</dd></div><div class="flex justify-between"><dt>Хранение</dt><dd>${j.retention_days} дней</dd></div><div class="flex justify-between"><dt>Следующий запуск</dt><dd>${j.enabled ? date(j.next_run) : "—"}</dd></div></dl><div class="flex gap-2 border-t border-slate-100 pt-4"><button class="btn text-xs" data-run="${esc(j.id)}">▷ Запустить</button>${j.interval_seconds ? `<button class="btn-secondary text-xs" data-toggle="${esc(j.id)}">${j.enabled ? "Приостановить" : "Включить"}</button>` : ""}</div></article>`).join("") : empty("Первое задание начинается здесь", "Выберите агент, укажите каталог и настройте расписание резервирования.")}</div>`;
}
function audit() {
  return `<div class="tile divide-y divide-slate-100">${state.audit.length ? state.audit.map((a) => `<div class="flex flex-wrap items-center justify-between gap-2 py-4"><div><p class="text-sm font-semibold">${esc(a.action)}</p><p class="mt-1 text-xs text-slate-400">${esc(a.actor)} · ${esc(a.object)}</p></div><div class="text-right"><p class="text-xs ${a.result === "denied" ? "text-rose-500" : "text-emerald-600"}">${esc(a.result)}</p><p class="mt-1 text-xs text-slate-400">${date(a.at)}</p></div></div>`).join("") : empty("Записей аудита пока нет", "Действия управления и результаты операций будут появляться здесь.")}</div>`;
}
function render() {
  const titles = {
    overview: [
      "Обзор системы",
      "Копии, операции и состояние инфраструктуры в одном месте.",
    ],
    activity: [
      "Активность",
      "Текущие операции, история и предстоящие запуски.",
    ],
    jobs: [
      "Задания резервирования",
      "Источники, расписания и политика хранения.",
    ],
    storage: ["Хранилища", "Размещение резервных копий и использование диска."],
    nodes: [
      "Компоненты",
      "Состояние подключенных сервисов и защищенных соединений.",
    ],
    audit: ["Журнал аудита", "История действий пользователей и системы."],
  };
  $("#page-title").textContent = titles[state.page][0];
  $("#page-description").textContent = titles[state.page][1];
  $("#breadcrumb").textContent = $(
    `[data-page="${state.page}"]`,
  ).textContent.trim();
  document
    .querySelectorAll("[data-page]")
    .forEach((el) =>
      el.classList.toggle("active", el.dataset.page === state.page),
    );
  $("#content").innerHTML =
    state.page === "overview"
      ? stats() +
        resourceOverview() +
        `<div class="mb-7 flex flex-wrap items-center justify-between gap-3 rounded-2xl border border-brand-100 bg-brand-50/60 px-5 py-4"><p class="text-xs leading-5 text-brand-700"><strong>GData SRK · v0.8</strong>　Репликация между медиасерверами, PostgreSQL PITR, SRK Rescue и ВМ OpenStack/Tionix, oVirt, Proxmox и OpenNebula.</p><span class="text-xs text-brand-500">Лимит: ${state.status.max_parallel ?? "—"} операций</span></div>` +
        activity()
      : state.page === "activity"
        ? activity()
        : state.page === "jobs"
          ? jobs()
          : state.page === "storage"
            ? storage()
            : state.page === "nodes"
              ? nodes()
              : audit();
}
const renderPage = render;
render = function () {
  renderPage();
  if (
    ["overview", "activity"].includes(state.page) &&
    state.tab !== "upcoming"
  ) {
    const footer = document.createElement("div");
    footer.className = "mt-6 flex items-center justify-between gap-4";
    footer.innerHTML = `<span class="text-xs text-slate-400">${state.runs.length ? `Операции ${state.offset + 1}–${state.offset + state.runs.length}.` : "Нет операций."} Отчет выгружает весь выбранный период.</span><div class="flex gap-2"><button class="btn-secondary" data-offset="-200" ${state.offset === 0 ? "disabled" : ""}>← Назад</button><button class="btn-secondary" data-offset="200" ${state.runs.length < 200 ? "disabled" : ""}>Далее →</button></div>`;
    $("#content").append(footer);
  }
};
const mobileNav = document.createElement("select");
mobileNav.className = "field max-w-52 lg:hidden";
mobileNav.setAttribute("aria-label", "Раздел системы");
document.querySelectorAll("[data-page]").forEach((b) => {
  const o = document.createElement("option");
  o.value = b.dataset.page;
  o.textContent = b.textContent.trim();
  mobileNav.append(o);
});
mobileNav.onchange = () => {
  state.page = mobileNav.value;
  state.offset = 0;
  render();
  refresh();
};
document.querySelector("header").prepend(mobileNav);
let refreshing = false;
async function refresh() {
  if (refreshing || document.hidden) return;
  refreshing = true;
  try {
    const me=await api("/v1/me");
    const accessKey=JSON.stringify(me);if(state.accessKey!==accessKey){clearAccessView();state.accessKey=accessKey;}
    if(window.applyAccess) window.applyAccess(me);
    const [status, jobs, runs, nodes, audit] = await Promise.all(
      [
        "/v1/status",
        "/v1/jobs",
        "/v1/runs?" + queryParameters().toString(),
        "/v1/nodes",
        "/v1/audit",
      ].map((url) => api(url)),
    );
    if(window.applyCatalogRecovery)window.applyCatalogRecovery(status);
    if(status.restricted&&state.page==="storage")state.storageView=await api("/v1/storage-view");
    else state.storageView=null;
    Object.assign(state, {
      status,
      jobs,
      runs,
      nodes,
      audit,
      last: new Date(),
      logged: true,
    });
    $("#login").hidden = true;
    $("#connection").textContent =
      `● Обновлено ${new Intl.DateTimeFormat("ru-RU", { hour: "2-digit", minute: "2-digit", second: "2-digit" }).format(state.last)}`;
    if (
      !["search", "date-from", "date-to", "status-filter"].includes(
        document.activeElement?.id,
      )
    )
      render();
  } catch (err) {
    $("#connection").textContent = state.last
      ? `! Нет связи · данные на ${date(state.last)}`
      : "! Нет подключения";
    if (state.logged) notice(err.message);
  } finally {
    refreshing = false;
  }
}
document.addEventListener("click", async (ev) => {
  const b = ev.target.closest("button");
  if (!b) return;
  try {
    if (b.dataset.offset) {
      state.offset = Math.max(0, state.offset + Number(b.dataset.offset));
      await refresh();
      return;
    }
    if (b.dataset.close) {
      $(`#${b.dataset.close}`).close();
      return;
    }
    if (b.dataset.page) {
      state.page = b.dataset.page;
      state.offset = 0;
      notice("");
      render();
      refresh();
      return;
    }
    if (b.dataset.tab) {
      state.tab = b.dataset.tab;
      state.offset = 0;
      render();
      refresh();
      return;
    }
    if (b.dataset.run) {
      b.disabled = true;
      await post(`/v1/jobs/${b.dataset.run}/run`);
      notice("Задание принято в очередь.");
      await refresh();
      return;
    }
    if (b.dataset.toggle) {
      const j = state.jobs.find((j) => j.id === b.dataset.toggle);
      await post(`/v1/jobs/${j.id}/enabled`, { enabled: !j.enabled });
      await refresh();
      return;
    }
    if (b.dataset.cancel) {
      await post(`/v1/runs/${b.dataset.cancel}/cancel`);
      $("#detail-dialog").close();
      await refresh();
      return;
    }
    if (b.dataset.verify) {
      await post(`/v1/runs/${b.dataset.verify}/verify`);
      $("#detail-dialog").close();
      notice("Проверка поставлена в очередь.");
      await refresh();
      return;
    }
    if (b.dataset.restore) {
      const source =
        state.runs.find((r) => r.id === b.dataset.restore) ||
        (await api(`/v1/runs/${b.dataset.restore}`));
      if(window.srkUser?.role!=="admin"){await approvedRestore(source);return;}
      if (
        ["openstack", "ovirt", "proxmox", "opennebula"].includes(
          source.job.type,
        )
      ) {
        await openVMRestore(source.id);
        return;
      }
      if (["kubernetes", "longhorn"].includes(source.result?.data?.kind) && !b.dataset.exportKubernetes) {
        await openKubernetesRestore(source);
        return;
      }
      $("#restore-form").dataset.run = b.dataset.restore;
      const data = source.result?.data;
      $("#restore-description").textContent =
        data?.kind === "s3"
          ? "Назначение: s3://TARGET_PROFILE/new-prefix/. Префикс обязателен. Существующие объекты не перезаписываются. Восстанавливаются текущие объекты, метаданные и теги; история версий и политики бакета не переносятся."
          : data?.kind === "mysql"
            ? "Назначение: mysql://TARGET_PROFILE/new_database. Целевой профиль должен разрешать восстановление и совпадать с семейством СУБД. Создается новая база; существующие базы не заменяются. При ошибке частичная новая база сохраняется для диагностики."
            : ["kubernetes", "longhorn"].includes(data?.kind) ? "Экспорт JSON-объектов и индекса Longhorn в новый каталог на агенте. Применение объектов в кластер не выполняется; для восстановления PVC нужен сохранённый backupstore Longhorn."
            : "Данные будут записаны в новый каталог на исходном агенте. Каталог не должен существовать и должен входить в restore_roots.";
      $("#destination").placeholder = data && !["kubernetes", "longhorn"].includes(data.kind)
        ? data.kind === "s3"
          ? "s3://restore/new-prefix/"
          : "mysql://restore/new_database"
        : "/srv/restores/recovery-001";
      $("#destination").value = "";
      const backup = state.runs.find((r) => r.id === b.dataset.restore)?.result
        ?.postgresql;
      $("#pitr-options").hidden = !backup?.archive_id;
      $("#pitr-enabled").checked = false;
      $("#pitr-target").disabled = true;
      $("#pitr-target").required = false;
      $("#pitr-target").value = "";
      $("#pitr-inclusive").disabled = true;
      $("#pitr-inclusive").checked = true;
      $("#detail-dialog").close();
      $("#restore-error").textContent = "";
      $("#restore-dialog").showModal();
      return;
    }
    if (b.dataset.detail) {
      const r =
        state.runs.find((r) => r.id === b.dataset.detail) ||
        (await api(`/v1/runs/${encodeURIComponent(b.dataset.detail)}`));
      if (r.kind === "restore" && r.destination?.startsWith("kubernetes://")) { await openKubernetesOperation(r.id); return; }
      if (r.tape) {tapeMedia=r.job.media_id;tapeOperation=r.id;if(!tapeDialog.open)tapeDialog.showModal();await showTapeOperation(r.id);return;}
      if (r.wal_replication) {
        await openWALReplicationDetail(r.id);
        return;
      }
      if (r.replication) {
        await openReplicationDetail(r.id);
        return;
      }
      delete $("#detail-dialog").dataset.walReplicationRun;
      delete $("#detail-dialog").dataset.replicationRun;
      if (
        ["openstack", "ovirt", "proxmox", "opennebula"].includes(r.job.type)
      ) {
        await openVMDetail(r.id);
        return;
      }
      delete $("#detail-dialog").dataset.vmRun;
      if (r.capture) {
        $("#detail-dialog").dataset.captureRun = r.id;
        $("#detail-content").innerHTML =
          `<div class="flex items-center justify-between gap-3"><div id="capture-status">${badge(r.status)}</div><span class="text-xs font-semibold text-brand-500">SRK Rescue · физический диск</span></div><h3 class="mb-3 mt-5 text-lg font-semibold">${esc(r.job.name)}</h3><p class="mb-5 break-all text-xs leading-5 text-slate-500">${esc(r.capture.disk.host)} · ${esc(r.capture.disk.device)}<br>${esc(r.capture.disk.model)} · ${esc(r.capture.disk.serial)}</p><div id="capture-live"></div><dl class="my-5 grid grid-cols-2 gap-3 text-xs"><div><dt class="text-slate-400">Размер и сектор</dt><dd class="mt-1 font-medium">${size(r.capture.disk.bytes)} · ${r.capture.disk.sector_size} байт</dd></div><div><dt class="text-slate-400">Медиасервер</dt><dd class="mt-1 font-medium">${esc(r.job.media_id)}</dd></div><div><dt class="text-slate-400">Начало</dt><dd class="mt-1">${date(r.created_at)}</dd></div><div><dt class="text-slate-400">Хранение</dt><dd class="mt-1">${r.job.retention_days} дней</dd></div></dl><p class="mb-5 text-xs leading-5 text-slate-500">RAW-копия всего диска: разделы, загрузочные области и все адресуемые секторы.</p><h4 class="mb-3 font-semibold">Журнал операции</h4><div id="capture-events" class="max-h-64 space-y-3 overflow-y-auto text-xs">Загрузка событий…</div><div id="capture-actions" class="mt-6 flex flex-wrap gap-2"></div><p class="mt-5 break-all font-mono text-[10px] text-slate-400">ID ${esc(r.id)}</p>`;
        $("#detail-dialog").showModal();
        return;
      }
      $("#detail-content").innerHTML =
        `${badge(r.status)}<h3 class="mb-4 mt-5 font-semibold">${esc(r.job.name)}</h3><dl class="space-y-3 break-all text-sm">${[
          ["ID", r.id],
          ["Операция", kindText[r.kind]],
          ["Источник", r.job.source],
          ["Медиасервер", r.job.media_id],
          ["Хранение копии до", date(r.snapshot_expires_at)],
          ["Создано", date(r.created_at)],
          ["Завершено", date(r.finished_at)],
          ["Согласованность", r.result?.consistency || "Не подтверждена"],
          ["Цель PITR", r.pitr?.target_time || "—"],
          [
            "PITR подтвержден",
            r.result?.pitr?.recovered
              ? "Да · кластер остановлен"
              : r.pitr
                ? "Нет"
                : "—",
          ],
          ["Архив WAL", r.result?.postgresql?.archive_id || "—"],
          [
            "PostgreSQL",
            r.result?.postgresql
              ? `Версия ${r.result.postgresql.major} · ${r.result.postgresql.profile}`
              : "—",
          ],
          [
            "Проверка PostgreSQL",
            r.result?.postgresql?.native_verified
              ? "Пройдена: файлы и WAL"
              : "—",
          ],
          [
            "Диапазоны WAL",
            r.result?.postgresql?.wal_ranges
              ?.map((w) => `${w.Timeline}: ${w["Start-LSN"]} → ${w["End-LSN"]}`)
              .join("; ") || "—",
          ],
          [
            "Проверка хешей",
            r.result?.verified ? "Пройдена" : "Не подтверждена",
          ],
          ["Прочитано", size(r.result?.logical_bytes)],
          ["Новых данных", size(r.result?.uploaded_bytes)],
          ["Ошибка", r.error || "—"],
          ["Состояние копии", r.snapshot_state || "—"],
          ["Удалено копий", r.maintenance?.snapshots_removed ?? "—"],
          ["Удалено блоков", r.maintenance?.chunks_removed ?? "—"],
          [
            "Удалено данных",
            r.maintenance ? size(r.maintenance.bytes_removed) : "—",
          ],
          ["Импортировано", r.maintenance?.imported ?? "—"],
        ]
          .map(
            ([k, v]) =>
              `<div class="grid grid-cols-3 gap-3"><dt class="text-slate-400">${k}</dt><dd class="col-span-2">${esc(v)}</dd></div>`,
          )
          .join(
            "",
          )}</dl>${kubernetesPanel(r.result)}<div class="mt-6 flex flex-wrap gap-2">${r.status === "success" && r.kind === "backup" && r.snapshot_state !== "deleted" ? `${r.capture || r.result?.disk ? "" : `<button class="btn" data-restore="${r.id}">Восстановить</button><button class="btn-secondary" data-approved-restore="${r.id}">На целевую площадку</button>`}<button class="btn-secondary" data-verify="${r.id}">Проверить</button><button class="btn-secondary" data-replicate="${r.id}">Реплицировать</button>${r.result?.replica && r.result?.postgresql?.archive_id ? `<button class="btn-secondary" data-configure-wal-replication="${esc(r.id)}">Непрерывная репликация WAL</button>` : ""}${r.result?.data || r.result?.vm ? "" : `<button class="btn-secondary" data-rescue-code="${r.id}">Аварийный диск · код</button>`}` : ""}${["queued", "running"].includes(r.status) && !(r.options && r.status === "running") ? `<button class="btn-secondary" data-cancel="${r.id}">Отменить операцию</button>` : ""}</div>`;
      delete $("#detail-dialog").dataset.captureRun;
      $("#detail-dialog").showModal();
    }
  } catch (err) {
    notice(err.message);
  } finally {
    b.disabled = false;
  }
});
let searchTimer;
document.addEventListener("input", (ev) => {
  if (ev.target.id === "search") {
    state.search = ev.target.value;
    state.offset = 0;
    const cursor = ev.target.selectionStart;
    render();
    $("#search").focus();
    $("#search").setSelectionRange(cursor, cursor);
    clearTimeout(searchTimer);
    searchTimer = setTimeout(refresh, 300);
  }
});
document.addEventListener("focusout", (ev) => {
  if (ev.target.id === "search") setTimeout(refresh, 0);
});
document.addEventListener("change", (ev) => {
  if (ev.target.id === "status-filter") state.filter = ev.target.value;
  else if (ev.target.id === "date-from") state.from = ev.target.value;
  else if (ev.target.id === "date-to") state.to = ev.target.value;
  else return;
  state.offset = 0;
  render();
  refresh();
});
$("#new-job").onclick = () => {
  $("#agent-select").innerHTML = state.nodes
    .filter((n) => n.role === "agent")
    .map((n) => `<option value="${esc(n.id)}">${esc(n.id)}</option>`)
    .join("");
  $("#media-select").innerHTML = state.nodes
    .filter((n) => n.role === "media")
    .map((n) => `<option value="${esc(n.id)}">${esc(n.id)}</option>`)
    .join("");
  $("#job-error").textContent = "";
  updateJobType();
  $("#job-dialog").showModal();
};
$("#job-form").onsubmit = async (ev) => {
  ev.preventDefault();
  const body = Object.fromEntries(new FormData(ev.target));
  body.interval_seconds = Number(body.interval_seconds);
  body.retention_days = Number(body.retention_days);
  body.enabled = body.interval_seconds > 0;
  try {
    await post("/v1/jobs", body);
    $("#job-dialog").close();
    state.page = "jobs";
    await refresh();
  } catch (err) {
    $("#job-error").textContent = err.message;
  }
};
function updateJobType() {
  const type = $("#job-type").value,
    pg = type === "postgresql",
    image = type === "disk-image",
    data = ["s3", "mysql", "kubernetes", "longhorn"].includes(type);
  const node = state.nodes.find((n) => n.id === $("#agent-select").value);
  const profiles = pg
    ? node?.status?.postgresql_profiles || []
    : node?.status?.data_profiles?.[type] || [];
  $("#postgresql-profiles").innerHTML = profiles
    .map((p) => `<option value="${type}://${esc(p)}"></option>`)
    .join("");
  $("#job-description").textContent = pg
    ? "Горячая физическая копия PostgreSQL с WAL и проверкой."
    : ["kubernetes", "longhorn"].includes(type)
      ? "Объекты выбранных namespaces сохраняются в СРК. PVC копируются только при настроенном Longhorn CSI backup-классе; данные томов остаются во внешнем backupstore Longhorn. Без него сохраняются только объекты. Единой транзакции между ресурсами и томами нет."
    : type === "s3"
      ? "Текущие объекты MinIO/Ceph с пользовательскими метаданными и тегами. История версий и настройки бакета не копируются. Для согласованного состояния бакета исключите запись на время копирования."
      : type === "mysql"
        ? "Полный логический дамп одной базы MySQL/MariaDB: InnoDB, процедуры, события и триггеры. На время дампа исключите DDL. Binlog и PITR пока не поддерживаются."
        : image
          ? "Каталог с одним подготовленным офлайн RAW-файлом .img."
          : "Копирование файлов через агент без замораживания приложения.";
  $("#source-label").textContent =
    pg || data ? "Профиль источника на агенте" : "Путь на агенте";
  $("#job-source").placeholder =
    pg || data ? `${type}://production` : "/srv/data";
  $("#source-hint").textContent =
    pg || data
      ? profiles.length
        ? `Доступны: ${profiles.join(", ")}. Секреты хранятся на агенте.`
        : "У агента нет настроенных профилей этого типа."
      : "Должен входить в allowed_roots конфигурации агента.";
  const mode = $("#job-form select[name=mode]");
  if (pg || image || data) mode.value = "full";
  for (const option of mode.options)
    option.disabled = (pg || image || data) && option.value !== "full";
}
$("#job-type").onchange = () => {
  $("#job-source").value = "";
  updateJobType();
};
$("#agent-select").onchange = updateJobType;
$("#restore-form").onsubmit = async (ev) => {
  ev.preventDefault();
  try {
    const request = {
      destination: new FormData(ev.target).get("destination"),
    };
    if ($("#pitr-enabled").checked && !$("#pitr-options").hidden)
      request.pitr = {
        target_time: new Date($("#pitr-target").value).toISOString(),
        inclusive: $("#pitr-inclusive").checked,
      };
    await post(`/v1/runs/${ev.target.dataset.run}/restore`, request);
    $("#restore-dialog").close();
    state.page = "activity";
    state.tab = "now";
    await refresh();
  } catch (err) {
    $("#restore-error").textContent = err.message;
  }
};
$("#pitr-enabled").onchange = () => {
  const enabled = $("#pitr-enabled").checked;
  $("#pitr-target").disabled = !enabled;
  $("#pitr-target").required = enabled;
  $("#pitr-inclusive").disabled = !enabled;
};
$("#login-form").onsubmit = async (ev) => {
  ev.preventDefault();
  try {
    await post("/v1/login", { username: $("#username").value.trim(), password: $("#password").value });
    $("#password").value = "";
    $("#login-error").textContent = "";
    state.logged = true;
    await refresh();
  } catch (err) {
    $("#login-error").textContent = err.message;
  }
};
$("#logout").onclick = async () => {
  clearAccessView();
  try {
    await post("/v1/logout");
  } finally {
    state.logged = false;
    $("#login").hidden = false;
  }
};
function queryParameters() {
  const q = new URLSearchParams({
    limit: "200",
    offset: String(state.offset),
    scope: ["overview", "activity"].includes(state.page) ? state.tab : "all",
    status: state.filter,
    search: state.search,
  });
  if (state.from) q.set("from", new Date(state.from).toISOString());
  if (state.to) q.set("to", new Date(state.to).toISOString());
  return q;
}
function downloadReport(format) {
  const q = queryParameters();
  q.delete("limit");
  q.delete("offset");
  window.location.href = "/v1/reports/operations." + format + "?" + q;
}
$("#export").onclick = () => downloadReport("csv");
$("#export-xlsx").onclick = () => downloadReport("xlsx");
refresh();
setInterval(() => {
  if (state.logged) refresh();
}, 2000);
document.addEventListener("visibilitychange", () => {
  if (!document.hidden && state.logged) refresh();
});
