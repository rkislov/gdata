// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
"use strict";
const kubernetesDialog = document.createElement("dialog");
kubernetesDialog.id = "kubernetes-dialog";
kubernetesDialog.className = "repository-dialog";
document.body.append(kubernetesDialog);
let kubernetesTimer;
kubernetesDialog.addEventListener("close", () => clearTimeout(kubernetesTimer));
async function openKubernetesRestore(source) {
  clearTimeout(kubernetesTimer);
  const node = state.nodes.find(n => n.id === source.job.agent_id);
  const profiles = node?.status?.kubernetes_restore_profiles || [];
  const options = values => values.map(v => `<option value="${esc(v)}">${esc(v)}</option>`).join("");
  $("#detail-dialog").close();
  kubernetesDialog.innerHTML = `<form class="p-8"><div class="flex items-center justify-between gap-4"><h2 class="text-xl font-semibold">Восстановить Kubernetes</h2><button type="button" class="btn-secondary" data-close="kubernetes-dialog">Закрыть</button></div><p class="my-5 text-sm">Создание нового namespace и новых томов Longhorn. Исходный backupstore должен быть доступен целевому кластеру.</p><div class="grid gap-5"><label class="label">Целевой профиль<select class="field mt-2" name="profile" required>${options(profiles)}</select></label><label class="label">Исходный namespace<select class="field mt-2" name="source" required>${options(source.result.data.kubernetes.namespaces)}</select></label><label class="label">Новый namespace<input class="field mt-2" name="namespace" required maxlength="63" pattern="[a-z][a-z0-9-]*[a-z0-9]" placeholder="restore-demo"></label><label class="flex items-center gap-3 text-sm"><input type="checkbox" name="start">Запустить приложения после восстановления томов</label></div><p class="my-5 text-xs text-slate-500">По умолчанию Deployment и StatefulSet имеют 0 реплик, CronJob приостановлены, DaemonSet не планируются. При ошибке ресурсы сохраняются для диагностики. Существующий namespace использовать нельзя.</p>${profiles.length ? "" : '<p class="my-4 text-sm text-rose-600">На агенте нет профилей с allow_restore. Настройте профиль и дождитесь обновления статуса.</p>'}<p class="my-4 text-sm text-rose-600" role="alert"></p><div class="flex flex-wrap gap-3"><button class="btn" type="submit" ${profiles.length ? "" : "disabled"}>Восстановить в кластер</button><button type="button" class="btn-secondary" data-restore="${esc(source.id)}" data-export-kubernetes="true">Экспорт JSON</button></div></form>`;
  kubernetesDialog.showModal();
  kubernetesDialog.querySelector("[data-export-kubernetes]").addEventListener("click", () => kubernetesDialog.close());
  const form = kubernetesDialog.querySelector("form");
  form.onsubmit = async e => {
    e.preventDefault();
    const fields = form.elements, button = form.querySelector('[type="submit"]');
    button.disabled = true;
    try {
      const destination = `kubernetes://${fields.profile.value}/${fields.source.value}/${fields.namespace.value}?start=${fields.start.checked}`;
      const run = await post(`/v1/runs/${encodeURIComponent(source.id)}/restore`, {destination});
      await openKubernetesOperation(run.id);
    } catch (error) {
      form.querySelector('[role="alert"]').textContent = error.message;
      button.disabled = false;
    }
  };
}
async function openKubernetesOperation(id) {
  clearTimeout(kubernetesTimer);
  const run = await api(`/v1/runs/${encodeURIComponent(id)}`);
  $("#detail-dialog").close();
  const events = run.result?.kubernetes_restore?.events || [];
  kubernetesDialog.innerHTML = `<div class="p-8"><div class="flex items-center justify-between gap-4"><h2 class="text-xl font-semibold">Восстановление Kubernetes</h2><button class="btn-secondary" data-close="kubernetes-dialog">Закрыть</button></div><div class="my-5">${badge(run.status)}</div>${operationProgress(run, run.result)}${kubernetesPanel(run.result)}<p class="my-4 break-all text-xs">${esc(run.destination)}</p>${run.error ? `<p class="my-4 text-sm text-rose-600">${esc(run.error)}. Частичные ресурсы сохранены для диагностики.</p>` : ""}<h3 class="my-4 font-semibold">Журнал операции</h3><div class="max-h-64 space-y-2 overflow-y-auto text-xs">${events.map(ev => `<div class="rounded-xl bg-slate-50 p-3"><p>${esc(ev.phase)} · ${esc(ev.resource)}</p><p class="mt-1 text-slate-400">${date(ev.at)}</p></div>`).join("") || "Ожидание событий агента…"}</div><p class="mt-5 text-xs text-slate-500">Показаны последние 200 событий. Полный журнал хранится в staging_dir/restore-journals на агенте. ${run.kubernetes_check ? "Проверены готовность Pod и настроенные HTTP-запросы; остальные функции приложения требуют отдельной проверки." : "Готовность проверяется через API; проверка приложения выполняется отдельно."}</p></div>`;
  if (!kubernetesDialog.open) kubernetesDialog.showModal();
  if (["queued", "running"].includes(run.status)) kubernetesTimer = setTimeout(() => {
    if (kubernetesDialog.open) openKubernetesOperation(id).catch(() => {});
  }, 2500);
}
