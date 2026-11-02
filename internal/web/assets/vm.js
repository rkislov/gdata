// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
"use strict";
const vmDialog = document.createElement("dialog");
vmDialog.id = "vm-dialog";
vmDialog.className = "repository-dialog";
document.body.append(vmDialog);
const vmPath = (proxy, profile) =>
  `/v1/proxies/${encodeURIComponent(proxy)}/virtualization${profile ? `/${encodeURIComponent(profile)}` : ""}`;
const vmOnline = (role) =>
  state.nodes.filter(
    (n) => n.role === role && Date.now() - new Date(n.last_seen) < 30000,
  );
const vmOptions = (items) =>
  items
    .map(([id, name]) => `<option value="${esc(id)}">${esc(name)}</option>`)
    .join("");
const vmField = (label, name, extra = "") =>
  `<label class="label">${label}<input class="field mt-2" name="${name}" ${extra}></label>`;
const vmPolicy =
  "Работающая ВМ: crash-consistent, без гарантии согласованности приложений между дисками. Режим cold-only требует уже выключенную ВМ. СРК не выключает исходную ВМ.";
let vmSelection = 0;
async function openVMBackup() {
  const proxies = vmOnline("proxy"),
    media = vmOnline("media");
  vmDialog.innerHTML = `<form id="vm-backup-form" class="p-8"><div class="mb-5 flex items-center justify-between"><h2 class="text-xl font-semibold">Бэкап виртуальной машины</h2><button type="button" class="btn-secondary" data-close="vm-dialog">Закрыть</button></div><div class="grid gap-5 md:grid-cols-2"><label class="label">Прокси<select class="field mt-2" name="proxy" required>${vmOptions(proxies.map((n) => [n.id, n.id]))}</select></label><label class="label">Профиль платформы<select class="field mt-2" name="profile" required></select></label><label class="label md:col-span-2">Виртуальная машина<select class="field mt-2" name="server" required></select></label>${vmField("Название задания", "name", 'required maxlength="200"')}<label class="label">Медиасервер<select class="field mt-2" name="media" required>${vmOptions(media.map((n) => [n.id, n.id]))}</select></label><label class="label">Согласованность<select class="field mt-2" name="consistency"><option value="crash-consistent">Снимок работающей ВМ</option><option value="cold-only">Только выключенная ВМ</option></select></label>${vmField("Хранение, дней", "retention", 'type="number" min="1" max="36500" value="30" required')}<label class="label">Расписание<select class="field mt-2" name="interval"><option value="0">Только вручную</option><option value="86400">Каждые 24 часа</option><option value="3600">Каждый час</option></select></label></div><p class="my-5 text-sm leading-6 text-slate-500">${vmPolicy} Полная копия всех поддерживаемых дисков хранится с блочной дедупликацией.</p><button type="button" id="vm-journal" class="btn-secondary">Журнал ресурсов платформы</button><div id="vm-plan" class="my-5 rounded-2xl bg-brand-50 p-4 text-sm" role="status">Загрузка профилей…</div><label class="my-5 flex items-center gap-3 text-sm"><input type="checkbox" name="run_now" checked class="size-4 accent-brand-600">Запустить после создания</label><p id="vm-error" class="my-4 text-sm text-rose-600" role="alert"></p><button class="btn" type="submit" disabled>Создать задание ВМ</button></form>`;
  vmDialog.showModal();
  const form = vmDialog.querySelector("form"),
    fields = form.elements;
  let ready = false,
    provider = "";
  const disable = () => {
    ready = false;
    fields.server.disabled = true;
    form.querySelector('[type="submit"]').disabled = true;
  };
  const plan = async () => {
    const ticket = ++vmSelection;
    ready = false;
    form.querySelector('[type="submit"]').disabled = true;
    if (!fields.server.value) return;
    $("#vm-plan").textContent = "Проверка всех дисков ВМ…";
    try {
      const info = await api(
        `${vmPath(fields.proxy.value, fields.profile.value)}/vms/${encodeURIComponent(fields.server.value)}/plan`,
      );
      if (ticket !== vmSelection || !vmDialog.open) return;
      ready =
        !!info.executable &&
        (fields.consistency.value !== "cold-only" ||
          ["SHUTOFF", "down", "stopped"].includes(info.backup?.status));
      $("#vm-plan").textContent = ready
        ? `Готово к копированию · ${info.backup?.name || fields.server.selectedOptions[0].textContent} · конфигурация проверена. ${info.backup?.volume_backed ? "Загрузка с Cinder-тома." : ""}`
        : info.reason || "Для cold-only исходная ВМ должна быть выключена.";
      form.querySelector('[type="submit"]').disabled = !ready || !media.length;
    } catch (error) {
      if (ticket === vmSelection) $("#vm-plan").textContent = error.message;
    }
  };
  const inventory = async () => {
    const ticket = ++vmSelection;
    disable();
    fields.server.innerHTML = "";
    $("#vm-plan").textContent = "Загрузка списка ВМ…";
    provider = fields.profile.selectedOptions[0]?.dataset.kind || "";
    try {
      const list = await api(
        `${vmPath(fields.proxy.value, fields.profile.value)}/vms`,
      );
      if (ticket !== vmSelection || !vmDialog.open) return;
      fields.server.innerHTML = vmOptions(
        list.map((v) => [v.id, `${v.name} · ${v.status} · ${v.id}`]),
      );
      fields.server.disabled = false;
      if (!list.length) {
        $("#vm-plan").textContent = "В профиле нет доступных ВМ.";
        return;
      }
      if (!fields.name.value) fields.name.value = `ВМ · ${list[0].name}`;
      await plan();
    } catch (error) {
      if (ticket === vmSelection) $("#vm-plan").textContent = error.message;
    }
  };
  const profiles = async () => {
    const ticket = ++vmSelection;
    disable();
    fields.profile.innerHTML = "";
    fields.server.innerHTML = "";
    if (!fields.proxy.value) {
      $("#vm-plan").textContent =
        "Нет подключенного прокси. Добавьте профиль платформы в конфигурацию прокси.";
      return;
    }
    try {
      const list = await api(vmPath(fields.proxy.value));
      if (ticket !== vmSelection || !vmDialog.open) return;
      fields.profile.innerHTML = Object.entries(list)
        .map(
          ([name, kind]) =>
            `<option value="${esc(name)}" data-kind="${esc(kind)}">${esc(name)} · ${esc(kind)}</option>`,
        )
        .join("");
      if (!Object.keys(list).length) {
        $("#vm-plan").textContent =
          "На прокси не настроены профили виртуализации.";
        return;
      }
      await inventory();
    } catch (error) {
      if (ticket === vmSelection) $("#vm-plan").textContent = error.message;
    }
  };
  form.querySelector("#vm-journal").onclick = async () => {
    try {
      const list = await api(
        `${vmPath(fields.proxy.value, fields.profile.value)}/journal`,
      );
      $("#vm-plan").innerHTML = list.length
        ? list
            .map(
              (j) =>
                `<div class="my-3 break-all"><p>${esc(j.id)} · ${esc(j.kind)} · ${j.resources.filter((r) => !r.deleted).length} ресурсов</p><p class="my-2 text-rose-700">${esc(j.last_error || "Операция ещё активна или ожидает очистки")}</p><button type="button" class="btn-secondary" data-vm-cleanup="${esc(j.id)}">Повторить безопасную очистку</button></div>`,
            )
            .join("")
        : "Незавершённых операций нет.";
      for (const button of $("#vm-plan").querySelectorAll("[data-vm-cleanup]"))
        button.onclick = async () => {
          button.disabled = true;
          try {
            await post(
              `${vmPath(fields.proxy.value, fields.profile.value)}/cleanup/${encodeURIComponent(button.dataset.vmCleanup)}`,
            );
            await plan();
          } catch (error) {
            $("#vm-error").textContent = error.message;
            button.disabled = false;
          }
        };
    } catch (error) {
      $("#vm-error").textContent = error.message;
    }
  };
  fields.proxy.onchange = profiles;
  fields.profile.onchange = inventory;
  fields.server.onchange = plan;
  fields.consistency.onchange = plan;
  form.onsubmit = async (event) => {
    event.preventDefault();
    if (!ready) return;
    const button = form.querySelector('[type="submit"]');
    button.disabled = true;
    try {
      const job = await post("/v1/jobs", {
        type: provider,
        name: fields.name.value.trim(),
        agent_id: fields.proxy.value,
        media_id: fields.media.value,
        source: `${provider}://${fields.profile.value}/${fields.server.value}`,
        mode: "full",
        retention_days: Number(fields.retention.value),
        interval_seconds: Number(fields.interval.value),
        enabled: Number(fields.interval.value) > 0,
        vm: { consistency: fields.consistency.value },
      });
      // A failed enqueue must not submit the already-created job a second time.
      ready = false;
      let run;
      if (fields.run_now.checked) {
        try {
          run = await post(`/v1/jobs/${job.id}/run`);
        } catch (error) {
          vmDialog.close();
          notice(
            `Задание ${job.name} создано; запуск не принят: ${error.message}. Запустите его в разделе «Задания».`,
          );
          await refresh();
          return;
        }
      }
      vmDialog.close();
      await refresh();
      if (run) await openVMDetail(run.id);
      else notice("Задание ВМ создано.");
    } catch (error) {
      $("#vm-error").textContent = error.message;
      button.disabled = !ready;
    }
  };
  await profiles();
}
$("#new-vm").onclick = openVMBackup;
async function openVMRestore(id) {
  const run = await api(`/v1/runs/${encodeURIComponent(id)}`),
    meta = run.result?.vm;
  if (!meta) throw new Error("В копии отсутствуют метаданные ВМ.");
  $("#detail-dialog").close();
  const proxies = vmOnline("proxy");
  const flavorLabel =
    {
      ovirt: "UUID целевого кластера",
      proxmox: "Целевой узел PVE",
      opennebula: "ID целевого кластера",
    }[meta.provider] || "Flavor ID";
  const storageLabel = {
    ovirt: "UUID домена хранения",
    proxmox: "Хранилище дисков PVE",
    opennebula: "ID хранилища образов",
  }[meta.provider];
  const networkLabel =
    {
      ovirt: "UUID vNIC-профилей через запятую",
      proxmox: "Мосты PVE через запятую",
      opennebula: "ID сетей через запятую",
    }[meta.provider] || "UUID сетей через запятую";
  vmDialog.innerHTML = `<form class="p-8"><div class="mb-5 flex items-center justify-between"><h2 class="text-xl font-semibold">Восстановление в новую ВМ</h2><button type="button" class="btn-secondary" data-close="vm-dialog">Закрыть</button></div><p class="mb-5 text-sm text-slate-500">Копия ${esc(meta.name)} · ${date(meta.snapshot_at)} · ${meta.disks.length} дисков. Создаются новые диски и сетевые порты. Старые IP и MAC автоматически не назначаются.</p><div class="grid gap-5 md:grid-cols-2"><label class="label">Целевой прокси<select class="field mt-2" name="proxy" required>${vmOptions(proxies.map((n) => [n.id, n.id]))}</select></label><label class="label">Целевой профиль<select class="field mt-2" name="profile" required></select></label>${vmField("Имя новой ВМ", "name", `required maxlength="200" value="${esc(meta.name)}-restored"`)}${vmField(flavorLabel, "flavor", `required value="${esc(meta.provider === "proxmox" ? meta.project_id : meta.flavor.id)}"`)}${storageLabel ? vmField(storageLabel, "storage", "required") : ""}${meta.provider === "proxmox" && !meta.archive ? vmField("Хранилище импорта PVE (import)", "imports", "required") : ""}${vmField(networkLabel, "networks", `required value="${esc(meta.networks.map((n) => n.id).join(", "))}"`)}${meta.provider !== "openstack" ? "" : vmField("Группы безопасности через запятую", "security", 'placeholder="default"') + vmField("Ключ SSH (необязательно)", "key") + vmField("Зона доступности (необязательно)", "zone")}</div><p class="my-5 text-sm leading-6 text-slate-500">Ресурсы исходной ВМ: ${meta.flavor.vcpus} vCPU, ${meta.flavor.ram} МиБ RAM. Восстановленная ВМ будет запущена. Статус запущенной ВМ подтверждает API платформы; готовность ОС и приложений проверяется отдельно. Образы и тома новой ВМ сохраняются как её зависимости.</p><p id="vm-error" class="my-4 text-sm text-rose-600" role="alert"></p><button class="btn" type="submit" disabled>Создать и запустить новую ВМ</button></form>`;
  const form = vmDialog.querySelector("form"),
    f = form.elements;
  f.proxy.value = run.job.agent_id;
  if (!f.proxy.value && proxies.length) f.proxy.value = proxies[0].id;
  let ticket = 0;
  const profiles = async () => {
    const current = ++ticket;
    f.profile.innerHTML = "";
    form.querySelector('[type="submit"]').disabled = true;
    try {
      const list = await api(vmPath(f.proxy.value));
      if (current !== ticket || !vmDialog.open) return;
      f.profile.innerHTML = vmOptions(
        Object.entries(list)
          .filter(([, kind]) => kind === meta.provider)
          .map(([name, kind]) => [name, `${name} · ${kind}`]),
      );
      if ([...f.profile.options].some((o) => o.value === meta.profile))
        f.profile.value = meta.profile;
      form.querySelector('[type="submit"]').disabled = !f.profile.value;
      $("#vm-error").textContent = f.profile.value
        ? ""
        : "На выбранном прокси нет профиля исходной платформы.";
    } catch (error) {
      if (current === ticket) $("#vm-error").textContent = error.message;
    }
  };
  f.proxy.onchange = profiles;
  form.onsubmit = async (event) => {
    event.preventDefault();
    const button = form.querySelector('[type="submit"]');
    button.disabled = true;
    try {
      const networks = f.networks.value
        .split(",")
        .map((s) => s.trim())
        .filter(Boolean);
      if (
        !networks.length ||
        new Set(networks).size !== networks.length ||
        networks.some(
          (n) =>
            !(
              meta.provider === "proxmox"
                ? /^[A-Za-z0-9][A-Za-z0-9_.-]*$/
                : meta.provider === "opennebula"
                  ? /^\d+$/
                  : /^[a-f\d]{8}(-[a-f\d]{4}){3}-[a-f\d]{12}$/i
            ).test(n),
        )
      )
        throw new Error(
          "Укажите различные идентификаторы целевых сетей через запятую.",
        );
      const restored = await post(
        `/v1/runs/${encodeURIComponent(id)}/restore`,
        {
          vm_restore: {
            provider: meta.provider,
            proxy_id: f.proxy.value,
            profile: f.profile.value,
            name: f.name.value.trim(),
            flavor_id: f.flavor.value.trim(),
            networks,
            security_groups: (f.security?.value || "")
              .split(",")
              .map((s) => s.trim())
              .filter(Boolean),
            key_name: f.key?.value.trim() || "",
            availability_zone: f.zone?.value.trim() || "",
            storage_domain_id: f.storage?.value.trim() || "",
            import_storage_id: f.imports?.value.trim() || "",
          },
        },
      );
      vmDialog.close();
      await refresh();
      await openVMDetail(restored.id);
    } catch (error) {
      $("#vm-error").textContent = error.message;
      button.disabled = false;
    }
  };
  vmDialog.showModal();
  await profiles();
}
let vmDetailBusy = false;
async function openVMDetail(id) {
  const dialog = $("#detail-dialog");
  delete dialog.dataset.captureRun;
  delete dialog.dataset.replicationRun;
  delete dialog.dataset.walReplicationRun;
  dialog.dataset.vmRun = id;
  $("#detail-content").innerHTML =
    '<p class="text-sm text-slate-500">Загрузка операции ВМ…</p>';
  if (!dialog.open) dialog.showModal();
  await refreshVMDetail();
}
async function refreshVMDetail() {
  const dialog = $("#detail-dialog"),
    id = dialog.dataset.vmRun;
  if (!dialog.open || !id || vmDetailBusy) return;
  vmDetailBusy = true;
  try {
    const [run, events] = await Promise.all([
      api(`/v1/runs/${encodeURIComponent(id)}`),
      api(`/v1/runs/${encodeURIComponent(id)}/events`),
    ]);
    if (!dialog.open || dialog.dataset.vmRun !== id) return;
    const meta = run.result?.vm,
      restored = run.result?.vm_restore;
    const phase = vmPhase(run.result?.phase);
    $("#detail-content").innerHTML =
      `<div class="flex items-center justify-between">${badge(run.status)}<span class="text-xs text-brand-600">${esc(meta?.provider || run.job.type)} · ВМ</span></div><h3 class="mb-3 mt-5 text-lg font-semibold">${esc(run.job.name)}</h3><p class="mb-5 break-all text-xs text-slate-500">${esc(run.job.source)}<br>Прокси ${esc(run.job.agent_id)} · Медиасервер ${esc(run.job.media_id)}</p>${operationProgress(run, run.result)}${phase ? `<p class="my-3 text-sm text-brand-700">${esc(phase)}</p>` : ""}${dedupPanel(run.result, run.kind)}${run.result?.replica ? `<div class="my-5 rounded-2xl bg-brand-50 p-4 text-xs"><p class="font-semibold">Самостоятельная реплика · ${esc(run.job.media_id)}</p><p class="mt-2">Хранение до ${date(run.snapshot_expires_at || run.result.replica.expires_at)}</p><p class="mt-2">Исходные данные: ${date(run.result.replica.captured_at)} · ${esc(run.result.replica.source_media_id)}</p></div>` : ""}${run.error ? `<p class="my-4 break-all text-sm text-rose-600" role="alert">${esc(run.error)}</p>` : ""}${restored ? `<div class="my-5 rounded-2xl bg-brand-50 p-4 text-sm"><p class="font-semibold">Новая ВМ: ${esc(restored.name)}</p><p class="mt-2 break-all">${esc(restored.server_id)} · ${esc(restored.status)}</p><p class="mt-2 text-xs">${restored.control_plane_verified ? "API подтвердил ВМ, диски и сети. Готовность гостевой ОС не проверена." : "Проверка API ещё не завершена."}</p></div>` : ""}${meta ? `<p class="my-4 text-xs text-slate-500">Согласованность: ${esc(meta.consistency)} · ${date(meta.snapshot_at)}<br>Исходная ВМ: ${esc(meta.server_id)}${meta.archive ? `<br>Архив VMA: ${esc(meta.archive.name)} · ${size(meta.archive.bytes)} · SHA-256` : ""}</p><div class="my-5 space-y-2">${meta.disks.map((d) => `<div class="rounded-xl border border-slate-100 p-3 text-xs"><p class="font-semibold">${esc(d.file || d.device)} · ${size(d.bytes)} · ${esc(d.disk_format)}</p><p class="mt-1 text-slate-500">${d.boot_index === 0 ? "Загрузочный диск" : "Диск данных"} · ${esc(d.device || d.volume_id || "")}</p></div>`).join("")}</div>` : ""}<h4 class="my-3 font-semibold">Журнал операции</h4><div class="max-h-64 space-y-2 overflow-y-auto text-xs">${events.map((e) => `<div class="rounded-xl bg-slate-50 p-3"><p class="font-semibold">${esc(e.action)}</p><p class="mt-1 break-all text-slate-600">${esc(e.result)}</p><p class="mt-1 text-slate-400">${date(e.at)}</p></div>`).join("") || "Событий пока нет"}</div><div class="mt-6 flex flex-wrap gap-2">${["queued", "running"].includes(run.status) ? `<button class="btn-secondary" data-cancel="${esc(id)}">Отменить</button>` : ""}${run.kind === "backup" && run.status === "success" && run.snapshot_state !== "expired" && run.snapshot_state !== "deleted" ? `<button class="btn" data-restore="${esc(id)}">Восстановить в новую ВМ</button><button class="btn-secondary" data-verify="${esc(id)}">Проверить копию</button><button class="btn-secondary" data-replicate="${esc(id)}">Реплицировать</button>` : ""}</div><p class="mt-5 break-all font-mono text-[10px] text-slate-400">ID ${esc(id)}</p>`;
  } catch (error) {
    if (dialog.open && dialog.dataset.vmRun === id) notice(error.message);
  } finally {
    vmDetailBusy = false;
  }
}
function vmPhase(phase) {
  return (
    {
      "vm-export": "Подготовка экспорта дисков платформой",
      "vm-snapshot": "Создание снимков и подготовка экспорта",
      "vm-transfer": "Передача дисков с дедупликацией",
      "vm-validation": "Проверка копии медиасервером",
      "vm-cleanup": "Очистка временных ресурсов",
      "vm-restore-transfer": "Загрузка дисков в платформу",
      "vm-restore-validation": "Создание новой ВМ",
      complete: "Операция завершена",
    }[phase] ||
    phase ||
    ""
  );
}
$("#detail-dialog").addEventListener("close", () => {
  delete $("#detail-dialog").dataset.vmRun;
});
vmDialog.addEventListener("close", () => {
  vmSelection++;
});
setInterval(refreshVMDetail, 2000);
