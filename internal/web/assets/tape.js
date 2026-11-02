// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
"use strict";
const tapeDialog = document.createElement("dialog");
tapeDialog.className = "repository-dialog";
tapeDialog.id = "tape-dialog";
document.body.append(tapeDialog);
const tapeActionText = {
  recover: "Сверить после сбоя",
  label: "Маркировка",
  load: "Загрузка в привод",
  unload: "Выгрузка в слот",
  import: "Импорт каталога кассеты",
  verify: "Проверка содержимого",
  write: "Запись резервной копии",
  restore: "Восстановление с кассеты",
  erase: "Стирание",
  recycle: "Подготовка к перезаписи",
  hold: "Удержание копии",
  "release-hold": "Снятие удержания",
};
let tapeMedia = "",
  tapeLibrary = null,
  tapeRefreshing = false,
  tapeOperation = "";
async function showTapeLibrary(media, refreshOnly = false) {
  if (refreshOnly && (!tapeDialog.open || tapeOperation)) return;
  if (tapeRefreshing) return;
  tapeRefreshing = true;
  try {
    const [lib, runs] = await Promise.all([api(`/v1/media/${encodeURIComponent(media)}/tape`), api(`/v1/media/${encodeURIComponent(media)}/tape/queue`)]);
    if (refreshOnly && (!tapeDialog.open || tapeOperation)) return;
    tapeMedia = media;
    tapeLibrary = lib;
    const historyOpen = refreshOnly && tapeDialog.querySelector("[data-tape-history]")?.open;
    const positionOpen = refreshOnly && tapeDialog.querySelector("[data-tape-positions]")?.open;
    const scrollTop = refreshOnly ? tapeDialog.scrollTop : 0;
    const supported = (a) => !lib.actions || lib.actions.includes(a);
    const positionLog = (lib.position_history || []).map((j) => `<div class="rounded-xl bg-slate-50 p-3 text-xs"><p>${esc(j.action)} · ${esc(j.phase)} · ${date(j.at)}</p><p>Кассета ${esc(j.barcode)} · привод ${esc(j.drive)} · файл ${j.file}</p><p>Элементы ${j.source_element} → ${j.destination_element}</p><p class="break-all">${esc(j.operation)} ${esc(j.error || "")}</p></div>`).join("");
    const activeIDs = new Set((lib.queue || []).filter(q => q.state === "running").map(q => q.id));
    const pending = (lib.positions || (lib.position ? [lib.position] : [])).filter(p => !activeIDs.has(p.operation) && !(lib.queue || []).some(q => q.state === "running" && q.drive === p.drive));
    const blocked = c => pending.some(p => p.barcode === c.barcode || (c.drive_id && p.drive === c.drive_id));
    const recovery = pending.map(p => `<div class="my-4 rounded-xl bg-rose-50 p-4 text-sm text-rose-600">Привод ${esc(p.drive)} · кассета ${esc(p.barcode)}: требуется сверка после сбоя.<button class="btn-secondary mt-3" data-tape-action="recover" data-barcode="${esc(p.barcode)}">Сверить после сбоя</button></div>`).join("");
    const activeRuns = runs.filter(r => ["queued", "running"].includes(r.status));
    const pastRuns = runs.filter(r => !["queued", "running"].includes(r.status)).reverse();
    const runCard = r => `<article class="rounded-xl border border-slate-100 p-4 text-sm"><div class="flex items-center justify-between gap-3"><p class="font-semibold">${esc(tapeActionText[r.tape.action] || r.tape.action)} · ${esc(r.result?.tape?.barcode || r.tape.barcode || ("Пул " + r.tape.pool_id))}</p>${badge(r.status)}</div><p class="my-2">Привод ${esc(r.result?.tape?.drive_id || r.tape.drive_id || "будет определён")} · ${date(r.created_at)}</p><p class="my-2 text-xs">${esc(r.result?.phase || "Ожидает свободного привода и лимита задач")}</p>${operationProgress(r, r.result)}${r.error ? `<p class="my-2 text-rose-600">${esc(r.error)}</p>` : ""}<button class="btn-secondary mt-3" data-tape-run="${esc(r.id)}">Прогресс и журнал</button>${r.status === "queued" ? `<button class="btn-secondary mt-3" data-cancel="${esc(r.id)}">Отменить</button>` : ""}</article>`;
    const poolsPanel = `<section class="my-5"><div class="flex items-center justify-between"><h3 class="font-semibold">Пулы кассет</h3><button class="btn-secondary" data-tape-pool-new>Создать пул</button></div><div class="my-3 space-y-3">${(lib.pools || []).map(p => `<article class="rounded-xl bg-brand-50 p-4 text-sm"><p class="font-semibold">${esc(p.name)} · ${esc(p.id)}</p><p class="my-2">Кассеты: ${p.barcodes.map(esc).join(", ")}</p><p>Приводы: ${p.drives.map(esc).join(", ")}</p><p class="my-2">Свободно на носителях: ${size(lib.cartridges.filter(c => p.barcodes.includes(c.barcode)).reduce((n,c) => n + Math.max(0,c.capacity_bytes-c.used_bytes),0))}. Архив должен помещаться на одну кассету.</p><button class="btn-secondary" data-tape-pool-edit="${esc(p.id)}">Настроить</button><button class="btn-secondary" data-tape-pool-write="${esc(p.id)}">Записать автоматически</button></article>`).join("") || "Пулы ещё не настроены"}</div></section>`;
    const queuePanel = `<section class="my-5"><h3 class="mb-3 font-semibold">Очередь и текущие операции · ${activeRuns.length}</h3><p class="mb-3 text-xs text-slate-500">Разные приводы работают параллельно. Задания одного привода выполняются по очереди с учётом общих лимитов.</p><div class="space-y-3">${activeRuns.map(runCard).join("") || '<p class="text-sm text-slate-500">Очередь пуста</p>'}</div><details class="my-4" data-tape-history ${historyOpen ? "open" : ""}><summary>История операций · последние 50</summary><div class="mt-3 space-y-3">${pastRuns.map(runCard).join("") || "Операций пока нет"}</div></details></section>`;

    tapeDialog.innerHTML = `<div class="p-8"><div class="flex items-center justify-between gap-4"><h2 class="text-xl font-semibold">Ленточная библиотека · ${esc(lib.library_id)}</h2><button data-close="tape-dialog" class="btn-secondary">Закрыть</button></div><p class="my-4 rounded-xl bg-amber-50 p-4 text-sm text-amber-700">${lib.backend === "scsi" ? "Linux SCSI · запись с контрольным чтением. Маркировка только пустой кассеты. Доступные действия определяет транспорт." : "Эмулятор библиотеки. Физические команды TS4300/LTO-9 не выполняются. Маркировка задаёт логическую метку, штрихкод кассеты сохраняется."}</p>${recovery}${poolsPanel}${queuePanel}${positionLog ? `<details class="my-4" data-tape-positions ${positionOpen ? "open" : ""}><summary>Журнал позиции ленты</summary><div class="mt-3 space-y-2">${positionLog}</div></details>` : ""}<div class="my-5 grid gap-3 md:grid-cols-2">${lib.drives.map((d) => `<div class="rounded-xl bg-brand-50 p-4 text-sm"><p class="font-semibold">${esc(d.id)} · LTO-${d.generation}</p><p class="mt-2">${d.online ? "В сети" : "Отключён"} · ${d.empty ? "Пуст" : "Кассета загружена"}</p><p class="mt-2">${activeRuns.filter(r => (r.result?.tape?.drive_id || r.tape.drive_id) === d.id).map(r => `${esc(tapeActionText[r.tape.action] || r.tape.action)} · ${r.status === "queued" || r.result?.phase === "tape-queued" ? "в очереди" : "выполняется"}`).join("<br>") || "Нет заданий"}</p></div>`).join("")}</div><div class="space-y-4">${lib.cartridges.map((c) => `<article class="rounded-2xl border border-slate-100 p-5"><div class="flex items-start justify-between gap-3"><div><h3 class="font-semibold">${esc(c.label || "Без метки")}</h3><p class="mt-2 text-xs text-slate-500">${esc(c.barcode)} · слот ${c.slot} · LTO-${c.generation}${c.drive_id ? ` · привод ${esc(c.drive_id)}` : ""}</p></div><span class="badge ${c.dirty ? "bg-rose-50 text-rose-600" : "bg-brand-50 text-brand-700"}">${c.busy ? "Выполняется операция" : c.dirty ? "Незавершённая запись" : c.known ? "Каталог известен" : "Нужен импорт / маркировка"}</span></div><p class="my-3 text-sm">Занято ${size(c.used_bytes)} / ${size(c.capacity_bytes)}${c.worm ? " · WORM" : ""}${c.write_protected ? " · Защита от записи" : ""}</p><progress class="h-2 w-full accent-brand-500" max="100" value="${Math.min(100, (c.used_bytes / c.capacity_bytes) * 100)}" aria-label="Заполнение кассеты"></progress><div class="my-4 flex flex-wrap gap-2">${(c.mounted ? ["unload", "label", "import", "verify", "write", "erase", "recycle"] : ["load", "label"]).filter(supported).map((a) => `<button class="btn-secondary" data-tape-action="${a}" data-barcode="${esc(c.barcode)}" ${blocked(c) ? "disabled" : ""}>${esc(tapeActionText[a])}</button>`).join("")}</div><div class="space-y-3">${(c.copies || []).map((p) => `<div class="rounded-xl bg-slate-50 p-3 text-xs"><p class="break-all font-mono">${esc(p.id)}</p><p class="my-2">${p.verified ? "Проверена" : "Запись не подтверждена"} · ${size(p.bytes || 0)} · хранение до ${date(p.expires_at)}${p.hold ? " · Удержание" : ""}</p><div class="flex flex-wrap gap-2"><button class="btn-secondary" data-tape-action="restore" data-barcode="${esc(c.barcode)}" data-copy="${esc(p.id)}" ${!c.mounted || blocked(c) || !p.verified ? "disabled" : ""}>Восстановить на диск</button><button class="btn-secondary" ${supported(p.hold ? "release-hold" : "hold") ? "" : "hidden"} data-tape-action="${p.hold ? "release-hold" : "hold"}" data-barcode="${esc(c.barcode)}" data-copy="${esc(p.id)}" ${blocked(c) ? "disabled" : ""}>${p.hold ? "Снять удержание" : "Удерживать"}</button></div></div>`).join("")}</div></article>`).join("")}</div><p class="mt-5 text-xs text-slate-400">${esc(media)} · замер ${date(lib.observed_at)}. Стирание разрешено только после окончания сроков всех копий и снятия удержания. Перезапись: подготовить кассету, затем записать новую копию.</p></div>`;
    if (!tapeDialog.open) tapeDialog.showModal();
    if (refreshOnly) tapeDialog.scrollTop = scrollTop;
  } finally {
    tapeRefreshing = false;
  }
}
async function tapeActionForm(action, barcode, copyID = "") {
  tapeOperation = "form";
  const c = tapeLibrary.cartridges.find((v) => v.barcode === barcode),
    destructive = ["erase", "recycle", "label", "release-hold"].includes(
      action,
    );
  const backups =
    action === "write"
      ? (await api("/v1/runs")).filter(
          (r) =>
            r.kind === "backup" &&
            r.status === "success" &&
            r.job.media_id === tapeMedia &&
            r.snapshot_state !== "deleted",
        )
      : [];
  const agents = state.nodes.filter((n) => ["agent", "proxy"].includes(n.role));
  tapeDialog.innerHTML = `<form class="p-8" id="tape-action-form"><h2 class="text-xl font-semibold">${esc(tapeActionText[action])} · ${esc(barcode)}</h2><p class="my-4 text-sm text-slate-500">${esc(c.label || "Без метки")} · ${c.mounted ? esc(c.drive_id) : "в слоте"}</p>${
    action === "load"
      ? `<label class="label block">Привод<select class="field mt-2 w-full" name="drive" required>${tapeLibrary.drives
          .filter((d) => d.online && d.empty)
          .map((d) => `<option value="${esc(d.id)}">${esc(d.id)}</option>`)
          .join("")}</select></label>`
      : ""
  }${action === "label" ? `<label class="label mt-4 block">Логическая метка<input class="field mt-2 w-full" name="label" value="${esc(c.label || "")}" maxlength="128" required></label>` : ""}${action === "write" ? `<label class="label block">Резервная копия<select class="field mt-2 w-full" name="source" required>${backups.map((r) => `<option value="${esc(r.id)}">${esc(r.job.name)} · ${date(r.created_at)}</option>`).join("")}</select></label>` : ""}${["write", "restore"].includes(action) ? `<label class="label mt-4 block">Срок хранения, дней<input class="field mt-2 w-full" type="number" name="days" min="1" max="36500" value="30" required></label>` : ""}${action === "restore" ? `<label class="label mt-4 block">Агент восстановления / прокси ВМ<select class="field mt-2 w-full" name="agent" required>${agents.map((n) => `<option value="${esc(n.id)}">${esc(n.id)} · ${esc(n.role)}</option>`).join("")}</select></label><p class="my-4 text-sm text-slate-500">Копия будет возвращена в дисковый репозиторий и зарегистрирована с удержанием. Затем выберите её в истории для восстановления файлов, базы или ВМ.</p>` : ""}${action === "erase" ? `<label class="label mt-4 block">Режим<select class="field mt-2 w-full" name="mode"><option value="quick">${tapeLibrary.backend === "scsi" ? "Быстрое — записать конец данных (EOD)" : "Быстрое — удалить файлы эмулятора"}</option><option value="long">${tapeLibrary.backend === "scsi" ? "Длительное — команда накопителя ERASE LONG" : "Полное — перезаписать файлы нулями и удалить"}</option></select></label>` : ""}${["erase", "recycle"].includes(action) ? '<p class="my-4 rounded-xl bg-rose-50 p-4 text-sm text-rose-700">Данные кассеты будут удалены. Действующие сроки хранения, удержание, WORM, защита от записи и неизвестное содержимое блокируют операцию. Быстрое стирание и подготовка к перезаписи не гарантируют физическое уничтожение прежних данных.</p>' : ""}${destructive ? `<label class="label mt-4 block">Причина<input class="field mt-2 w-full" name="reason" maxlength="500" required></label><label class="label mt-4 block">Введите штрихкод ${esc(barcode)} для подтверждения<input class="field mt-2 w-full" name="confirmation" autocomplete="off" required></label>` : ""}<p class="my-4 text-sm text-rose-600" role="alert" data-tape-error></p><div class="mt-6 flex gap-3"><button type="button" class="btn-secondary" data-tape-back>Назад</button><button type="submit" class="btn">${esc(tapeActionText[action])}</button></div></form>`;
  const form = tapeDialog.querySelector("form");
  form.onsubmit = async (e) => {
    e.preventDefault();
    const el = form.elements;
    const b = form.querySelector('[type="submit"]');
    b.disabled = true;
    try {
      const run = await post(
        `/v1/media/${encodeURIComponent(tapeMedia)}/tape/operations`,
        {
          action,
          barcode,
          copy_id: copyID,
          drive_id: el.drive?.value || (action === "recover" ? (tapeLibrary.positions || [tapeLibrary.position]).find(p => p?.barcode === barcode)?.drive : c.drive_id) || "",
          label: el.label?.value || "",
          confirm: el.confirmation?.value || "",
          reason: el.reason?.value || "",
          erase_mode: el.mode?.value || "quick",
          snapshot_id: el.source?.value || "",
          agent_id: el.agent?.value || "",
          retention_days: Number(el.days?.value || 30),
        },
      );
      await showTapeOperation(run.id);
    } catch (error) {
      form.querySelector("[data-tape-error]").textContent = error.message;
      b.disabled = false;
    }
  };
}
async function showTapeOperation(id) {
  tapeOperation = id;
  await refreshTapeOperation();
}
async function refreshTapeOperation() {
  if (
    !tapeDialog.open ||
    !tapeOperation ||
    tapeOperation === "form" ||
    tapeRefreshing
  )
    return;
  tapeRefreshing = true;
  const id = tapeOperation;
  try {
    const [r, events] = await Promise.all([
      api(`/v1/runs/${encodeURIComponent(id)}`),
      api(`/v1/runs/${encodeURIComponent(id)}/events`),
    ]);
    if (tapeOperation !== id || !tapeDialog.open) return;
    tapeDialog.innerHTML = `<div class="p-8"><div class="flex items-center justify-between"><h2 class="text-xl font-semibold">${esc(tapeActionText[r.tape.action])} · ${esc(r.result?.tape?.barcode || r.tape.barcode || ("Пул " + r.tape.pool_id))}</h2>${badge(r.status)}</div><p class="my-4 text-sm">${esc(r.result?.phase || "Ожидает запуска")} · привод ${esc(r.result?.tape?.drive_id || r.tape.drive_id || "будет определён")}</p>${operationProgress(r, r.result)}${r.error ? `<p class="my-4 text-sm text-rose-600" role="alert">${esc(r.error)}</p>` : ""}${r.result?.tape?.copy_id ? `<p class="my-4 break-all text-xs">Копия на ленте: ${esc(r.result.tape.copy_id)}</p>` : ""}${r.result?.tape?.snapshot ? `<p class="my-4 break-all text-xs">Восстановлена на диск: ${esc(r.result.tape.snapshot.id)}. Копия зарегистрирована в истории с удержанием.</p>` : ""}<h3 class="my-4 font-semibold">Журнал</h3><div class="max-h-64 space-y-2 overflow-y-auto">${events.map((v) => `<div class="rounded-xl bg-slate-50 p-3 text-xs"><p>${esc(v.action)} · ${date(v.at)}</p><p class="mt-2 break-all">${esc(v.result)}</p></div>`).join("")}</div><div class="mt-6 flex gap-3"><button class="btn-secondary" data-tape-back>К библиотеке</button><button class="btn-secondary" data-close="tape-dialog">Закрыть</button></div></div>`;
    if (!tapeDialog.open) tapeDialog.showModal();
  } finally {
    tapeRefreshing = false;
  }
}
document.addEventListener("click", (e) => {
  const b = e.target.closest("button");
  if (!b) return;
  (async () => {
    if (b.dataset.tapeLibrary) {
      tapeOperation = "";
      await showTapeLibrary(b.dataset.tapeLibrary);
    }
    if (b.hasAttribute("data-tape-pool-new")) await tapePoolForm();
    if (b.dataset.tapePoolEdit) await tapePoolForm(b.dataset.tapePoolEdit);
    if (b.dataset.tapePoolWrite) await tapePoolWrite(b.dataset.tapePoolWrite);
    if (b.dataset.tapeRun) await showTapeOperation(b.dataset.tapeRun);
    if (b.dataset.tapeAction)
      await tapeActionForm(
        b.dataset.tapeAction,
        b.dataset.barcode,
        b.dataset.copy,
      );
    if (b.hasAttribute("data-tape-back")) {
      tapeOperation = "";
      await showTapeLibrary(tapeMedia);
    }
  })().catch((e) => notice(e.message));
});
tapeDialog.addEventListener("close", () => {
  tapeOperation = "";
});
setInterval(() => {
  if (tapeDialog.open)
    (tapeOperation
      ? refreshTapeOperation()
      : showTapeLibrary(tapeMedia, true)
    ).catch((e) => notice(e.message));
}, 2000);

async function tapePoolForm(id = "") {
 tapeOperation="form";
 const p=(tapeLibrary.pools || []).find(p=>p.id===id) || {id:"",name:"",barcodes:[],drives:[]};
 tapeDialog.innerHTML=`<form class="p-8" id="tape-pool-form"><h2 class="text-xl font-semibold">Пул кассет</h2><label class="label mt-4 block">ID<input name="id" class="field mt-2 w-full" pattern="[A-Z0-9_-]{1,32}" value="${esc(p.id)}" ${id ? "readonly" : ""} required></label><label class="label mt-4 block">Название<input name="name" class="field mt-2 w-full" maxlength="128" value="${esc(p.name)}" required></label><p class="my-4">Кассеты</p>${tapeLibrary.cartridges.map(c=>`<label class="my-2 block"><input type="checkbox" name="barcode" value="${esc(c.barcode)}" ${p.barcodes.includes(c.barcode)?"checked":""}> ${esc(c.barcode)} · ${esc(c.label || "Без метки")}</label>`).join("")}<p class="my-4">Приводы</p>${tapeLibrary.drives.map(d=>`<label class="my-2 block"><input type="checkbox" name="drive" value="${esc(d.id)}" ${p.drives.includes(d.id)?"checked":""}> ${esc(d.id)}</label>`).join("")}<p class="my-4 text-sm text-slate-500">Для автоматической записи сначала маркируйте кассеты. Неизвестные, защищённые и неподтверждённые носители не выбираются. Стирание автоматически не выполняется.</p><p data-pool-error class="my-3 text-sm text-rose-600"></p><button type="button" class="btn-secondary" data-tape-back>Назад</button><button class="btn" type="submit">Сохранить</button></form>`;
 const form=tapeDialog.querySelector("form");form.onsubmit=async ev=>{ev.preventDefault();const b=form.querySelector('[type="submit"]');b.disabled=true;try{const f=new FormData(form);const pool={id:f.get("id"),name:f.get("name"),barcodes:f.getAll("barcode"),drives:f.getAll("drive")};await api(`/v1/media/${encodeURIComponent(tapeMedia)}/tape/pools/${encodeURIComponent(pool.id)}`,{method:"PUT",body:JSON.stringify(pool)});tapeOperation="";await showTapeLibrary(tapeMedia);}catch(e){form.querySelector("[data-pool-error]").textContent=e.message;b.disabled=false;}};
}
async function tapePoolWrite(pool) {
 tapeOperation="form";
 const backups=(await api("/v1/runs")).filter(r=>r.kind==="backup"&&r.status==="success"&&r.job.media_id===tapeMedia&&r.snapshot_state!=="deleted");
 tapeDialog.innerHTML=`<form class="p-8"><h2 class="text-xl font-semibold">Автоматическая запись · ${esc(pool)}</h2><label class="label mt-4 block">Резервная копия<select class="field mt-2 w-full" name="source" required>${backups.map(r=>`<option value="${esc(r.id)}">${esc(r.job.name)} · ${date(r.created_at)}</option>`).join("")}</select></label><label class="label mt-4 block">Хранение, дней<input class="field mt-2 w-full" name="days" type="number" min="1" max="36500" value="30" required></label><p class="my-4 text-sm">Выбор кассеты → загрузка → запись с проверкой → выгрузка. При отсутствии подходящего носителя задание ожидает до отмены или общего тайм-аута.</p><p data-pool-error class="my-3 text-sm text-rose-600"></p><button type="button" class="btn-secondary" data-tape-back>Назад</button><button class="btn" type="submit">В очередь</button></form>`;
 const form=tapeDialog.querySelector("form");form.onsubmit=async ev=>{ev.preventDefault();const b=form.querySelector('[type="submit"]');b.disabled=true;try{const f=new FormData(form);const run=await post(`/v1/media/${encodeURIComponent(tapeMedia)}/tape/operations`,{action:"write",pool_id:pool,snapshot_id:f.get("source"),retention_days:Number(f.get("days"))});await showTapeOperation(run.id);}catch(e){form.querySelector("[data-pool-error]").textContent=e.message;b.disabled=false;}};
}
