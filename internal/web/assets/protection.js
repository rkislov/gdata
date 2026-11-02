// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
"use strict";
const protectionDialog=document.createElement("dialog");
protectionDialog.id="protection-dialog";
protectionDialog.className="repository-dialog";
document.body.append(protectionDialog);
const protectionButton=document.createElement("button");
protectionButton.className="btn-secondary";
protectionButton.textContent="Защита 3-2-1";
document.querySelector("#new-vm").before(protectionButton);
protectionButton.onclick=()=>loadProtection().catch(e=>alert(e.message));
let protectionData;
const protectionNames={protected:"Защищено",attention:"Требует внимания","no-confirmed-copy":"Нет подтверждённой копии"};
const mediumNames={disk:"Диск / СХД",tape:"Лента",flash:"Flash",cloud:"Объектное / облачное"};
const storageNames={repository:"Репозиторий",tape:"Кассета",flash:"Съёмный flash",longhorn:"Backupstore Longhorn"};
const protectionAge=seconds=>seconds==null?"—":seconds<3600?`${Math.floor(seconds/60)} мин`:`${(seconds/3600).toFixed(1)} ч`;
const protectionHead=title=>`<div class="mb-5 flex items-center justify-between gap-4"><h2 class="text-xl font-semibold">${esc(title)}</h2><button type="button" class="btn-secondary" data-close="protection-dialog">Закрыть</button></div>`;
function protectionShow(html){
 protectionDialog.innerHTML=`<div class="p-8">${html}</div>`;
 if(!protectionDialog.open)protectionDialog.showModal();
}
async function loadProtection(){
 protectionShow(protectionHead("Защищённость 3-2-1")+'<p role="status">Проверяем каталоги хранилищ…</p>');
 protectionData=await api("/v1/protection");renderProtection();
}
function renderProtection(){
 const d=protectionData,s=d.settings;
 protectionShow(protectionHead("Защищённость 3-2-1")+`${d.scope_limited?'<p class="my-3 rounded-xl bg-amber-50 p-3 text-sm">Показана только разрешённая область. Счётчики, RPO и результаты восстановления рассчитаны по доступным копиям; это не оценка всей инфраструктуры.</p>':''}`+`<p class="my-4 text-sm text-slate-500">Оригинал + две независимые резервные копии, два типа носителей, одна копия вне зоны аварии источника. Размещение и тип оборудования задаёт администратор; наличие копий подтверждается каталогами медиасерверов.</p><div class="my-5 flex flex-wrap gap-3"><button class="btn" data-protection-settings>Площадки и хранилища</button><button class="btn-secondary" data-protection-refresh>Обновить проверку</button><span class="text-xs text-slate-500">Проверено ${date(d.at)}</span></div>${(d.observations||[]).filter(o=>o.errors?.length).map(o=>`<p class="my-2 text-sm text-amber-700">${esc(o.media_id)}: ${o.errors.map(esc).join("; ")}</p>`).join("")}<div class="grid gap-5 md:grid-cols-2">${d.cards.map(c=>`<article class="tile"><div class="flex items-start justify-between gap-3"><h3 class="font-semibold">${esc(c.job.name)}</h3><span class="rounded-xl px-3 py-2 text-xs ${c.state==="protected"?"bg-brand-50 text-brand-700":"bg-amber-50 text-amber-700"}">${esc(protectionNames[c.state])}</span></div><div class="my-5 grid grid-cols-2 gap-4 text-sm"><div><p class="text-xs text-slate-500">Независимые резервные копии</p><p class="mt-1 text-xl font-semibold">${c.backup_copies} / 2</p></div><div><p class="text-xs text-slate-500">Удалённая копия</p><p class="mt-1 font-semibold">${c.remote?"Подтверждена":"Не подтверждена"}</p></div><div><p class="text-xs text-slate-500">Возраст данных / RPO</p><p class="mt-1">${protectionAge(c.age_seconds)} / ${c.policy?protectionAge(c.policy.rpo_seconds):"не задан"}</p></div><div><p class="text-xs text-slate-500">Отставание удалённой копии</p><p class="mt-1">${protectionAge(c.remote_lag_seconds)}</p></div></div><p class="text-xs">${c.medium_types.map(t=>esc(mediumNames[t]||t)).join(" · ")||"Типы носителей не настроены"}</p><p class="my-3 text-sm">Восстановление: ${c.restore_ok?"✓ Актуальная успешная проверка":"Требует проверки"} · ${date(c.last_restore?.at)}</p>${c.dependency?`<p class="my-3 rounded-xl bg-brand-50 p-3 text-xs">${esc(c.dependency)}. Копий метаданных: ${c.metadata_copies}.</p>`:""}<ul class="my-3 space-y-2 text-xs text-amber-700">${c.reasons.map(reason=>`<li>• ${esc(reason)}</li>`).join("")}</ul><details class="my-4 text-xs"><summary>Копии и основания расчёта</summary><div class="mt-3 space-y-3">${c.copies.map(cp=>`<div class="rounded-xl bg-slate-50 p-3"><p class="font-semibold">${esc(s.storages.find(x=>x.id===cp.storage_id)?.name||cp.media_id+" · не настроено")}</p><p class="mt-1">${date(cp.data_at)} · ${cp.available&&cp.verified?"Есть подтверждение":"Не подтверждена"}${cp.metadata_only?" · Метаданные":""}</p><p class="mt-1 break-all">${esc(cp.snapshot_id)}</p>${cp.reason?`<p class="mt-1">${esc(cp.reason)}</p>`:""}</div>`).join("")||"Нет сведений о копиях"}</div></details><button class="btn-secondary" data-protection-job="${esc(c.job.id)}">Требования защиты</button></article>`).join("")||'<p>Заданий пока нет.</p>'}</div><p class="mt-5 text-xs text-slate-500">RPO оценивается по времени данных копии, а не завершению репликации. Непрерывный WAL не используется для улучшения этой оценки. Чтение всех блоков и физических лент при открытии страницы не выполняется.</p>`);
 protectionDialog.querySelector("[data-protection-settings]").onclick=renderProtectionSettings;
 protectionDialog.querySelector("[data-protection-refresh]").onclick=()=>loadProtection().catch(e=>alert(e.message));
 protectionDialog.querySelectorAll("[data-protection-job]").forEach(b=>b.onclick=()=>editProtectionPolicy(b.dataset.protectionJob));
}
function renderProtectionSettings(){
 const s=protectionData.settings;
 protectionShow(protectionHead("Площадки и хранилища")+`<button class="btn-secondary mb-5" id="protection-back">Карточки защиты</button><p class="my-3 text-sm">Одинаковая зона аварии означает, что площадки могут пострадать одновременно. Одинаковый домен отказа указывает на общее устройство или backend: его копии считаются один раз.</p>${[["sites","Площадки"],["storages","Хранилища"]].map(([key,title])=>`<section class="my-6"><div class="flex items-center justify-between"><h3 class="font-semibold">${title}</h3><button class="btn" data-setting-add="${key}">Добавить</button></div><div class="mt-3 space-y-3">${s[key].map(v=>`<div class="rounded-xl border border-slate-200 p-4"><p class="font-semibold">${esc(v.name)}</p><p class="my-2 text-xs">${esc(key==="sites"?"Зона аварии: "+v.disaster_domain:(storageNames[v.kind]||v.kind)+" · "+(s.sites.find(site=>site.id===v.site_id)?.name||v.site_id)+" · Домен отказа: "+v.failure_domain)}</p><div class="flex gap-3"><button class="btn-secondary" data-setting-edit="${key}" data-id="${esc(v.id)}">Изменить</button><button class="btn-secondary" data-setting-delete="${key}" data-id="${esc(v.id)}">Убрать настройку</button></div></div>`).join("")||'<p class="text-sm text-slate-500">Пока не настроено</p>'}</div></section>`).join("")}<p class="my-3 text-xs text-slate-500">Изменение настроек не перемещает данные и не удаляет копии. Для офлайнового носителя наличие данных не считается подтверждённым, пока он недоступен для инвентаризации.</p><p class="text-sm text-rose-600" role="alert"></p>`);
 protectionDialog.querySelector("#protection-back").onclick=renderProtection;
 protectionDialog.querySelectorAll("[data-setting-add]").forEach(b=>b.onclick=()=>editProtectionSetting(b.dataset.settingAdd));
 protectionDialog.querySelectorAll("[data-setting-edit]").forEach(b=>b.onclick=()=>editProtectionSetting(b.dataset.settingEdit,b.dataset.id));
 protectionDialog.querySelectorAll("[data-setting-delete]").forEach(b=>b.onclick=async()=>{
  const next=structuredClone(s);next[b.dataset.settingDelete]=next[b.dataset.settingDelete].filter(v=>v.id!==b.dataset.id);
  try{await saveProtection(next);renderProtectionSettings();}catch(e){protectionDialog.querySelector('[role="alert"]').textContent=e.message;}
 });
}
const protectionOptions=(items,value)=>items.map(([id,label])=>`<option value="${esc(id)}" ${id===value?"selected":""}>${esc(label)}</option>`).join("");
const protectionField=(label,name,value,extra="")=>`<label class="label">${label}<input class="field mt-2" name="${name}" value="${esc(value||"")}" ${extra}></label>`;
async function saveProtection(settings){
 protectionData.settings=await api("/v1/protection/settings",{method:"PUT",body:JSON.stringify(settings)});
 // Reload evidence after configuration changes; no stale green statuses.
 protectionData=await api("/v1/protection");
}
function editProtectionSetting(key,id){
 const s=protectionData.settings,v=s[key].find(v=>v.id===id)||{},freshID=id||crypto.randomUUID();
 let fields=protectionField("Название","name",v.name,"required maxlength=200");
 if(key==="sites"){
  fields+=protectionField("Зона общей аварии","disaster_domain",v.disaster_domain||freshID,"required maxlength=128");
 }else{
  fields+=`<label class="label">Вид хранилища<select class="field mt-2" name="kind">${protectionOptions(Object.entries(storageNames),v.kind||"repository")}</select></label><label class="label">Тип носителя<select class="field mt-2" name="medium_type">${protectionOptions(Object.entries(mediumNames),v.medium_type||"disk")}</select></label><label class="label">Площадка<select class="field mt-2" name="site_id" required>${protectionOptions(s.sites.map(x=>[x.id,x.name]),v.site_id)}</select></label>`;
  fields+=protectionField("Домен отказа / общая СХД","failure_domain",v.failure_domain||freshID,"required maxlength=128");
  fields+=`<label class="label" data-storage-field="media">Медиасервер<select class="field mt-2" name="media_id">${protectionOptions((protectionData.observations||[]).map(x=>[x.media_id,x.media_id]),v.media_id)}</select></label><div data-storage-field="repository">${protectionField("Идентификатор репозитория","repository_id",v.repository_id)}</div><div data-storage-field="tape" class="grid gap-4">${protectionField("Идентификатор библиотеки","library_id",v.library_id)}${protectionField("Штрихкод кассеты","barcode",v.barcode)}</div><div data-storage-field="flash">${protectionField("UUID съёмного накопителя","volume_id",v.volume_id)}</div>`;
 }
 protectionShow(protectionHead(key==="sites"?"Настройка площадки":"Настройка хранилища")+`<form id="protection-setting-form"><div class="grid gap-5 md:grid-cols-2">${fields}</div><p class="my-5 text-xs text-slate-500">Домен — постоянный идентификатор без пробелов. Для общих ресурсов используйте одинаковые значения, даже если они доступны через разные серверы.</p><p role="alert" class="my-4 text-sm text-rose-600"></p><button type="submit" class="btn">Сохранить</button></form>`);
 const form=protectionDialog.querySelector("form"),f=form.elements;
 if(key==="storages"){
  const visible=()=>{
   const kind=f.kind.value;
   form.querySelectorAll("[data-storage-field]").forEach(el=>{el.hidden=el.dataset.storageField!==(kind==="repository"?"repository":kind)&&!(el.dataset.storageField==="media"&&kind!=="longhorn");});
   if(kind==="tape"||kind==="flash")f.medium_type.value=kind;
   f.medium_type.disabled=kind==="tape"||kind==="flash";
  };
  const identity=()=>{if(f.kind.value==="repository")f.repository_id.value=protectionData.observations.find(o=>o.media_id===f.media_id.value)?.repository_id||"";};
  f.kind.onchange=()=>{visible();if(!id)identity();};f.media_id.onchange=identity;visible();if(!id)identity();
 }
 form.onsubmit=async e=>{
  e.preventDefault();const next=structuredClone(s),value={id:freshID,name:f.name.value};
  const keys=key==="sites"?["disaster_domain"]:["kind","medium_type","site_id","failure_domain","media_id","repository_id","library_id","barcode","volume_id"];
  keys.forEach(k=>value[k]=f[k].value);
  next[key]=next[key].filter(v=>v.id!==freshID);next[key].push(value);
  try{await saveProtection(next);renderProtectionSettings();}catch(error){form.querySelector('[role="alert"]').textContent=error.message;}
 };
}
function editProtectionPolicy(jobID){
 const s=protectionData.settings,p=s.policies.find(p=>p.job_id===jobID)||{},job=protectionData.cards.find(c=>c.job.id===jobID)?.job;
 protectionShow(protectionHead("Требования защиты · "+(job?.name||jobID))+`<form><div class="grid gap-5 md:grid-cols-2"><label class="label">Площадка оригинала<select class="field mt-2" name="source_site_id" required>${protectionOptions(s.sites.map(x=>[x.id,x.name]),p.source_site_id)}</select></label><label class="label">Носитель оригинала<select class="field mt-2" name="source_medium_type">${protectionOptions(Object.entries(mediumNames),p.source_medium_type||"disk")}</select></label>${protectionField("Домен отказа оригинала","source_failure_domain",p.source_failure_domain||"source-"+jobID,"required maxlength=128")}${protectionField("RPO, часов","rpo",(p.rpo_seconds||86400)/3600,'type="number" min="0.016667" max="8760" step="any" required')}${protectionField("Проверка восстановления не старше, дней","restore_age",(p.restore_max_age_seconds||604800)/86400,'type="number" min="0.000695" max="365" step="any" required')}<label class="label">Backupstore Longhorn<select class="field mt-2" name="longhorn_storage_id">${protectionOptions([["","Не используется / не настроен"],...s.storages.filter(x=>x.kind==="longhorn").map(x=>[x.id,x.name])],p.longhorn_storage_id||"")}</select></label></div><p class="my-5 text-sm">Для Longhorn доступны только копии метаданных СРК и зависимость от внешнего backupstore. Даже несколько проверенных JSON-копий не образуют две независимые копии PVC.</p><p role="alert" class="my-4 text-sm text-rose-600"></p><button class="btn" type="submit">Сохранить требования</button></form>`);
 const form=protectionDialog.querySelector("form"),f=form.elements;
 form.onsubmit=async e=>{
  e.preventDefault();const next=structuredClone(s),policy={job_id:jobID,source_site_id:f.source_site_id.value,source_medium_type:f.source_medium_type.value,source_failure_domain:f.source_failure_domain.value,rpo_seconds:Math.round(Number(f.rpo.value)*3600),restore_max_age_seconds:Math.round(Number(f.restore_age.value)*86400),longhorn_storage_id:f.longhorn_storage_id.value};
  next.policies=next.policies.filter(p=>p.job_id!==jobID);next.policies.push(policy);
  try{await saveProtection(next);renderProtection();}catch(error){form.querySelector('[role="alert"]').textContent=error.message;}
 };
}
