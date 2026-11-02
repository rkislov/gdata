// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
"use strict";
const kubeChecksButton = document.createElement("button");
kubeChecksButton.className = "btn-secondary";
kubeChecksButton.textContent = "Проверки Kubernetes";
document.querySelector("#new-vm").before(kubeChecksButton);
kubeChecksButton.addEventListener("click", () => showKubernetesChecks().catch(e => alert(e.message)));
const kubeCheckStates = {passed:"Проверка пройдена",failed:"Проверка не пройдена",checking:"Проверка приложений",restoring:"Восстановление"};
const kubeCleanupStates = {pending:"Ожидается",running:"Выполняется",complete:"Ресурсы удалены",retained:"Сохранены для диагностики",failed:"Требуется внимание","not-needed":"Не требуется"};
function kubernetesCheckReport(result){
 const c=result?.kubernetes_check;if(!c)return "";
 return `<div class="my-3 rounded-xl bg-brand-50 p-4 text-sm"><p class="font-semibold">${esc(kubeCheckStates[c.state]||c.state)}</p><p class="mt-2">Namespace: ${esc(c.namespace||"—")} · Готовых Pod: ${c.ready_pods||0}</p><p class="mt-2">Очистка: ${esc(kubeCleanupStates[c.cleanup]||c.cleanup)}</p><p class="mt-2 break-all text-xs">Копия: ${esc(c.snapshot_id||"—")} · ${date(c.started_at)}</p><p class="mt-2">Длительность: ${Number(c.duration_seconds||0).toFixed(1)} с</p>${(c.checks||[]).map(p=>`<p class="mt-2">${p.passed?"✓":"✕"} ${esc(p.name)} · ${esc(p.detail)}</p>`).join("")}${c.error?`<p class="mt-2 text-rose-600">${esc(c.error)}</p>`:""}</div>`;
}
async function showKubernetesChecks(){
 clearTimeout(kubernetesTimer);
 const rows=await api("/v1/kubernetes-checks");
 kubernetesDialog.innerHTML=`<div class="p-8"><div class="flex items-center justify-between"><h2 class="text-xl font-semibold">Проверки восстановления Kubernetes</h2><button class="btn-secondary" data-close="kubernetes-dialog">Закрыть</button></div><p class="my-5 text-sm">По расписанию восстанавливается последняя доступная копия. Проверяются готовность Pod и HTTP-ответы приложений; временные ресурсы очищаются.</p><button class="btn" id="new-kube-check">Добавить проверку</button><div class="my-5 grid gap-4">${rows.map((row,i)=>`<article class="rounded-2xl border border-slate-200 p-4"><p class="font-semibold">${esc(state.jobs.find(j=>j.id===row.policy.job_id)?.name||row.policy.job_id)} · ${esc(row.policy.source_namespace)}</p><p class="mt-2 text-sm">${row.policy.enabled?"Включена":"Приостановлена"} · каждые ${row.policy.interval_seconds/3600} ч · следующий запуск ${row.policy.enabled?date(row.policy.next_run):"—"}</p>${row.last_run?kubernetesCheckReport(row.last_run.result):'<p class="my-3 text-sm">Ещё не выполнялась</p>'}${row.last_run?.error?`<p class="my-3 text-sm text-rose-600">${esc(row.last_run.error)}</p>`:""}<div class="mt-3 flex gap-3"><button class="btn-secondary" data-check-edit="${i}">Настроить</button>${row.last_run?`<button class="btn-secondary" data-check-report="${esc(row.last_run.id)}">Отчёт и журнал</button>`:""}</div></article>`).join("")||'<p>Политик пока нет.</p>'}</div></div>`;
 if(!kubernetesDialog.open)kubernetesDialog.showModal();
 kubernetesDialog.querySelector("#new-kube-check").onclick=()=>editKubernetesCheck();
 kubernetesDialog.querySelectorAll("[data-check-edit]").forEach(b=>b.onclick=()=>editKubernetesCheck(rows[Number(b.dataset.checkEdit)].policy));
 kubernetesDialog.querySelectorAll("[data-check-report]").forEach(b=>b.onclick=()=>openKubernetesOperation(b.dataset.checkReport).catch(e=>alert(e.message)));
 if(rows.some(row=>row.policy.enabled&&(!row.last_run||new Date(row.policy.next_run)<=new Date()||["queued","running"].includes(row.last_run.status))))kubernetesTimer=setTimeout(()=>{if(kubernetesDialog.open)showKubernetesChecks().catch(()=>{});},2500);
}
function editKubernetesCheck(policy={}){
 clearTimeout(kubernetesTimer);
 const jobs=state.jobs.filter(j=>["kubernetes","longhorn"].includes(j.type));
 const opts=items=>items.map(([v,label])=>`<option value="${esc(v)}">${esc(label)}</option>`).join("");
 kubernetesDialog.innerHTML=`<form class="p-8"><div class="flex items-center justify-between"><h2 class="text-xl font-semibold">Плановая проверка восстановления</h2><button class="btn-secondary" type="button" data-close="kubernetes-dialog">Закрыть</button></div><div class="my-5 grid gap-4 md:grid-cols-2"><label class="label">Задание<select class="field mt-2" name="job" required>${opts(jobs.map(j=>[j.id,j.name]))}</select></label><label class="label">Целевой профиль<select class="field mt-2" name="profile" required></select></label><label class="label">Исходный namespace<input class="field mt-2" name="source" required value="${esc(policy.source_namespace||"")}" placeholder="demo"></label><label class="label">Префикс временных namespaces<input class="field mt-2" name="prefix" readonly required></label><label class="label">Интервал, часов<input class="field mt-2" type="number" name="hours" min="0.166667" max="8760" step="any" required value="${(policy.interval_seconds||86400)/3600}"></label><label class="label">Тайм-аут проверки, секунд<input class="field mt-2" type="number" name="timeout" min="10" max="7200" required value="${policy.timeout_seconds||600}"></label></div><p class="my-4 text-sm">Проверяется готовность хотя бы одного Pod. CronJob остаются приостановленными. Используйте отдельный тестовый кластер или заранее ограничьте связи приложений с рабочими системами.</p><h3 class="my-3 font-semibold">HTTP-проверки Service</h3><div id="kube-probes" class="grid gap-3"></div><button type="button" class="btn-secondary my-3" id="add-kube-probe">Добавить HTTP-проверку</button><label class="my-3 flex gap-3 text-sm"><input type="checkbox" name="keep" ${policy.keep_on_failure?"checked":""}>Сохранять ресурсы при ошибке для диагностики</label><label class="my-3 flex gap-3 text-sm"><input type="checkbox" name="enabled" ${policy.enabled!==false?"checked":""}>Включить расписание</label><p class="my-4 text-xs text-slate-500">После сохранения включённая политика ставит первую проверку в очередь. Последующие проверки используют новые уникальные namespaces. Исходные копии Longhorn не удаляются.</p><p class="my-4 text-rose-600 text-sm" role="alert"></p><button class="btn" type="submit">Сохранить политику</button></form>`;
 const form=kubernetesDialog.querySelector("form"),f=form.elements;
 const refreshProfiles=()=>{
  const job=jobs.find(j=>j.id===f.job.value),node=state.nodes.find(n=>n.id===job?.agent_id),profiles=node?.status?.kubernetes_check_profiles||{};
  f.profile.innerHTML=opts(Object.keys(profiles).map(v=>[v,v]));
  if(policy.profile&&profiles[policy.profile])f.profile.value=policy.profile;
  const prefix=()=>{f.prefix.value=profiles[f.profile.value]||"";};
  f.profile.onchange=prefix;prefix();
  form.querySelector('[type="submit"]').disabled=Object.keys(profiles).length===0;
  form.querySelector('[role="alert"]').textContent=Object.keys(profiles).length?"":"На агенте нет профилей с разрешённой автоматической проверкой.";
 };
 if(policy.job_id)f.job.value=policy.job_id;
 f.job.onchange=refreshProfiles;refreshProfiles();
 if(policy.id){f.job.disabled=true;f.profile.disabled=true;f.source.readOnly=true;}
 const addProbe=(p={})=>{
  const row=document.createElement("div");row.className="kube-probe rounded-xl border border-slate-200 p-3 grid gap-3";
  row.innerHTML=`<label class="label">Service<input data-probe="service" class="field" required value="${esc(p.service||"")}"></label><div class="grid grid-cols-2 gap-3"><label class="label">Порт<input data-probe="port" class="field" type="number" min="1" max="65535" required value="${p.port||80}"></label><label class="label">HTTP-статус<input data-probe="status" class="field" type="number" min="200" max="299" required value="${p.expected_status||200}"></label></div><label class="label">Путь<input data-probe="path" class="field" required value="${esc(p.path||"/health")}"></label><label class="label">Ожидаемый текст (необязательно)<input data-probe="contains" class="field" value="${esc(p.contains||"")}"></label><button type="button" class="btn-secondary">Убрать проверку</button>`;
  row.querySelector("button").onclick=()=>row.remove();
  form.querySelector("#kube-probes").append(row);
 };
 (policy.http||[]).forEach(addProbe);
 form.querySelector("#add-kube-probe").onclick=()=>addProbe();
 form.onsubmit=async e=>{
  e.preventDefault();const button=form.querySelector('[type="submit"]');button.disabled=true;
  try{
   const http=Array.from(form.querySelectorAll(".kube-probe")).map(row=>{const value=k=>row.querySelector(`[data-probe="${k}"]`).value;return {service:value("service"),port:Number(value("port")),path:value("path"),expected_status:Number(value("status")),contains:value("contains")};});
   await post("/v1/kubernetes-checks",{job_id:f.job.value,profile:f.profile.value,source_namespace:f.source.value,namespace_prefix:f.prefix.value,interval_seconds:Math.round(Number(f.hours.value)*3600),timeout_seconds:Number(f.timeout.value),keep_on_failure:f.keep.checked,enabled:f.enabled.checked,http});
   await showKubernetesChecks();
  }catch(error){form.querySelector('[role="alert"]').textContent=error.message;button.disabled=false;}
 };
}
