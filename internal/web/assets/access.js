// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
"use strict";
const accessStyle=document.createElement("style");document.head.append(accessStyle);
const accountButton=document.createElement("button");accountButton.className="btn-secondary";accountButton.textContent="Пользователи";accountButton.id="users-button";
document.querySelector("#new-job").before(accountButton);
const identityLabel=document.createElement("span");identityLabel.className="text-xs text-slate-500";document.querySelector("#logout").before(identityLabel);
const accessDialog=document.createElement("dialog");accessDialog.className="repository-dialog access-dialog";document.body.append(accessDialog);
window.applyAccess=u=>{
 window.srkUser=u;
 const admin=u.role==="admin",global=admin||(u.all_jobs&&u.all_sites),can=p=>admin||(u.role==="operator"&&u.permissions?.includes(p));
 document.querySelector("#logout").textContent="Выйти";
 identityLabel.textContent=`${u.name||u.username} · ${{admin:"Администратор",operator:"Оператор",auditor:"Аудитор"}[u.role]||u.role}`;
 const hide=[];
 if(!admin)hide.push("#users-button","#catalog-backups-button","#manage-targets","#new-job","#new-vm","[data-toggle]","[data-replicate]","[data-rescue-code]","[data-configure-wal-replication]","[data-protection-settings]","[data-protection-job]","#new-capture","#new-kube-check","[data-check-edit]","[data-import]","[data-import-snapshot]","[data-policy]");
 for(const p of ["run","verify","restore","cancel"])if(!can(p)||(!global&&["cancel"].includes(p)))hide.push(`[data-${p}]`);
 if(!can("restore"))hide.push("[data-approved-restore]");
 if(!can("cleanup"))hide.push("[data-gc-preview]","[data-apply-gc]");
 if(!admin){hide.push('[data-tape-action]:not([data-tape-action="erase"]):not([data-tape-action="recycle"]):not([data-tape-action="restore"]):not([data-tape-action="verify"])','[data-tape-pool]');for(const [p,actions] of Object.entries({erase:["erase","recycle"],restore:["restore"],verify:["verify"]}))if(!can(p)||!global)actions.forEach(a=>hide.push(`[data-tape-action="${a}"]`));}
 protectionButton.hidden=false;
 if(!global)hide.push("[data-gc-preview]","[data-apply-gc]","[data-tape-library]","[data-removable]","[data-import-snapshot]");
 accessStyle.textContent=hide.length?hide.join(",")+"{display:none!important}":"";
};
async function listAccounts(){
 const users=await api("/v1/users");
 accessDialog.innerHTML=`<div class="flex justify-between"><h2 class="text-xl font-semibold">Пользователи и права</h2><button class="btn-secondary" id="access-close">Закрыть</button></div><p class="my-4 text-sm text-slate-500">Изменение прав или пароля отзывает все веб-сессии учётной записи. CLI использует персональный сертификат роли user.</p><button id="account-add" class="btn">Добавить пользователя</button><div class="mt-5 grid gap-3">${users.map((u,i)=>`<button class="tile text-left" data-account="${i}"><strong>${esc(u.name||u.username)}</strong><p class="text-sm">${esc(u.username)} · ${esc(u.role)} · ${u.disabled?"Отключён":"Активен"}</p><p class="text-xs text-slate-500">${u.all_jobs?"Все задания":esc((u.job_ids||[]).join(", "))} / ${u.all_sites?"Все площадки":esc((u.site_ids||[]).join(", "))}</p></button><button class="btn-secondary" data-certs="${i}">Сертификаты · ${esc(u.username)}</button>`).join("")||"Учётные записи ещё не созданы."}</div>`;
 accessDialog.querySelector("#access-close").onclick=()=>accessDialog.close();
 accessDialog.querySelector("#account-add").onclick=()=>editAccount({version:0,role:"auditor",permissions:[],job_ids:[],site_ids:[]});
 accessDialog.querySelectorAll("[data-account]").forEach(b=>b.onclick=()=>editAccount(users[+b.dataset.account]));
 accessDialog.querySelectorAll("[data-certs]").forEach(b=>b.onclick=()=>showCertificates(users[+b.dataset.certs].username).catch(e=>notice(e.message)));
 if(!accessDialog.open)accessDialog.showModal();
}
accountButton.onclick=()=>listAccounts().catch(e=>notice(e.message));
function editAccount(u){
 accessDialog.innerHTML=`<h2 class="text-xl font-semibold mb-4">${u.version?"Изменить учётную запись":"Новая учётная запись"}</h2><form id="account-form" class="grid gap-3 md:grid-cols-2"><label class="label">Логин<input class="field" name="username" required pattern="[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}" value="${esc(u.username||"")}" ${u.version?"readonly":""}></label><label class="label">Имя<input class="field" name="name" value="${esc(u.name||"")}" maxlength="200"></label><label class="label">Роль<select class="field" name="role">${["admin","operator","auditor"].map(r=>`<option ${r===u.role?"selected":""}>${r}</option>`).join("")}</select></label><label class="label">${u.version?"Новый пароль (пусто — сохранить)":"Пароль"}<input class="field" type="password" name="password" autocomplete="new-password" minlength="16" maxlength="72" ${u.version?"":"required"}></label><label><input type="checkbox" name="disabled" ${u.disabled?"checked":""}> Отключить доступ</label><label><input type="checkbox" name="all_jobs" ${u.all_jobs?"checked":""}> Все задания</label><label class="label">ID заданий через запятую<input class="field" name="job_ids" value="${esc((u.job_ids||[]).join(", "))}"></label><label><input type="checkbox" name="all_sites" ${u.all_sites?"checked":""}> Все площадки</label><label class="label">ID площадок через запятую<input class="field" name="site_ids" value="${esc((u.site_ids||[]).join(", "))}"></label><label><input type="checkbox" name="all_restore_sites" ${u.all_restore_sites?"checked":""}> Все целевые площадки восстановления</label><label class="label">ID целевых площадок через запятую<input class="field" name="restore_site_ids" value="${esc((u.restore_site_ids||[]).join(", "))}"></label><fieldset class="md:col-span-2"><legend class="label">Дополнительные права оператора</legend>${Object.entries({run:"Запуск",verify:"Проверка",cancel:"Отмена",restore:"Восстановление",erase:"Стирание / перезапись",cleanup:"Очистка"}).map(([p,label])=>`<label class="mr-4"><input type="checkbox" name="permission" value="${p}" ${(u.permissions||[]).includes(p)?"checked":""}> ${label}</label>`).join("")}</fieldset><p class="text-xs text-slate-500 md:col-span-2">Области заданий и площадок пересекаются. Администратор имеет полный доступ. Восстановление требует доступа к исходному заданию и отдельно к целевой площадке. Отмена и операции с общими носителями требуют глобальной исходной области.</p><p role="alert" class="text-sm text-rose-600 md:col-span-2"></p><div class="flex gap-3 md:col-span-2"><button class="btn" type="submit">Сохранить</button><button class="btn-secondary" type="button" id="account-back">Назад</button></div></form>`;
 accessDialog.querySelector("#account-back").onclick=()=>listAccounts().catch(e=>notice(e.message));
 accessDialog.querySelector("form").onsubmit=async e=>{e.preventDefault();const form=e.currentTarget,d=new FormData(form),split=k=>String(d.get(k)||"").split(",").map(x=>x.trim()).filter(Boolean);const next={username:d.get("username"),name:d.get("name"),role:d.get("role"),version:u.version,disabled:d.has("disabled"),all_jobs:d.has("all_jobs"),all_sites:d.has("all_sites"),job_ids:split("job_ids"),site_ids:split("site_ids"),all_restore_sites:d.has("all_restore_sites"),restore_site_ids:split("restore_site_ids"),permissions:d.getAll("permission"),password:d.get("password")};try{await api("/v1/users",{method:"PUT",body:JSON.stringify(next)});form.reset();await listAccounts();}catch(err){form.querySelector('[role="alert"]').textContent=err.message;}};
}
