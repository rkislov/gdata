// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
const pitrCalendarDialog = document.createElement("dialog");
pitrCalendarDialog.className = "repository-dialog";
pitrCalendarDialog.id = "pitr-calendar-dialog";
document.body.append(pitrCalendarDialog);
let currentWALPlan = null;
const walStatusText = {candidate:"Предварительно доступно",unknown:"Недостаточно временных меток",gap:"Есть пропуск WAL",expired:"Базовая копия просрочена",timeline_switch:"Смена timeline",clock_or_identity_error:"Ошибка часов или идентификатора"};
function pitrMonth(calendar, month) {
  const start = new Date(`${month}-01T00:00:00Z`);
  if (!Number.isFinite(+start)) return "";
  const end = new Date(Date.UTC(start.getUTCFullYear(),start.getUTCMonth()+1,1));
  let cells = "";
  for (let day = +start; day < +end; day += 86400000) {
    const windows = calendar.windows.filter(w => +new Date(w.from) < day + 86400000 && +(w.to_exclusive ? new Date(w.to_exclusive) : new Date(w.from)) >= day);
    const candidate = windows.some(w => w.to_exclusive && w.status !== "expired" && +new Date(w.to_exclusive) > day && +new Date(w.from) < day + 86400000);
    const broken = windows.some(w => w.gaps.length);
    const color = candidate ? "bg-emerald-50 text-emerald-700" : windows.length ? "bg-amber-50 text-amber-700" : "bg-slate-50 text-slate-400";
    cells += `<div class="rounded-xl p-3 ${color}" title="${candidate ? "Кандидат PITR; границы уточняются ниже" : windows.length ? "Нужна проверка" : "Нет интервала"}">${new Date(day).getUTCDate()}<span class="block text-[10px]">${candidate ? "PITR*" : windows.length ? "Неизв." : "—"}</span></div>`;
  }
  return cells;
}
async function showPITRCalendar(media,archive) {
 const calendar = await api(`/v1/media/${encodeURIComponent(media)}/wal/${encodeURIComponent(archive)}/calendar`);
 const month = new Date().toISOString().slice(0,7);
 pitrCalendarDialog.innerHTML = `<div class="p-8"><div class="flex items-center justify-between gap-3"><h2 class="text-xl font-semibold">Календарь PITR · UTC</h2><button class="btn-secondary" data-close="pitr-calendar-dialog">×</button></div><p class="my-4 text-sm leading-6 text-slate-500">Зелёный — кандидат восстановления по непрерывной цепочке и временным меткам транзакций. Красные записи ниже показывают пропуски по LSN; их календарное время неизвестно. Янтарный — данных недостаточно. Точный результат подтверждается запуском PostgreSQL; время загрузки WAL не используется как время восстановления.</p><input id="pitr-month" class="field" type="month" value="${month}"><div id="pitr-days" class="my-5 grid grid-cols-7 gap-2"></div><div class="space-y-4">${calendar.windows.map(w=>`<article class="rounded-xl border border-slate-100 p-4"><p class="font-semibold">${esc(walStatusText[w.status]||w.status)}</p><p class="my-2 break-all text-xs">База ${esc(w.base_id)} · ${esc(w.from)} ≤ цель &lt; ${esc(w.to_exclusive || "не установлена")}</p><p class="text-xs">Непрерывная цепочка до LSN ${esc(w.contiguous_end)}. ${w.hold ? "Удержание включено." : `Хранение до ${esc(w.expires_at)}.`}</p>${w.gaps.map(g=>`<p class="mt-2 text-xs text-rose-600">Timeline ${g.timeline}: нет ${g.count} сегментов, LSN ${esc(g.from)} — ${esc(g.to)}</p>`).join("")}</article>`).join("") || "<p>Нет зависимых базовых копий.</p>"}</div><button id="wal-cleanup-preview" class="btn-secondary mt-6">Предварительная очистка WAL</button></div>`;
 const renderMonth = () => {pitrCalendarDialog.querySelector("#pitr-days").innerHTML=pitrMonth(calendar,pitrCalendarDialog.querySelector("#pitr-month").value);};
 renderMonth();pitrCalendarDialog.querySelector("#pitr-month").onchange=renderMonth;
 pitrCalendarDialog.querySelector("#wal-cleanup-preview").onclick=()=>showWALCleanup(media,archive).catch(e=>notice(e.message));
 walDialog.close();pitrCalendarDialog.showModal();
}
async function showWALCleanup(media,archive) {
 pitrCalendarDialog.innerHTML=`<div class="p-8"><h2 class="text-xl font-semibold">Безопасная очистка WAL</h2><p class="my-4 text-sm leading-6 text-slate-500">Все физически существующие базовые копии, включая просроченные и удержанные, защищают нужную цепочку. Активное создание базы блокирует очистку. Последний сегмент и история timeline сохраняются. План действует 15 минут и проверяется заново при выполнении.</p><form id="wal-gc-form"><label class="label">Удалять только WAL, архивированные до (UTC)</label><input id="wal-before" class="field my-3 w-full" type="datetime-local" value="${new Date(Date.now()-30*86400000).toISOString().slice(0,16)}" required><label class="flex gap-3 text-sm"><input id="wal-orphan" type="checkbox">Разрешить очистку архива без базовых копий</label><button class="btn mt-5">Рассчитать план</button></form><div id="wal-gc-result" class="mt-5"></div><button data-close="pitr-calendar-dialog" class="btn-secondary mt-5">Закрыть</button></div>`;
 currentWALPlan=null;
 pitrCalendarDialog.querySelector("#wal-gc-form").onsubmit=async event=>{
  event.preventDefault();currentWALPlan=null;
  const container=pitrCalendarDialog.querySelector("#wal-gc-result");container.textContent="Проверка зависимостей…";
  try {
   const before=new Date(pitrCalendarDialog.querySelector("#wal-before").value+":00Z").toISOString();
   const orphan=pitrCalendarDialog.querySelector("#wal-orphan").checked;
   currentWALPlan=await api(`/v1/media/${encodeURIComponent(media)}/wal/${encodeURIComponent(archive)}/gc?before=${encodeURIComponent(before)}&allow_orphan=${orphan}`);
   const p=currentWALPlan;
   container.innerHTML=`<p>К удалению: ${p.names.length} сегментов · ${size(p.bytes)}</p><p class="my-3 text-sm">Зависимых баз: ${p.dependencies.length}. Активных защит: ${p.active_pins}. Ожидают подтверждения реплик: ${p.replication_protected_files || 0} файлов · ${size(p.replication_protected_bytes || 0)}.</p><p class="my-3 break-all text-xs">${esc(p.dependencies.join(", "))}</p><p class="text-sm text-rose-600">${esc(p.blocked.join("; "))}</p><button id="wal-gc-apply" class="btn mt-4" ${!p.names.length||p.blocked.length ? "disabled" : ""}>Удалить перечисленные ${p.names.length} сегментов</button><details class="mt-3 text-xs"><summary>Список сегментов</summary><p class="break-all">${esc(p.names.join(", "))}</p></details>`;
   container.querySelector("#wal-gc-apply").onclick=async event=>{
    event.target.disabled=true;
    try {await post(`/v1/media/${encodeURIComponent(media)}/wal-gc`,{wal_plan:currentWALPlan});pitrCalendarDialog.close();notice("Очистка WAL поставлена в очередь");await refresh();}
    catch(e){notice(e.message);event.target.disabled=false;}
   };
  } catch(e){container.textContent=e.message;}
 };
}
document.addEventListener("click",event=>{const button=event.target.closest("[data-pitr-calendar]");if(button)showPITRCalendar(button.dataset.media,button.dataset.pitrCalendar).catch(e=>notice(e.message));});
