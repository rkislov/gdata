// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
"use strict";
const repositoryUI = { media: "", points: [], plan: null, sequence: 0 };
const repositoryDialog = document.createElement("dialog");
repositoryDialog.id = "repository-dialog";
repositoryDialog.className = "repository-dialog";
document.body.append(repositoryDialog);
const lifecycleDialog = document.createElement("dialog");
lifecycleDialog.id = "lifecycle-dialog";
document.body.append(lifecycleDialog);
function closeLifecycle() {
  lifecycleDialog.close();
  repositoryDialog.close();
}
async function openRepository(media) {
  const sequence = ++repositoryUI.sequence;
  repositoryUI.media = media;
  repositoryDialog.innerHTML = `<div class="p-8"><div class="flex items-center justify-between"><h2 class="text-xl font-semibold">Копии в репозитории</h2><button data-close="repository-dialog" class="btn-secondary" aria-label="Закрыть">×</button></div><p class="mt-4 text-sm text-slate-500">Проверяем метаданные хранилища…</p></div>`;
  if (!repositoryDialog.open) repositoryDialog.showModal();
  try {
    const data = await api(`/v1/media/${encodeURIComponent(media)}/snapshots`);
    if (sequence !== repositoryUI.sequence) return;
    repositoryUI.points = data.snapshots;
    repositoryDialog.innerHTML = `<div class="p-8"><div class="mb-3 flex items-center justify-between gap-4"><div><h2 class="text-xl font-semibold">Копии в репозитории</h2><p class="mt-2 break-all text-xs text-slate-400">${esc(media)} · ${esc(data.repository.id)}</p></div><button data-close="repository-dialog" class="btn-secondary" aria-label="Закрыть">×</button></div><div class="mb-5 flex gap-3"><button class="btn-secondary" data-repository="${esc(media)}">Обновить</button><button class="btn-secondary" data-gc-preview="${esc(media)}">Предварительная очистка</button></div><p class="mb-5 text-xs leading-5 text-slate-500">Удержанные копии защищены от очистки. Импорт проверяет содержимое, продлевает хранение и устанавливает защиту до ее явного снятия. Задания импорта не запускаются автоматически.</p><div class="grid gap-4 md:grid-cols-2">${
      data.snapshots.length
        ? data.snapshots
            .map((s) => {
              const expired = new Date(s.expires_at) <= new Date();
              return `<article class="rounded-2xl border border-slate-200 p-5"><div class="mb-3 flex items-center justify-between gap-2"><span class="badge ${s.hold ? "bg-brand-50 text-brand-700" : expired ? "bg-amber-50 text-amber-700" : "bg-emerald-50 text-emerald-700"}">${s.hold ? "Защищена" : expired ? "Срок истек" : "Доступна"}</span><span class="text-xs text-slate-400">${size(s.result.logical_bytes)}</span></div><p class="break-all font-mono text-xs text-slate-600">${esc(s.id)}</p><p class="mt-2 break-all text-xs leading-5 text-slate-400">${esc(s.source)}</p><p class="mt-3 text-xs leading-5 text-slate-500">Создана: ${date(s.created_at)}<br>Хранение до: ${date(s.expires_at)}</p>${s.hold && s.hold_reason ? `<p class="mt-2 text-xs text-brand-600">${esc(s.hold_reason)}</p>` : ""}<div class="mt-4 flex flex-wrap gap-2"><button class="btn-secondary text-xs" data-policy="${esc(s.id)}">Хранение и защита</button><button class="btn-secondary text-xs" data-import-snapshot="${esc(s.id)}">Импорт в каталог</button></div></article>`;
            })
            .join("")
        : empty(
            "Нет точек восстановления",
            "Создайте первую копию или подключите репозиторий собственного формата СРК.",
          )
    }</div></div>`;
  } catch (e) {
    repositoryDialog.innerHTML = `<div class="p-8"><h2 class="text-xl font-semibold">Не удалось прочитать репозиторий</h2><p class="mt-4 break-all text-sm text-rose-600">${esc(e.message)}</p><button data-close="repository-dialog" class="btn-secondary mt-6">Закрыть</button></div>`;
  }
}
async function showGC(media) {
  const sequence = ++repositoryUI.sequence;
  repositoryUI.media = media;
  notice("Проверяем содержимое и рассчитываем безопасную очистку…");
  try {
    const p = await api(`/v1/media/${encodeURIComponent(media)}/gc`);
    if (sequence !== repositoryUI.sequence) return;
    repositoryUI.plan = p;
    notice("");
    lifecycleDialog.innerHTML = `<div class="p-8"><h2 class="text-xl font-semibold">Предварительная очистка</h2><p class="mt-3 text-sm leading-6 text-slate-500">Будут удалены просроченные точки и блоки, которые не нужны оставшимся копиям. План проверяется повторно при применении.</p><div class="my-6 grid grid-cols-3 gap-4"><div><p class="label">Копий</p><p class="text-2xl font-semibold">${p.snapshots.length}</p></div><div><p class="label">Блоков</p><p class="text-2xl font-semibold">${p.chunks.length}</p></div><div><p class="label">Объем</p><p class="text-2xl font-semibold">${size(p.bytes)}</p></div></div>${p.active_writers ? `<p class="mb-5 rounded-xl bg-amber-50 p-4 text-sm text-amber-700">Активных потоков записи: ${p.active_writers}. Удаление блоков отложено; базы этих потоков защищены.</p>` : ""}<p class="text-xs leading-5 text-slate-400">Читающих операций: ${p.active_readers}. План действует 15 минут. Изменение репозитория потребует нового расчета.</p><div class="mt-7 flex justify-end gap-3"><button class="btn-secondary" data-close="lifecycle-dialog">Отмена</button><button class="btn" data-apply-gc="true">Подтвердить очистку</button></div></div>`;
    lifecycleDialog.showModal();
  } catch (e) {
    notice(e.message);
  }
}
function showImport(id) {
  const s = repositoryUI.points.find((p) => p.id === id);
  if (!s) return;
  lifecycleDialog.innerHTML = `<form id="repository-import-form" class="p-8" data-snapshot="${esc(id)}"><h2 class="text-xl font-semibold">Импорт копии</h2><p class="mt-3 break-all text-sm leading-6 text-slate-500">${esc(s.source)}<br>${esc(s.id)}</p><label class="label mt-6">Агент для восстановления</label><select class="field" name="agent" required>${state.nodes
    .filter((n) => ["agent","proxy"].includes(n.role))
    .map((n) => `<option value="${esc(n.id)}">${esc(n.id)}</option>`)
    .join(
      "",
    )}</select><label class="label mt-5">Минимальный новый срок в днях</label><input class="field" name="days" type="number" min="1" max="36500" value="30" required><p class="mt-5 text-xs leading-5 text-slate-400">Содержимое будет проверено. Копия получит защиту от удаления; ее необходимо снять отдельно после проверки восстановления. Существующие записи каталога не дублируются.</p><p class="mt-3 text-sm text-rose-600" data-lifecycle-error role="alert"></p><div class="mt-7 flex justify-end gap-3"><button type="button" class="btn-secondary" data-close="lifecycle-dialog">Отмена</button><button class="btn" type="submit">Проверить и импортировать</button></div></form>`;
  lifecycleDialog.showModal();
}
function showPolicy(id) {
  const s = repositoryUI.points.find((p) => p.id === id);
  if (!s) return;
  const t = new Date(s.expires_at);
  const local = new Date(t.getTime() - t.getTimezoneOffset() * 60000)
    .toISOString()
    .slice(0, 19);
  lifecycleDialog.innerHTML = `<form id="repository-policy-form" class="p-8" data-snapshot="${esc(id)}"><h2 class="text-xl font-semibold">Хранение и защита</h2><p class="mt-3 break-all font-mono text-xs text-slate-400">${esc(id)}</p><label class="label mt-6">Дата окончания хранения</label><input class="field" name="expires" type="datetime-local" step="1" value="${local}" required><label class="mt-5 flex items-center gap-3 text-sm"><input name="hold" type="checkbox" class="size-4 accent-brand-600" ${s.hold ? "checked" : ""}> Защитить от автоматического удаления</label><label class="label mt-5">Причина удержания</label><input class="field" name="reason" value="${esc(s.hold_reason || "Ручное удержание")}"><p class="mt-5 text-xs leading-5 text-amber-700">После снятия защиты копия с истекшим сроком может быть удалена следующей очисткой. Изменение применяется только к выбранной копии.</p><p class="mt-3 text-sm text-rose-600" data-lifecycle-error role="alert"></p><div class="mt-7 flex justify-end gap-3"><button type="button" class="btn-secondary" data-close="lifecycle-dialog">Отмена</button><button class="btn" type="submit">Подтвердить изменение</button></div></form>`;
  lifecycleDialog.showModal();
}
async function queueRepository(kind, body) {
  const run = await post(
    `/v1/media/${encodeURIComponent(repositoryUI.media)}/${kind}`,
    body,
  );
  closeLifecycle();
  state.page = "activity";
  state.tab = "now";
  state.offset = 0;
  notice(`Операция принята: ${run.id}`);
  await refresh();
}
document.addEventListener("click", async (ev) => {
  const b = ev.target.closest("button");
  if (!b) return;
  try {
    if (b.dataset.repository) {
      await openRepository(b.dataset.repository);
    } else if (b.dataset.gcPreview) {
      await showGC(b.dataset.gcPreview);
    } else if (b.dataset.applyGc) {
      b.disabled = true;
      await queueRepository("gc", { plan: repositoryUI.plan });
    } else if (b.dataset.importSnapshot) {
      showImport(b.dataset.importSnapshot);
    } else if (b.dataset.policy) {
      showPolicy(b.dataset.policy);
    }
  } catch (e) {
    notice(e.message);
  } finally {
    b.disabled = false;
  }
});
document.addEventListener("submit", async (ev) => {
  if (
    !["repository-import-form", "repository-policy-form"].includes(ev.target.id)
  )
    return;
  ev.preventDefault();
  const form = ev.target;
  const b = form.querySelector("button[type=submit]");
  b.disabled = true;
  try {
    const data = new FormData(form);
    if (form.id === "repository-import-form") {
      await queueRepository("import", {
        snapshot_ids: [form.dataset.snapshot],
        agent_id: data.get("agent"),
        retention_days: Number(data.get("days")),
      });
    } else {
      await queueRepository("policy", {
        snapshot_id: form.dataset.snapshot,
        policy: {
          expires_at:
            form.elements.expires.value === form.elements.expires.defaultValue
              ? repositoryUI.points.find((p) => p.id === form.dataset.snapshot)
                  .expires_at
              : new Date(data.get("expires")).toISOString(),
          hold: data.get("hold") === "on",
          reason: data.get("reason"),
        },
      });
    }
  } catch (e) {
    form.querySelector("[data-lifecycle-error]").textContent = e.message;
  } finally {
    b.disabled = false;
  }
});
