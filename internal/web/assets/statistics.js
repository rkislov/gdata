// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
"use strict";
const statisticDetails = new Map();
document.addEventListener(
  "toggle",
  (event) => {
    const key = event.target.dataset?.statDetail;
    if (key) statisticDetails.set(key, event.target.open);
  },
  true,
);

function percentText(value) {
  return Number.isFinite(value)
    ? `${Math.max(0, Math.min(100, value)).toFixed(1)}%`
    : "—";
}
function rateText(value) {
  return Number.isFinite(value) ? `${size(value)}/с` : "—";
}
function usageBar(label, value, description = "") {
  return `<div><div class="mb-2 flex items-center justify-between gap-3 text-xs"><span class="text-slate-500">${esc(label)}</span><span class="font-semibold text-slate-700">${percentText(value)}</span></div>${Number.isFinite(value) ? `<progress max="100" value="${Math.min(100, Math.max(0, value))}" class="h-2 w-full accent-brand-500" aria-label="${esc(label)}"></progress>` : `<p class="text-xs text-slate-400">Нет измерения</p>`}${description ? `<p class="mt-1 break-all text-xs text-slate-400">${esc(description)}</p>` : ""}</div>`;
}
function hostPanel(host, lastSeen, compact = false, key = "") {
  if (!host)
    return '<p class="mt-5 text-xs text-slate-400">Метрики хоста еще не получены.</p>';
  const detailKey = `${key || host.hostname}-io`;
  const stale =
    Date.now() - new Date(host.at) > 20000 ||
    Date.now() - new Date(lastSeen) > 30000;
  const memory = host.memory;
  const network = (host.network || [])
    .filter((n) => !/^lo[0-9]*$/.test(n.name))
    .sort(
      (a, b) =>
        (b.receive_bps || 0) +
        (b.send_bps || 0) -
        (a.receive_bps || 0) -
        (a.send_bps || 0),
    )[0];
  return `<div class="mt-5 space-y-4" data-host-metrics>${stale ? '<p class="rounded-xl bg-amber-50 p-3 text-xs text-amber-700">Измерения устарели — показан последний полученный образец.</p>' : ""}<p class="break-all text-xs text-slate-400">${esc(host.hostname)} · ${esc(host.os)}</p><div class="grid grid-cols-2 gap-5">${usageBar("CPU", host.cpu_percent)}${usageBar("RAM", memory?.percent, memory ? `${size(memory.used)} / ${size(memory.total)}` : "")}</div>${network ? `<div class="rounded-xl bg-slate-50 p-3"><p class="mb-2 text-xs text-slate-500">Сеть · ${esc(network.name)}</p><div class="flex flex-wrap justify-between gap-2 text-xs font-medium"><span>↓ ${rateText(network.receive_bps)}</span><span>↑ ${rateText(network.send_bps)}</span></div></div>` : ""}${compact ? "" : `<details data-stat-detail="${esc(detailKey)}" ${statisticDetails.get(detailKey) ? "open" : ""} class="rounded-xl border border-slate-100 p-3"><summary class="cursor-pointer text-xs font-semibold text-brand-600">Диски и сетевые интерфейсы</summary><div class="mt-4 max-h-80 space-y-4 overflow-y-auto">${(host.volumes || []).map((v) => usageBar(`Том ${v.path}`, v.percent, `${size(v.used)} / ${size(v.total)} · свободно ${size(v.free)}`)).join("")}${(host.disks || []).map((d) => `<div class="border-t border-slate-100 pt-3 text-xs"><p class="font-medium">Диск ${esc(d.name)} · занят ${percentText(d.busy_percent)}</p><p class="mt-1 text-slate-500">Чтение ${rateText(d.read_bps)} · запись ${rateText(d.write_bps)}</p></div>`).join("")}${(host.network || []).map((n) => `<div class="border-t border-slate-100 pt-3 text-xs"><p class="font-medium">${esc(n.name)}</p><p class="mt-1 text-slate-500">↓ ${rateText(n.receive_bps)} · ↑ ${rateText(n.send_bps)}<br>Ошибки приема ${n.errors_in} · передачи ${n.errors_out}</p></div>`).join("")}</div></details>`}<p class="text-xs text-slate-400">Измерено ${date(host.at)}${host.unavailable?.length ? ` · Нет данных: ${esc(host.unavailable.join(", "))}` : ""}</p></div>`;
}
function componentNodes() {
  const manager =
    state.status?.role === "manager"
      ? [
          {
            id: state.status.id,
            role: "manager",
            url: location.origin,
            last_seen: state.status.time,
            status: state.status,
          },
        ]
      : [];
  return [...manager, ...state.nodes];
}
function resourceOverview() {
  return `<details data-stat-detail="resources-overview" class="tile mb-7" ${statisticDetails.get("resources-overview") !== false ? "open" : ""}><summary class="cursor-pointer text-sm font-semibold text-slate-600">Ресурсы компонентов</summary><p class="mt-2 text-xs text-slate-400">Показатели всей ОС. Сервисы на одном хосте показывают его общие ресурсы.</p><div class="mt-4 grid gap-5 md:grid-cols-2 xl:grid-cols-4">${componentNodes()
    .map(
      (n) =>
        `<section class="min-w-0 rounded-2xl border border-slate-100 p-4"><p class="truncate text-sm font-semibold">${esc(n.id)}</p>${hostPanel(n.status?.host, n.last_seen, true)}</section>`,
    )
    .join("")}</div></details>`;
}
function dedupPanel(result, kind = "backup") {
  if (
    !["backup", "replicate"].includes(kind) ||
    !result ||
    result.logical_bytes <= 0 ||
    result.phase === "postgresql-basebackup"
  )
    return "";
  const saved = Math.max(0, result.logical_bytes - result.uploaded_bytes);
  const ratio = (100 * saved) / result.logical_bytes;
  return `<div class="rounded-xl bg-emerald-50 p-3 text-xs" data-dedup><div class="flex flex-wrap justify-between gap-2"><span class="font-semibold text-emerald-700">Дедупликация · ${percentText(ratio)}</span><span class="text-emerald-600">Повторно использовано ${size(saved)}</span></div><p class="mt-1 leading-5 text-emerald-600">Доля прочитанных данных, которые уже были в репозитории.</p></div>`;
}
function operationProgress(run, live) {
  if (run.status !== "running") return "";
  const known = Number.isFinite(live?.progress_percent);
  const names = {
    "kubernetes-objects": "Сохранение объектов Kubernetes",
    "longhorn-backup": "Создание резервной копии Longhorn",
    "longhorn-backup-ready": "Подтверждение копий Longhorn",
    "tape-pool": "Ожидание кассеты или привода",
    "scsi-write": "Запись на ленту",
    "scsi-readback": "Контрольное чтение ленты",
    basebackup: "Получение базы и WAL",
    unpack: "Подготовка файлов базы",
    "wal-download": "Загрузка архива WAL",
    recovery: "Применение WAL до цели PITR",
    validation: "Проверка PostgreSQL",
    "data-transfer": "Передача данных",
    bytes: "Передача проверенных блоков",
    "wal-replication": "Проверка и передача архива WAL",
    operation: "Завершение",
  };
  const stale = (() => {
    const agent = state.nodes.find(
      (n) =>
        n.id ===
        (run.wal_replication?.target_media_id ||
          run.replication?.target_media_id ||
          run.job?.agent_id),
    );
    return agent && Date.now() - new Date(agent.last_seen) > 20000;
  })();
  return `<div class="rounded-xl bg-brand-50 p-3" data-operation-progress><div class="mb-2 flex justify-between gap-3 text-xs"><span class="text-brand-700">${esc(names[live?.progress_scope] || "Подготовка операции")}${stale ? " · данные устарели" : ""}</span><span class="font-semibold text-brand-700">${known ? percentText(live.progress_percent) : "Выполняется"}</span></div><progress max="100" ${known ? `value="${Math.min(100, Math.max(0, live.progress_percent))}"` : ""} class="h-2 w-full accent-brand-500" aria-label="Прогресс текущего этапа"></progress>${live?.total_bytes ? `<p class="mt-2 text-xs text-brand-600">${size(live.progress_bytes)} / ${run.capture || run.replication ? "" : "~"}${size(live.total_bytes)} · ${run.replication ? "уникальные блоки проверены на приёмнике" : run.capture ? "подтверждено медиасервером" : "оценка текущего этапа"}</p>` : ""}</div>`;
}
function repositoryDedup(status) {
  const saved =
    status.logical_bytes > 0 ? percentText(status.dedup_saved_percent) : "—";
  return `<div class="mb-5 rounded-2xl bg-emerald-50 p-4" data-repository-dedup><p class="text-xs font-semibold text-emerald-700">Экономия дедупликации · ${saved}</p><div class="mt-3 grid grid-cols-2 gap-4 text-xs text-emerald-700"><div>Логический объем<p class="mt-1 font-semibold">${size(status.logical_bytes)}</p></div><div>Уникальные блоки<p class="mt-1 font-semibold">${size(status.unique_chunk_bytes)}</p></div></div><p class="mt-3 text-xs leading-5 text-emerald-600">Повторно использовано ${size(status.dedup_saved_bytes)}. WAL: ${size(status.wal_bytes)} · ${status.wal_files || 0} файлов. Метаданные и WAL в расчет не входят; неочищенные блоки учитываются.</p></div>`;
}
function walArchivePanel(archives) {
  if (!archives || !Object.keys(archives).length) return "";
  return `<div class="mt-5 space-y-3">${Object.entries(archives)
    .map(
      ([name, s]) =>
        `<section class="rounded-xl ${s.error ? "bg-rose-50" : "bg-brand-50"} p-4" data-wal-archive><p class="text-xs font-semibold">WAL · ${esc(name)}</p><p class="mt-2 text-xs leading-5 text-slate-500">${s.error ? esc(s.error) : s.last_success && !s.last_success.startsWith("0001-") ? `Подтверждено ${date(s.last_success)}<br>${esc(s.name)}` : "Настроен прием от archive_command. Ожидается первый сегмент."}</p>${s.archive_id ? `<button class="mt-3 text-xs font-semibold text-brand-600" data-wal-list="${esc(s.archive_id)}" data-wal-media="${esc(s.media_id)}">Посмотреть архив →</button>` : ""}</section>`,
    )
    .join("")}</div>`;
}
const walDialog = document.createElement("dialog");
walDialog.id = "wal-dialog";
walDialog.className = "repository-dialog";
document.body.append(walDialog);
document.addEventListener("click", async (event) => {
  const button = event.target.closest("[data-wal-list]");
  if (!button) return;
  try {
    const records = await api(
      `/v1/media/${encodeURIComponent(button.dataset.walMedia)}/wal/${encodeURIComponent(button.dataset.walList)}`,
    );
    walDialog.innerHTML = `<div class="p-8"><div class="flex items-center justify-between"><h2 class="text-xl font-semibold">Архив PostgreSQL WAL</h2><button data-close="wal-dialog" class="btn-secondary" aria-label="Закрыть">×</button></div><p class="mt-4 break-all font-mono text-xs text-slate-400">${esc(button.dataset.walList)}</p><p class="my-5 text-xs leading-5 text-slate-500"><button class="btn-secondary mb-4" data-pitr-calendar="${esc(button.dataset.walList)}" data-media="${esc(button.dataset.walMedia)}">Календарь PITR и очистка WAL</button><br>Сохранено ${records.length} файлов. Время ниже — подтверждение архивации, а не граница восстановления. Пропуски и достижение цели проверяются при PITR. Показаны последние 100 записей.</p><div class="max-h-96 space-y-3 overflow-y-auto">${
      records
        .slice(-100)
        .reverse()
        .map(
          (r) =>
            `<article class="rounded-xl border border-slate-100 p-3"><p class="break-all font-mono text-xs">${esc(r.name)}</p><p class="mt-1 text-xs text-slate-400">${size(r.bytes)} · ${date(r.archived_at)} · PostgreSQL ${r.major}</p></article>`,
        )
        .join("") || '<p class="text-sm text-slate-400">Архив пуст.</p>'
    }</div></div>`;
    walDialog.showModal();
  } catch (e) {
    notice(e.message);
  }
});

function kubernetesPanel(result) {
 if(result?.kubernetes_check) return kubernetesCheckReport(result);
 const restored=result?.kubernetes_restore;
 if (restored) return `<div class="my-3 rounded-xl bg-brand-50 p-4 text-sm"><p class="font-semibold">Восстановление · ${esc(restored.namespace)}</p><p class="mt-2">Объекты: ${restored.objects}/${restored.total_objects} · Тома: ${restored.volumes}/${restored.total_volumes}</p><p class="mt-2">${esc(restored.state)} · ${esc(restored.current_resource || "")}</p><p class="mt-2 text-xs">${restored.workloads_started ? "Запуск приложений разрешён." : "Приложения подготовлены без автоматического запуска."}</p></div>`;
 const k=result?.data?.kubernetes;if (!k) return "";
 return `<div class="my-3 rounded-xl bg-brand-50 p-4 text-sm"><p class="font-semibold">Kubernetes · ${k.objects} объектов</p><p class="my-2">Namespaces: ${k.namespaces.map(esc).join(", ")}</p><p>${k.pvc_data === "longhorn-external-backupstore" ? `Longhorn: ${k.volume_backups} резервных копий PVC во внешнем backupstore. Для восстановления томов нужен этот backupstore.` : "Данные PVC не включены: сохранены только API-объекты."}</p><p class="mt-2 text-xs">${k.includes_secrets ? "Secrets включены, кроме токенов ServiceAccount." : "Secrets исключены."} Доступны экспорт JSON-архива и восстановление в новый namespace.</p></div>`;
}
