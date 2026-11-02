// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
"use strict";
const replicationDialog = document.createElement("dialog");
replicationDialog.id = "replication-dialog";
replicationDialog.className = "repository-dialog";
document.body.append(replicationDialog);
async function openReplication(id) {
  const source = await api(`/v1/runs/${encodeURIComponent(id)}`);
  const targets = state.nodes.filter(
    (n) =>
      n.role === "media" &&
      n.id !== source.job.media_id &&
      Date.now() - new Date(n.last_seen) < 30000,
  );
  $("#detail-dialog").close();
  replicationDialog.innerHTML = `<form class="p-8"><div class="mb-5 flex items-center justify-between"><h2 class="text-xl font-semibold">Репликация резервной копии</h2><button type="button" class="btn-secondary" data-close="replication-dialog">Закрыть</button></div><p class="mb-5 text-sm text-slate-500">${esc(source.job.name)}<br>Источник: ${esc(source.job.media_id)} · ${date(source.result?.replica?.captured_at || source.created_at)}</p><label class="label">Целевой медиасервер<select class="field mt-2" name="target" required>${targets.map((n) => `<option value="${esc(n.id)}">${esc(n.id)}</option>`).join("")}</select></label><label class="label mt-5 block">Независимый срок хранения, дней<input class="field mt-2" name="retention" type="number" min="1" max="36500" value="30" required></label><p class="my-5 text-sm leading-6 text-slate-500">Блоки передаются напрямую между медиасерверами по mTLS. Проверяются SHA-256 и полнота файлов; пригодная копия появится после завершения проверки. Срок хранения начинается после передачи. Хранение источника не изменится.</p>${source.result?.postgresql?.archive_id ? '<label class="my-5 flex items-center gap-3 rounded-xl bg-brand-50 p-4 text-sm"><input type="checkbox" name="include_wal" checked class="size-4 accent-brand-600">Непрерывно реплицировать архив WAL после базовой копии (каждые 30 с)</label>' : ""}${targets.length ? "" : '<p class="my-5 text-sm text-amber-700">Нет другого подключённого медиасервера. Зарегистрируйте его в этой инсталляции.</p>'}<p data-replication-error class="my-4 text-sm text-rose-600" role="alert"></p><button class="btn" type="submit" ${targets.length ? "" : "disabled"}>Начать репликацию</button></form>`;
  replicationDialog.showModal();
  const form = replicationDialog.querySelector("form");
  form.onsubmit = async (event) => {
    event.preventDefault();
    const button = form.querySelector('[type="submit"]');
    button.disabled = true;
    try {
      const run = await post(`/v1/runs/${encodeURIComponent(id)}/replicate`, {
        target_media_id: form.elements.target.value,
        retention_days: Number(form.elements.retention.value),
        include_wal: !!form.elements.include_wal?.checked,
      });
      replicationDialog.close();
      await refresh();
      await openReplicationDetail(run.id);
    } catch (error) {
      form.querySelector("[data-replication-error]").textContent =
        error.message;
      button.disabled = false;
    }
  };
}
let replicationDetailBusy = false;
async function openReplicationDetail(id) {
  const dialog = $("#detail-dialog");
  delete dialog.dataset.walReplicationRun;
  delete dialog.dataset.vmRun;
  delete dialog.dataset.captureRun;
  dialog.dataset.replicationRun = id;
  $("#detail-content").innerHTML =
    '<p class="text-sm text-slate-500">Загрузка репликации…</p>';
  if (!dialog.open) dialog.showModal();
  await refreshReplicationDetail();
}
async function refreshReplicationDetail() {
  const dialog = $("#detail-dialog"),
    id = dialog.dataset.replicationRun;
  if (!dialog.open || !id || replicationDetailBusy) return;
  replicationDetailBusy = true;
  try {
    const [run, events] = await Promise.all([
      api(`/v1/runs/${encodeURIComponent(id)}`),
      api(`/v1/runs/${encodeURIComponent(id)}/events`),
    ]);
    if (!dialog.open || dialog.dataset.replicationRun !== id) return;
    const p = run.replication,
      res = run.result,
      receipt = res?.replica;
    const phases = {
      "replication-manifest": "Проверка манифеста источника",
      "replication-transfer": "Передача и проверка блоков",
      "replication-validation": "Проверка файлов и публикация копии",
      complete: "Репликация завершена",
    };
    $("#detail-content").innerHTML =
      `${badge(run.status)}<h3 class="mb-4 mt-5 text-lg font-semibold">Репликация · ${esc(run.job.name)}</h3><div class="my-5 rounded-2xl bg-brand-50 p-4 text-sm"><p class="break-all font-semibold">${esc(p.source_media_id)} → ${esc(p.target_media_id)}</p><p class="mt-2 text-brand-700">Независимое хранение: ${p.retention_days} дней</p><p class="mt-2 break-all text-xs text-slate-500">Исходная точка: ${esc(p.source_snapshot_id)}</p></div>${operationProgress(run, res)}<p class="my-4 text-sm text-brand-700">${esc(phases[res?.phase] || (run.status === "queued" ? "Ожидает свободных потоков медиасерверов" : ""))}</p>${dedupPanel(res, "replicate")}${run.error ? `<p class="my-4 break-all text-sm text-rose-600" role="alert">${esc(run.error)}</p>` : ""}${receipt && run.status === "success" ? `<div class="my-5 rounded-2xl bg-emerald-50 p-4 text-sm text-emerald-700"><p class="font-semibold">Самостоятельная копия проверена</p><p class="mt-2">Хранится до ${date(receipt.expires_at)}</p><p class="mt-2 break-all text-xs">${esc(receipt.target_snapshot_id)}</p><p class="mt-2 text-xs">Изменение или удаление источника не меняет эту копию.</p></div>` : ""}<h4 class="my-4 font-semibold">Журнал репликации</h4><div class="max-h-64 space-y-2 overflow-y-auto text-xs">${events.map((e) => `<div class="rounded-xl bg-slate-50 p-3"><p class="font-semibold">${esc(e.action)}</p><p class="mt-1 break-all text-slate-600">${esc(e.result)}</p><p class="mt-1 text-slate-400">${date(e.at)}</p></div>`).join("") || "Событий пока нет"}</div><div class="mt-6 flex flex-wrap gap-2">${["queued", "running"].includes(run.status) ? `<button class="btn-secondary" data-cancel="${esc(id)}">Отменить репликацию</button>` : ""}${receipt && run.status === "success" ? `<button class="btn" data-detail="${esc(receipt.target_run_id)}">Открыть копию на приёмнике</button>` : ""}${["error", "cancelled"].includes(run.status) ? `<button class="btn-secondary" data-reconcile-replication="${esc(id)}">Проверить завершённую копию на приёмнике</button><button class="btn-secondary" data-replicate="${esc(p.source_run_id)}">Повторить передачу</button>` : ""}</div><p class="mt-5 break-all font-mono text-[10px] text-slate-400">ID ${esc(id)}</p>`;
  } catch (error) {
    notice(error.message);
  } finally {
    replicationDetailBusy = false;
  }
}
document.addEventListener("click", async (event) => {
  const button = event.target.closest("button");
  if (!button) return;
  try {
    if (button.dataset.replicate) {
      await openReplication(button.dataset.replicate);
    }
    if (button.dataset.reconcileReplication) {
      button.disabled = true;
      await post(
        `/v1/runs/${encodeURIComponent(button.dataset.reconcileReplication)}/reconcile-replication`,
      );
      await refresh();
      await openReplicationDetail(button.dataset.reconcileReplication);
    }
  } catch (error) {
    notice(error.message);
    button.disabled = false;
  }
});
$("#detail-dialog").addEventListener("close", () => {
  delete $("#detail-dialog").dataset.replicationRun;
});
setInterval(refreshReplicationDetail, 2000);
