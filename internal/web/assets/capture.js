// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
const captureDialog = document.createElement("dialog");
captureDialog.id = "capture-dialog";
captureDialog.className = "repository-dialog";
document.body.append(captureDialog);
captureDialog.addEventListener("close", () => captureDialog.replaceChildren());
document.querySelector("#new-capture").onclick = () => {
  const media = state.nodes.filter(
    (n) =>
      n.role === "media" &&
      n.status?.rescue_capture_enabled &&
      Date.now() - new Date(n.last_seen) < 30000,
  );
  captureDialog.innerHTML = `<form id="capture-code-form" class="p-8"><div class="flex items-center justify-between gap-4"><h2 class="text-xl font-semibold">Копия физического диска</h2><button type="button" class="btn-secondary" data-close="capture-dialog">×</button></div><p class="my-5 text-sm leading-6 text-slate-500">Загрузите компьютер с ISO SRK Rescue и подключитесь по отдельному коду записи. Медиасервер получит RAW-копию напрямую, с дедупликацией. Копируются все секторы, включая таблицу разделов и загрузчик. Исходный диск должен быть отключён от работающей ОС и не изменяться другим узлом.</p><div class="grid gap-4 md:grid-cols-2"><label class="label">Название<input class="field mt-2 w-full" name="name" required maxlength="200" placeholder="АРМ бухгалтерии · системный диск"></label><label class="label">Медиасервер<select class="field mt-2 w-full" name="media_id" required>${media.map((n) => `<option value="${esc(n.id)}">${esc(n.id)}</option>`).join("")}</select></label><label class="label">Хранение, дней<input class="field mt-2 w-full" name="retention_days" type="number" min="1" max="36500" value="30" required></label><label class="label">Максимум, GiB<input class="field mt-2 w-full" name="max_gib" type="number" min="1" max="4096" value="512" required></label></div>${!media.length ? '<p class="my-4 text-sm text-amber-700">Нет готового медиасервера. Включите rescue_capture_enabled в его конфигурации и проверьте связь с менеджером.</p>' : ""}<p class="my-5 text-sm leading-6 text-slate-500">Код действует однократно 10 минут; сессия записи — до 12 часов. Разрешена одна копия на выбранный медиасервер в пределах заданного размера. Права восстановления этот код не даёт.</p><button class="btn" ${media.length ? "" : "disabled"}>Выдать код записи</button><p id="capture-code-error" class="mt-4 text-sm text-rose-600" role="alert"></p></form>`;
  captureDialog.showModal();
  captureDialog.querySelector("form").onsubmit = async (event) => {
    event.preventDefault();
    const button = event.target.querySelector(
      'button[type="submit"],button.btn',
    );
    button.disabled = true;
    try {
      const fields = Object.fromEntries(new FormData(event.target));
      const issued = await post("/v1/rescue/codes", {
        operation: "backup",
        name: fields.name,
        media_id: fields.media_id,
        retention_days: Number(fields.retention_days),
        max_bytes: Number(fields.max_gib) * 1073741824,
      });
      captureDialog.innerHTML = `<div class="p-8"><h2 class="text-xl font-semibold">Код создания копии диска</h2><p class="my-4 text-sm leading-6 text-slate-500">В SRK Rescue выберите Connect using one-time code, затем Back up a physical disk to media server. Выберите диск и подтвердите BACKUP. Прогресс появится в разделе «Активность» после выбора диска.</p><label class="label" for="capture-code-text">Конфиденциальный одноразовый код</label><textarea id="capture-code-text" readonly class="field my-3 h-32 w-full font-mono text-xs"></textarea><p id="capture-code-until" class="my-3 break-all text-xs text-slate-500"></p><div class="flex flex-wrap gap-3"><button class="btn" id="capture-copy">Скопировать</button><button class="btn-secondary" id="capture-revoke">Отозвать доступ</button><button class="btn-secondary" data-close="capture-dialog">Закрыть</button></div><p id="capture-message" class="mt-4 text-sm" role="status"></p></div>`;
      captureDialog.querySelector("textarea").value = issued.code;
      captureDialog.querySelector("#capture-code-until").textContent =
        `Код до ${date(issued.code_expires_at)} · сессия до ${date(issued.session_expires_at)} · ID ${issued.id}`;
      captureDialog.querySelector("#capture-copy").onclick = async () => {
        try {
          await navigator.clipboard.writeText(
            captureDialog.querySelector("textarea").value,
          );
          captureDialog.querySelector("#capture-message").textContent =
            "Код скопирован";
        } catch {
          captureDialog.querySelector("textarea").select();
        }
      };
      captureDialog.querySelector("#capture-revoke").onclick = async () => {
        try {
          await api(`/v1/rescue/codes/${encodeURIComponent(issued.id)}`, {
            method: "DELETE",
          });
          captureDialog.querySelector("textarea").value = "";
          captureDialog.querySelector("#capture-copy").disabled = true;
          captureDialog.querySelector("#capture-message").textContent =
            "Доступ отозван";
        } catch (e) {
          captureDialog.querySelector("#capture-message").textContent =
            e.message;
        }
      };
    } catch (e) {
      captureDialog.querySelector("#capture-code-error").textContent =
        e.message;
      button.disabled = false;
    }
  };
};
let captureDetailBusy = false;
async function refreshCaptureDetail() {
  const dialog = document.querySelector("#detail-dialog"),
    id = dialog.dataset.captureRun;
  if (!dialog.open || !id || captureDetailBusy) return;
  captureDetailBusy = true;
  try {
    const [run, events] = await Promise.all([
      api(`/v1/runs/${encodeURIComponent(id)}`),
      api(`/v1/runs/${encodeURIComponent(id)}/events`),
    ]);
    if (!dialog.open || dialog.dataset.captureRun !== id) return;
    document.querySelector("#capture-status").innerHTML = badge(run.status);
    document.querySelector("#capture-actions").innerHTML =
      run.status === "success" && run.result?.verified
        ? `<button class="btn" data-rescue-code="${esc(id)}">Восстановить · код SRK Rescue</button><button class="btn-secondary" data-verify="${esc(id)}">Проверить копию</button><button class="btn-secondary" data-replicate="${esc(id)}">Реплицировать</button>`
        : ["queued", "running"].includes(run.status)
          ? `<button class="btn-secondary" data-cancel="${esc(id)}">Отменить операцию</button>`
          : "";
    document.querySelector("#capture-live").innerHTML =
      operationProgress(run, run.result) +
      `<div class="my-4 grid grid-cols-2 gap-3 rounded-xl bg-slate-50 p-3 text-xs"><div class="text-slate-500">Подтверждено данных<p class="mt-1 text-base font-semibold text-slate-800">${size(run.result?.logical_bytes)}</p></div><div class="text-slate-500">Передано по сети<p class="mt-1 text-base font-semibold text-slate-800">${size(run.result?.uploaded_bytes)}</p></div></div>` +
      dedupPanel(run.result, run.kind) +
      `<p class="my-3 text-sm">${run.status === "success" && run.result?.verified ? "Полная копия проверена медиасервером" : esc({ running: "Выполняется", queued: "В очереди", success: "Успешно", error: "Ошибка", cancelled: "Отменено" }[run.status] || run.status)}${run.error ? ` · ${esc(run.error)}` : ""}</p>`;
    const labels = {
      "capture.enqueue": "Поставлено в очередь",
      "capture.started": "Медиасервер готов",
      "capture.progress": "Прогресс медиасервера",
      "backup.finished": "Копирование завершено",
      "capture.abort": "Клиент запросил остановку",
      "run.cancel": "Администратор запросил отмену",
    };
    document.querySelector("#capture-events").innerHTML =
      events
        .map(
          (e) =>
            `<div class="rounded-xl bg-slate-50 p-3"><p class="font-semibold">${esc(labels[e.action] || e.action)} · ${esc(e.action === "capture.progress" ? e.result.replace("uploading", "Передача блоков").replace("verifying", "Проверка всех блоков").replace("complete", "Проверка пройдена") : { success: "Успешно", error: "Ошибка", cancelled: "Отменено", "media ticket ready": "Приём диска разрешён", requested: "Запрошено" }[e.result] || e.result)}</p><p class="mt-1 text-slate-500">${date(e.at)} · ${esc(e.actor)}</p></div>`,
        )
        .join("") || "Событий пока нет";
  } catch (e) {
    notice(e.message);
  } finally {
    captureDetailBusy = false;
  }
}
document.addEventListener("click", (event) => {
  if (event.target.closest("[data-detail]"))
    setTimeout(refreshCaptureDetail, 0);
});
setInterval(refreshCaptureDetail, 2000);
