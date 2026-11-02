// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
"use strict";
const walPolicyDialog = document.createElement("dialog");
walPolicyDialog.id = "wal-policy-dialog";
walPolicyDialog.className = "repository-dialog";
document.body.append(walPolicyDialog);
function walLagCard(lag) {
  const states = {pending: "Ожидает доставки", caught_up: "Архив доставлен", released: "Защита снята", source_unavailable: "Источник недоступен"};
  const age = lag?.pending_age_seconds;
  return `<div class="my-4 rounded-xl bg-brand-50 p-4 text-sm"><p class="font-semibold">${esc(states[lag?.state] || "Состояние неизвестно")}</p><p class="mt-2">Не подтверждено: ${lag?.pending_files == null ? "неизвестно" : `${lag.pending_files} файлов · ${size(lag.pending_bytes)}`}</p><p class="mt-2">Ожидание старейшего файла: ${age == null ? "неизвестно" : `${Math.floor(age / 3600)} ч ${Math.floor(age % 3600 / 60)} мин ${age % 60} с`}</p><p class="mt-2">${lag?.state === "source_unavailable" ? "Защита не проверена" : lag?.source_protected ? "Очистка неподтверждённых WAL запрещена" : "Защита источника не активна"} · приёмник ${lag?.target_online ? "в сети" : "не в сети"}</p><p class="mt-2 text-xs">Последнее подтверждение: ${lag?.last_acknowledged_at ? date(lag.last_acknowledged_at) : "—"} · контакт: ${lag?.last_contact_at ? date(lag.last_contact_at) : "—"}</p><p class="mt-2 text-xs text-slate-500">Замер: ${lag?.observed_at ? date(lag.observed_at) : "—"}. Возраст архивного файла не равен RPO PostgreSQL.</p></div>`;
}
async function showWALReplicationPolicies(refreshOnly = false) {
  if (refreshOnly && !walPolicyDialog.open) return;
  const rows = await api("/v1/wal-replication-policies");
  if (refreshOnly && !walPolicyDialog.open) return;
  walPolicyDialog.innerHTML = `<div class="p-8"><div class="mb-5 flex items-center justify-between"><h2 class="text-xl font-semibold">Непрерывная репликация WAL</h2><button class="btn-secondary" data-close="wal-policy-dialog">Закрыть</button></div><p class="mb-5 text-sm leading-6 text-slate-500">Регулярная синхронизация закрытых WAL и history-файлов. Перезапуск управления сохраняет политики. Пауза сохраняет защиту неподтверждённых WAL на источнике. Фактические окна и пропуски PITR смотрите в календаре приёмника.</p><div class="space-y-5">${rows.map(({ policy: p, last_run: r, lag }) => `<article class="rounded-2xl border border-slate-100 p-5"><p class="font-semibold">${esc(p.source_media_id)} → ${esc(p.target_media_id)}</p><p class="mt-2 break-all text-xs text-slate-500">Архив ${esc(p.archive_id)}</p><p class="my-3 text-sm">${p.enabled ? `Включено · каждые ${p.interval_seconds} с` : "Приостановлено"} · следующий опрос ${p.enabled ? date(p.next_run) : "—"}</p>${walLagCard(lag)}${r ? `${badge(r.status)}<p class="mt-2 text-xs">Последний запуск: ${date(r.created_at)}${r.result?.wal_replication ? ` · новых ${r.result.wal_replication.copied}, существующих ${r.result.wal_replication.reused}` : ""}</p>${r.error ? `<p class="my-3 break-all text-sm text-rose-600">${esc(r.error)}</p>` : ""}` : '<p class="text-xs text-slate-500">Ожидает первого запуска</p>'}<div class="mt-5 flex flex-wrap gap-2"><button class="btn-secondary" data-wal-policy-toggle="${esc(p.id)}" data-enabled="${!p.enabled}">${p.enabled ? "Приостановить" : "Включить"}</button>${p.enabled ? `<button class="btn-secondary" data-wal-policy-sync="${esc(p.id)}">Синхронизировать сейчас</button>` : ""}<button class="btn-secondary" data-wal-policy-calendar="${esc(p.id)}">Календарь PITR приёмника</button>${r ? `<button class="btn" data-wal-operation="${esc(r.id)}">Прогресс и журнал</button>` : ""}</div></article>`).join("") || '<p class="text-sm text-slate-500">Политик пока нет. Реплицируйте PostgreSQL с опцией WAL либо включите доставку в подробностях существующей удалённой базовой копии.</p>'}</div></div>`;
  walPolicyDialog._policies = rows.map((row) => row.policy);
  if (!walPolicyDialog.open) walPolicyDialog.showModal();
}
$("#wal-replication-policies").onclick = () =>
  showWALReplicationPolicies().catch((error) => notice(error.message));
async function openWALReplicationDetail(id) {
  const d = $("#detail-dialog");
  delete d.dataset.replicationRun;
  delete d.dataset.vmRun;
  delete d.dataset.captureRun;
  d.dataset.walReplicationRun = id;
  $("#detail-content").innerHTML =
    '<p class="text-sm">Загрузка синхронизации WAL…</p>';
  if (!d.open) d.showModal();
  await refreshWALReplicationDetail();
}
let walReplicationBusy = false;
async function refreshWALReplicationDetail() {
  const dialog = $("#detail-dialog"),
    id = dialog.dataset.walReplicationRun;
  if (!dialog.open || !id || walReplicationBusy) return;
  walReplicationBusy = true;
  try {
    const [run, events] = await Promise.all([
      api(`/v1/runs/${encodeURIComponent(id)}`),
      api(`/v1/runs/${encodeURIComponent(id)}/events`),
    ]);
    if (!dialog.open || dialog.dataset.walReplicationRun !== id) return;
    const p = run.wal_replication,
      res = run.result,
      wal = res?.wal_replication;
    const phases = {
      "wal-replication-inventory": "Получение списка WAL",
      "wal-replication-transfer": "Передача и проверка сегментов",
      complete: "Снимок доступного архива синхронизирован",
    };
    $("#detail-content").innerHTML =
      `${badge(run.status)}<h3 class="my-5 text-lg font-semibold">Репликация непрерывного архива WAL</h3><div class="my-5 rounded-2xl bg-brand-50 p-4 text-sm"><p class="font-semibold">${esc(p.source_media_id)} → ${esc(p.target_media_id)}</p><p class="mt-2 break-all text-xs">${esc(p.archive_id)}</p><p class="mt-2 text-xs">Политика: каждые ${p.interval_seconds} с. Передаются только закрытые файлы.</p></div>${operationProgress(run, res)}<p class="my-4 text-sm text-brand-700">${esc(phases[res?.phase] || "Ожидает запуска")}</p>${wal ? `<p class="my-4 text-sm">Новых: ${wal.copied} · проверено существующих: ${wal.reused} · в выбранном наборе: ${wal.segments}</p>` : ""}${run.error ? `<p class="my-4 break-all text-sm text-rose-600" role="alert">${esc(run.error)}</p>` : ""}${wal?.calendar ? `<div class="my-5 space-y-3">${wal.calendar.windows.map((w) => `<div class="rounded-xl ${w.status === "candidate" ? "bg-emerald-50 text-emerald-700" : "bg-amber-50 text-amber-700"} p-4 text-xs"><p class="font-semibold">${esc(walStatusText[w.status] || w.status)}</p><p class="mt-2 break-all">${esc(w.from)} ≤ цель &lt; ${esc(w.to_exclusive || "не подтверждено")}</p>${w.gaps.map((g) => `<p class="mt-2 text-rose-700">Пропуск: ${esc(g.from)} — ${esc(g.to)} · ${g.count} сегм.</p>`).join("")}<p class="mt-2">Точный результат PITR подтверждается запуском PostgreSQL.</p></div>`).join("")}</div>` : ""}<h4 class="my-3 font-semibold">Журнал WAL-репликации</h4><div class="max-h-64 space-y-2 overflow-y-auto text-xs">${events.map((e) => `<div class="rounded-xl bg-slate-50 p-3"><p class="font-semibold">${esc(e.action)}</p><p class="mt-1 break-all text-slate-600">${esc(e.result)}</p><p class="mt-1 text-slate-400">${date(e.at)}</p></div>`).join("") || "Событий пока нет"}</div><div class="mt-6 flex flex-wrap gap-2"><button class="btn-secondary" data-wal-detail-calendar="${esc(p.target_media_id)}" data-archive="${esc(p.archive_id)}">Актуальный календарь PITR</button>${["queued", "running"].includes(run.status) ? `<button class="btn-secondary" data-cancel="${esc(id)}">Отменить этот запуск</button>` : ""}<button class="btn" data-show-wal-policies>Политики WAL</button></div><p class="mt-5 break-all font-mono text-[10px] text-slate-400">ID ${esc(id)}</p>`;
  } catch (error) {
    notice(error.message);
  } finally {
    walReplicationBusy = false;
  }
}
document.addEventListener("click", async (event) => {
  const b = event.target.closest("button");
  if (!b) return;
  try {
    if (b.dataset.configureWalReplication) {
      await post(
        `/v1/runs/${encodeURIComponent(b.dataset.configureWalReplication)}/wal-replication`,
        { interval_seconds: 30, enabled: true },
      );
      $("#detail-dialog").close();
      await showWALReplicationPolicies();
    }
    if (b.hasAttribute("data-show-wal-policies")) {
      $("#detail-dialog").close();
      await showWALReplicationPolicies();
    }
    if (b.dataset.walPolicyToggle) {
      await post(
        `/v1/wal-replication-policies/${encodeURIComponent(b.dataset.walPolicyToggle)}/enabled`,
        { enabled: b.dataset.enabled === "true" },
      );
      await showWALReplicationPolicies();
    }
    if (b.dataset.walPolicySync) {
      await post(
        `/v1/wal-replication-policies/${encodeURIComponent(b.dataset.walPolicySync)}/run`,
      );
      notice("Синхронизация WAL запрошена.");
      await showWALReplicationPolicies();
    }
    if (b.dataset.walOperation) {
      walPolicyDialog.close();
      await openWALReplicationDetail(b.dataset.walOperation);
    }
    if (b.dataset.walPolicyCalendar) {
      const p = walPolicyDialog._policies.find(
        (p) => p.id === b.dataset.walPolicyCalendar,
      );
      walPolicyDialog.close();
      await showPITRCalendar(p.target_media_id, p.archive_id);
    }
    if (b.dataset.walDetailCalendar) {
      $("#detail-dialog").close();
      await showPITRCalendar(b.dataset.walDetailCalendar, b.dataset.archive);
    }
  } catch (error) {
    notice(error.message);
  }
});
$("#detail-dialog").addEventListener(
  "close",
  () => delete $("#detail-dialog").dataset.walReplicationRun,
);
setInterval(refreshWALReplicationDetail, 2000);

setInterval(() => {
  if (walPolicyDialog.open)
    showWALReplicationPolicies(true).catch((error) => notice(error.message));
}, 2000);
