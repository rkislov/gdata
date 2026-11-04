// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
"use strict";
const serviceCertificateButton=document.createElement("button");
serviceCertificateButton.className="btn-secondary";serviceCertificateButton.textContent="Сервисные сертификаты";serviceCertificateButton.hidden=true;
document.querySelector("#new-job").before(serviceCertificateButton);
const certificateNotice=document.createElement("p");certificateNotice.className="my-4 text-sm text-rose-600";certificateNotice.setAttribute("role","status");certificateNotice.hidden=true;
document.querySelector("#new-job").parentElement.after(certificateNotice);
const serviceCertificateDialog=document.createElement("dialog");serviceCertificateDialog.className="repository-dialog access-dialog";document.body.append(serviceCertificateDialog);
let serviceCertificateLast=0,serviceCertificateRequest=0;
window.refreshServiceCertificates=async me=>{
 const admin=me.role==="admin";serviceCertificateButton.hidden=!admin;
 if(!admin){serviceCertificateRequest++;serviceCertificateLast=0;certificateNotice.hidden=true;serviceCertificateDialog.close();serviceCertificateDialog.replaceChildren();return;}
 if(Date.now()-serviceCertificateLast<30000)return;
 serviceCertificateLast=Date.now();const generation=++serviceCertificateRequest;
 try{const data=await api("/v1/service-certificates");if(generation!==serviceCertificateRequest||window.srkUser?.role!=="admin")return;
  certificateNotice.textContent=data.alerts.length?`Сертификаты: требуют внимания ${data.alerts.length}. Откройте «Сервисные сертификаты» для сроков и ошибок продления.`:"";certificateNotice.hidden=!data.alerts.length;
 }catch(e){if(generation===serviceCertificateRequest){certificateNotice.textContent="Не удалось проверить сроки сервисных сертификатов.";certificateNotice.hidden=false;}}
};
serviceCertificateButton.onclick=async()=>{
 try{const data=await api("/v1/service-certificates");if(window.srkUser?.role!=="admin")return;
  const labels={healthy:"Действует",expiring:"Истекает в течение 30 дней",critical:"Осталось не более 7 дней",expired:"Истёк",unavailable:"Срок неизвестен"};
  serviceCertificateDialog.innerHTML=`<div class="flex justify-between"><h2 class="text-xl font-semibold">Сервисные сертификаты</h2><button class="btn-secondary" data-cert-close>Закрыть</button></div><p class="my-4 text-sm">Выпуск сервисных сертификатов: ${data.issuer_enabled?"настроен":"не настроен"}. Автопродление включается в конфигурации каждого компонента. Корневой CA обновляет администратор.</p>${data.certificates.map(v=>`<section class="tile my-3"><strong>${esc(v.role)} / ${esc(v.id)}</strong><p>${esc(labels[v.state]||v.state)}${v.stale?" · Нет свежего heartbeat":""}</p><p>Действует до: ${v.expires_at&&v.expires_at!=="0001-01-01T00:00:00Z"?esc(new Intl.DateTimeFormat("ru-RU",{year:"numeric",month:"short",day:"2-digit",hour:"2-digit",minute:"2-digit"}).format(new Date(v.expires_at))):"неизвестно"}</p><p class="text-sm">Автопродление: ${v.auto_renew?"включено":"выключено"}</p>${v.renewal_error?`<p class="text-rose-600">Ошибка автоматического продления. Проверьте журнал компонента.</p>`:""}<p class="text-xs">Серийный номер: ${esc(v.serial||"—")}</p>${v.registered_serial?`<p class="text-xs">Последняя выдача в реестре: ${esc(v.registered_serial)}</p>`:""}</section>`).join("")}`;
  serviceCertificateDialog.querySelector("[data-cert-close]").onclick=()=>serviceCertificateDialog.close();serviceCertificateDialog.showModal();
 }catch(e){notice(e.message);}
};
// Clear privileged views immediately on logout, before the next polling cycle.
document.querySelector("#logout").addEventListener("click",()=>{serviceCertificateRequest++;serviceCertificateLast=0;certificateNotice.hidden=true;serviceCertificateButton.hidden=true;serviceCertificateDialog.close();serviceCertificateDialog.replaceChildren();});
