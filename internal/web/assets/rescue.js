// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
const rescueDialog = document.createElement("dialog");
rescueDialog.className = "repository-dialog";
rescueDialog.id = "rescue-dialog";
document.body.append(rescueDialog);
let rescueScopeID = "";
rescueDialog.addEventListener("close", () => { rescueDialog.replaceChildren(); });
document.addEventListener("click", async (event) => {
  const button = event.target.closest("[data-rescue-code]");
  if (!button) return;
  button.disabled = true;
  try {
    const response = await post("/v1/rescue/codes", {run_ids: [button.dataset.rescueCode]});
    rescueScopeID = response.id;
    rescueDialog.innerHTML = `<div class="p-8"><h2 class="text-xl font-semibold">Аварийное восстановление</h2><p class="my-4 text-sm leading-6 text-slate-500">Загрузите компьютер с SRK Rescue ISO, настройте сеть и выберите Connect using one-time code. Код даёт доступ только к этой копии и срабатывает один раз в течение 10 минут. Сессия действует 4 часа. Передавайте код конфиденциально.</p><label class="label" for="rescue-code-text">Код подключения</label><textarea readonly id="rescue-code-text" class="field mt-2 h-32 w-full font-mono text-xs"></textarea><p id="rescue-code-expiry" class="my-3 break-all text-xs text-slate-500"></p><p class="my-4 text-sm leading-6 text-slate-500">Полная запись на диск доступна для копий RAW-образа типа «Образ диска». Для обычной файловой копии этот режим недоступен. Запись требует подтверждения в аварийном меню.</p><div class="flex flex-wrap gap-3"><button id="rescue-copy" class="btn">Скопировать код</button><button id="rescue-revoke" class="btn-secondary">Отозвать доступ</button><button data-close="rescue-dialog" class="btn-secondary">Закрыть</button></div><p id="rescue-message" class="mt-4 text-sm" role="status"></p></div>`;
    rescueDialog.querySelector("textarea").value = response.code;
    rescueDialog.querySelector("#rescue-code-expiry").textContent = `Код до ${date(response.code_expires_at)} · Сессия до ${date(response.session_expires_at)} · ID: ${response.id}`;
    rescueDialog.querySelector("#rescue-copy").onclick = async () => {
      try { await navigator.clipboard.writeText(rescueDialog.querySelector("textarea").value); rescueDialog.querySelector("#rescue-message").textContent = "Код скопирован"; }
      catch { rescueDialog.querySelector("textarea").select(); }
    };
    rescueDialog.querySelector("#rescue-revoke").onclick = async () => {
      try { await api(`/v1/rescue/codes/${encodeURIComponent(rescueScopeID)}`, {method:"DELETE"}); rescueDialog.querySelector("textarea").value = ""; rescueDialog.querySelector("#rescue-copy").disabled = true; rescueDialog.querySelector("#rescue-message").textContent = "Доступ отозван"; }
      catch (error) {rescueDialog.querySelector("#rescue-message").textContent = error.message;}
    };
    document.querySelector("#detail-dialog").close();
    rescueDialog.showModal();
  } catch (error) { alert(error.message); }
  finally {button.disabled = false;}
});
