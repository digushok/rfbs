'use strict';
const $ = (s, el = document) => el.querySelector(s);
const $$ = (s, el = document) => [...el.querySelectorAll(s)];
const esc = s => String(s ?? '').replace(/[&<>"']/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));

let S = { profile: null, shops: [], active: '' };
let run = null;
let logSeq = 0, logShop = '', logLines = [];

async function api(path, body) {
  const opt = body === undefined ? {} : { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) };
  const r = await fetch(path, opt);
  const j = await r.json().catch(() => ({}));
  if (!r.ok) throw new Error(j.error || ('Ошибка ' + r.status));
  return j;
}

function toast(msg, err) {
  const t = $('#toast');
  t.textContent = msg; t.className = 'toast' + (err ? ' err' : '');
  clearTimeout(toast.t); toast.t = setTimeout(() => t.classList.add('hidden'), err ? 6000 : 3000);
}

function confirmBox(html, yes = 'Да', danger = false) {
  return new Promise(res => {
    $('#modalBody').innerHTML = html; $('#modalYes').textContent = yes;
    $('#modalYes').className = 'btn ' + (danger ? 'danger' : 'primary');
    $('#modal').classList.remove('hidden');
    const done = v => { $('#modal').classList.add('hidden'); $('#modalYes').onclick = $('#modalNo').onclick = null; res(v); };
    $('#modalYes').onclick = () => done(true); $('#modalNo').onclick = () => done(false);
  });
}

const pad = n => String(n).padStart(2, '0');
function fmt(t, withDate) {
  if (!t || t.startsWith('0001')) return '';
  const d = new Date(t), now = new Date();
  const hm = pad(d.getHours()) + ':' + pad(d.getMinutes());
  if (!withDate && d.toDateString() === now.toDateString()) return hm;
  return pad(d.getDate()) + '.' + pad(d.getMonth() + 1) + ' ' + hm;
}
function rel(t) {
  if (!t || t.startsWith('0001')) return '';
  let s = Math.round((new Date(t) - Date.now()) / 1000);
  const past = s < 0; s = Math.abs(s);
  const txt = s < 60 ? s + ' сек' : s < 3600 ? Math.round(s / 60) + ' мин' : Math.floor(s / 3600) + ' ч ' + Math.round((s % 3600) / 60) + ' мин';
  return past ? txt + ' назад' : 'через ' + txt;
}

// ---------------- состояние ----------------
async function loadState(full = true) {
  const j = await api('/api/state');
  S = j;
  const sel = $('#shopSel');
  sel.innerHTML = j.shops.map(s => `<option value="${s.id}">${esc(s.name)}${s.demo ? ' (демо)' : ''}${s.running ? ' ●' : ''}</option>`).join('');
  sel.value = j.active;
  $('#verFoot').textContent = 'Ozon rFBS · версия ' + j.version;
  $('#dataDir').textContent = j.data_dir;
  $('#aboutSub').textContent = 'Версия ' + j.version + ' · обновления выходят на GitHub в разделе Releases';
  $('#relLink').href = j.repo + '/releases';
  renderProfile(full);
  renderShops();
}

function renderProfile(full = true) {
  const p = S.profile; if (!p) return;
  $('#demoNote').classList.toggle('hidden', !p.demo);
  $('#keyNote').classList.toggle('hidden', p.demo || (p.client_id && p.api_key_set));
  $('#testNote').classList.toggle('hidden', p.live);
  $('#liveNote').classList.toggle('hidden', !p.live);
  $('#demoBtns').classList.toggle('hidden', !p.demo);
  $('#testReset').classList.toggle('hidden', p.live);
  $$('#modeSeg input').forEach(i => i.checked = (i.value === 'live') === p.live);
  if (!full) return;
  const ws = $('#whSel');
  if (!ws.options.length || ws.dataset.shop !== p.id) {
    ws.dataset.shop = p.id;
    ws.innerHTML = p.warehouse_id ? `<option value="${p.warehouse_id}">${esc(p.warehouse_name || p.warehouse_id)}</option>` : '<option value="">— нажмите ↻, чтобы загрузить склады —</option>';
  }
  $('#sName').value = p.name; $('#sClient').value = p.client_id || '';
  $('#sKey').value = ''; $('#sKey').placeholder = p.api_key_set ? 'сохранён (введите новый, чтобы заменить)' : '';
  $('#sName').disabled = false; $('#sClient').disabled = $('#sKey').disabled = p.demo;
  $('#pPause').value = p.pause_sec; $('#pMargin').value = p.margin_min; $('#pPoll').value = p.poll_sec; $('#pLook').value = p.lookback_days;
  $('#pPrestock').value = p.prestock_left ?? 3; $('#pZeroOld').checked = !p.keep_old_stock; $('#pZeroReset').checked = !p.keep_on_reset;
  renderTemplates();
}

function renderShops() {
  $('#sShops').innerHTML = S.shops.map(s => `<div class="shoprow">
    <span class="dot ${s.running ? 'on' : ''} ${s.running && !s.live ? 'test' : ''}"></span>
    <span class="n">${esc(s.name)}</span>
    ${s.demo ? '<span class="chip">демо</span>' : ''}
    <span class="chip ${s.live ? 'err' : ''}">${s.live ? 'боевой' : 'тест'}</span>
    <span class="muted small">${s.running ? 'автоматизация работает' : 'остановлена'}</span>
    <span class="a">${s.id === S.active ? '<span class="chip next">выбран</span>' : `<button class="btn ghost sm" data-sel="${s.id}">Выбрать</button>`}
    <button class="btn danger sm" data-del="${s.id}">Удалить</button></span></div>`).join('');
}

// ---------------- товары, склад, остатки ----------------
async function loadWarehouses() {
  const ws = $('#whSel'), p = S.profile;
  try {
    const j = await api('/api/warehouses?id=' + p.id);
    ws.innerHTML = '<option value="">— выберите склад —</option>' + j.warehouses.map(w =>
      `<option value="${w.id}" ${w.id == p.warehouse_id ? 'selected' : ''}>${esc(w.name)}${w.is_rfbs ? ' · rFBS' : ''} · ${w.id}</option>`).join('');
    toast('Складов загружено: ' + j.warehouses.length);
  } catch (e) { toast(e.message, true); }
}

// ---------------- партии ----------------
let prodInfo = {};          // sku → товар из Ozon
let queueEdit = null;       // редактируемая очередь (пока не сохранена)

function batchSource() {
  // При работающей автоматизации — её состояние (в тестовом режиме там симуляция), иначе — настройки.
  if (run && run.running && run.batches) return run.batches;
  return ((S.profile && S.profile.batches) || []).map((b, i) => ({ ...b, num: i + 1, in_work: 0, delivered: 0 }));
}

function renderBatches() {
  const tb = $('#batchTable tbody');
  if (tb.contains(document.activeElement)) return; // не мешаем вводу
  const src = batchSource();
  const started = src.filter(b => b.state !== 'queued');
  const queued = queueEdit || src.filter(b => b.state === 'queued').map(b => ({ id: b.id, sku1: b.sku1, sku2: b.sku2, qty: b.qty }));
  const name = sku => {
    const x = prodInfo[sku];
    if (!x) return '';
    return x.missing ? '<div class="pname miss">не найден в магазине</div>' : `<div class="pname" title="${esc(x.name)}">${esc(x.name)}</div>`;
  };
  const stock = sku => { const x = prodInfo[sku]; return x && x.present != null ? ` · на складе ${x.present}, резерв ${x.reserved}` : ''; };
  let n = 0;
  const rows = started.map(b => {
    n++;
    const pct = b.qty > 0 ? Math.min(100, Math.round(b.sold / b.qty * 100)) : 0;
    const st = b.state === 'next'
      ? `<span class="chip ok">продаётся (заранее)</span> продано <b>${b.sold}</b> из ${b.qty}<div class="muted small">выставлена, пока распродаётся текущая · в работе ${b.in_work}${stock(b.sku1)}</div>`
      : b.state === 'active'
      ? `<span class="chip next">продаётся</span> продано <b>${b.sold}</b> из ${b.qty}<div class="bar"><i style="width:${pct}%"></i></div><div class="muted small">в работе ${b.in_work}, доставлено ${b.delivered}${stock(b.sku1)}</div>`
      : `<span class="chip ${b.in_work ? 'warn' : 'ok'}">${b.in_work ? 'доставляется' : 'завершена'}</span> продано ${b.sold} из ${b.qty}<div class="muted small">в работе ${b.in_work}, доставлено ${b.delivered}</div>`;
    const act = b.state === 'done' && !b.in_work ? `<button class="btn ghost sm" data-rm="${b.id}" title="Убрать из списка. Её заказы всё равно будут доведены до конца.">Убрать</button>` : '';
    return `<tr class="${b.state}" data-keep="${b.id}"><td class="bn">${n}</td><td>${b.sku1}${name(b.sku1)}</td><td>${b.sku2}${name(b.sku2)}</td><td class="r">${b.qty}</td><td>${st}</td><td class="r">${act}</td></tr>`;
  }).concat(queued.map((b, i) => {
    n++;
    return `<tr class="queued" data-qi="${i}"><td class="bn">${n}</td>
      <td><input data-f="sku1" inputmode="numeric" value="${esc(b.sku1 || '')}" placeholder="SKU Ozon">${name(b.sku1)}</td>
      <td><input data-f="sku2" inputmode="numeric" value="${esc(b.sku2 || '')}" placeholder="SKU Ozon">${name(b.sku2)}</td>
      <td class="r"><input data-f="qty" class="qty" type="number" min="1" value="${esc(b.qty || '')}"></td>
      <td><span class="chip">в очереди</span></td>
      <td><div class="icons"><button class="btn ghost sm" data-up="${i}" title="Выше">↑</button><button class="btn ghost sm" data-down="${i}" title="Ниже">↓</button><button class="btn ghost sm" data-del="${i}" title="Удалить">✕</button></div></td></tr>`;
  }));
  tb.innerHTML = rows.join('') || '<tr><td colspan="6" class="muted">Партий нет — нажмите «+ Партия»</td></tr>';
  $('#batchSave').classList.toggle('hidden', !queueEdit);
  $('#batchDirty').classList.toggle('hidden', !queueEdit);
}

function editQueue() {
  if (!queueEdit) queueEdit = batchSource().filter(b => b.state === 'queued').map(b => ({ id: b.id, sku1: b.sku1, sku2: b.sku2, qty: b.qty }));
  return queueEdit;
}

async function loadProducts() {
  const p = S.profile;
  if (!p || !(p.batches || []).length) { prodInfo = {}; renderBatches(); return; }
  if (!p.demo && !(p.client_id && p.api_key_set)) { renderBatches(); return; }
  try {
    const j = await api('/api/products?id=' + p.id);
    prodInfo = {};
    j.products.forEach(x => prodInfo[x.sku] = x);
  } catch (e) { toast('Товары: ' + e.message, true); }
  renderBatches();
}

async function saveCfg() {
  const ws = $('#whSel'), opt = ws.selectedOptions[0];
  if (!ws.value) return toast('Выберите склад', true);
  try {
    await api('/api/profile/save', { id: S.profile.id, warehouse_id: ws.value, warehouse_name: opt ? opt.textContent.split(' · ')[0] : '' });
    $('#cfgMsg').textContent = 'Сохранено'; setTimeout(() => $('#cfgMsg').textContent = '', 2500);
    await loadState(); loadProducts();
  } catch (e) { toast(e.message, true); }
}

async function saveBatches() {
  const keep = $$('#batchTable tr[data-keep]').map(tr => tr.dataset.keep);
  try {
    await api('/api/batches/save', { id: S.profile.id, keep_ids: keep, batches: queueEdit || [] });
    queueEdit = null;
    document.activeElement.blur();
    toast('Очередь партий сохранена');
    await loadState(); await refreshRun(); loadProducts();
  } catch (e) { toast(e.message, true); }
}

async function applyStock() {
  const p = S.profile, a = batchSource().find(b => b.state === 'active');
  if (!a) return toast('Нет активной партии — запустите автоматизацию, она выставит остаток первой партии сама', true);
  if (p.live && !(await confirmBox(`<h3>Выставить остаток заново?</h3><p>SKU ${a.sku1} и ${a.sku2} на складе «${esc(p.warehouse_name)}» будет выставлено по <b>${a.qty} шт.</b> Это изменит остатки в Ozon.</p>`, 'Выставить'))) return;
  try {
    const j = await api('/api/stock', { id: p.id, qty: a.qty });
    $('#stockMsg').innerHTML = j.messages.map(m => `<div class="${m.startsWith('[ТЕСТ]') ? 'test' : 'ok'}">${esc(m)}</div>`).join('');
    setTimeout(loadProducts, 800);
  } catch (e) { $('#stockMsg').innerHTML = `<div class="err">${esc(e.message)}</div>`; }
}

// ---------------- автоматизация ----------------
async function setMode(live) {
  const p = S.profile;
  if (live && !(await confirmBox(`<h3>Включить боевой режим?</h3><p>Программа начнёт <b>по-настоящему</b> менять статусы целевых заказов в Ozon: «Собрать» → «Доставляется» → «Курьер в пути» → «Условно доставлен». Кнопка «Выставить остатки» тоже будет менять остатки.</p><p class="muted">Рекомендуем сначала проверить работу в тестовом режиме.</p>`, 'Включить'))) {
    renderProfile(); return;
  }
  try { await api('/api/profile/save', { id: p.id, live }); await loadState(); refreshRun(); } catch (e) { toast(e.message, true); }
}

async function toggleRun() {
  const p = S.profile;
  try {
    if (run && run.running) { await api('/api/run/stop', { id: p.id }); toast('Автоматизация остановлена'); }
    else {
      if (p.live && !(await confirmBox('<h3>Запустить в боевом режиме?</h3><p>Целевые заказы будут переводиться по статусам в Ozon автоматически.</p>', 'Запустить'))) return;
      await api('/api/run/start', { id: p.id }); toast('Автоматизация запущена');
    }
    await loadState(); setTimeout(refreshRun, 400);
  } catch (e) { toast(e.message, true); }
}

function stepCell(v, i) {
  const t = v.plan[i];
  if (v.manual && v.manual[i] && i <= v.stage) return '<span class="st manual" title="Этап нажат вручную, не программой">✓ вручную</span>';
  if (v.done[i] && i <= v.stage) {
    if (!t || t.startsWith('0001')) return '<span class="st done">✓</span>';
    const simu = !S.profile.live;
    return `<span class="st ${simu ? 'simu' : 'done'}" title="${simu ? 'выполнено в тесте' : 'выполнено'}">✓ ${fmt(t)}</span>`;
  }
  if (v.final) return '<span class="muted">—</span>';
  if (!t || t.startsWith('0001')) return '<span class="muted">—</span>';
  return `<span class="st plan">~ ${fmt(t)}</span>`;
}

let lastActive = '';
function renderRun() {
  const p = S.profile;
  const actB = ((run && run.batches) || []).find(b => b.state === 'active');
  const key = actB ? actB.id + ':' + actB.sold : '';
  if (key !== lastActive) { lastActive = key; if (run && run.running) loadProducts(); }
  renderBatches();
  const btn = $('#runBtn'), on = run && run.running;
  btn.textContent = on ? 'Остановить' : 'Запустить';
  btn.className = 'btn lg ' + (on ? 'stop' : 'primary');
  $('#runDot').className = 'dot' + (on ? ' on' : '') + (on && !p.live ? ' test' : '');
  let info = on ? `Работает с ${fmt(run.started, true)} · последняя проверка ${fmt(run.last_poll)} · следующая ${rel(run.next_poll)}` : 'Остановлена';
  if (on && run.last_error) info += `<div class="e">Ошибка: ${esc(run.last_error)}</div>`;
  $('#runInfo').innerHTML = info;

  const rows = (run && run.postings) || [];
  $('#ordersEmpty').classList.toggle('hidden', rows.length > 0);
  const act = rows.filter(v => !v.final).length;
  $('#ordersCount').textContent = rows.length ? `· в работе ${act}, завершено ${rows.length - act}` : '';
  $('#ordersTable').closest('.tablebox').classList.toggle('hidden', rows.length === 0);
  $('#ordersEmpty').textContent = on ? 'Пока нет целевых заказов.' : 'Запустите автоматизацию, чтобы увидеть заказы.';
  $('#ordersTable tbody').innerHTML = rows.map(v => {
    let now;
    if (v.final) now = `<span class="chip ${v.final.includes('доставлен') ? 'ok' : 'warn'}">${esc(v.final)}</span>`;
    else if (v.next_action) now = `<span class="chip next">${esc(v.next_action)}</span> <span class="muted small">${v.next_at && !v.next_at.startsWith('0001') ? rel(v.next_at) : ''}</span>`;
    else now = '';
    const notes = (v.error ? `<div class="note err">${esc(v.error)}</div>` : '') + (v.warning ? `<div class="note warn">${esc(v.warning)}</div>` : '');
    const win = v.window_end && !v.window_end.startsWith('0001') ? `${fmt(v.window_begin, true)} – ${fmt(v.window_end, true).slice(-5)}` : '<span class="note warn">нет данных</span>';
    return `<tr class="${v.final ? 'fin' : ''}">
      <td><div class="pn">${esc(v.posting_number)}${v.picked ? ' <span class="chip warn" title="Часть этапов была нажата вручную — программа подхватила заказ и ведёт его дальше">подхвачен</span>' : ''}${v.no_count ? ' <span class="chip" title="Пара из партии в очереди — в «продано» не считается">вне счёта</span>' : ''}</div><div class="muted small">${v.batch ? `партия ${v.batch} · ` : ''}создан ${fmt(v.created, true)} · Ozon: ${esc(v.status)}${v.substatus ? ' / ' + esc(v.substatus) : ''}</div></td>
      <td class="st">${win}</td>
      ${[1, 2, 3, 4].map(i => `<td>${stepCell(v, i)}</td>`).join('')}
      <td>${now}${notes}</td></tr>`;
  }).join('');

  const sk = (run && run.skipped) || [];
  $('#skipCount').textContent = sk.length ? `(${sk.length})` : '';
  $('#skipTable tbody').innerHTML = sk.map(s => `<tr><td class="pn">${esc(s.posting_number)}</td><td class="st">${fmt(s.created, true)}</td><td>${esc(s.products)}</td><td>${esc(s.status)}</td><td>${esc(s.reason)}</td></tr>`).join('')
    || '<tr><td colspan="5" class="muted">Нет</td></tr>';
}

async function refreshRun() {
  if (!S.profile) return;
  try { run = await api('/api/run?id=' + S.profile.id); renderRun(); } catch (e) { /* программа закрыта */ }
}

// ---------------- журнал ----------------
async function refreshLog() {
  if (!S.profile) return;
  if (logShop !== S.profile.id) { logShop = S.profile.id; logSeq = 0; logLines = []; $('#logList').innerHTML = ''; }
  try {
    const j = await api(`/api/log?id=${S.profile.id}&after=${logSeq}`);
    if (!j.lines.length) return;
    logSeq = j.lines[j.lines.length - 1].seq;
    logLines.push(...j.lines);
    if (logLines.length > 2000) logLines = logLines.slice(-1500);
    renderLog();
  } catch (e) { }
}
function renderLog() {
  const f = $('#logFilter input:checked').value, box = $('#logList');
  const atBottom = box.scrollHeight - box.scrollTop - box.clientHeight < 40;
  const lines = logLines.filter(l => !f || (f === 'err' ? (l.level === 'err' || l.level === 'warn') : (l.level === 'ok' || l.level === 'test' || l.level === 'err')));
  box.innerHTML = lines.map(l => `<div class="${l.level}"><span class="tm">${fmt(l.time, true)}:${pad(new Date(l.time).getSeconds())}</span>${esc(l.text)}</div>`).join('') || '<span class="muted">Записей пока нет</span>';
  if (atBottom) box.scrollTop = box.scrollHeight;
}

// ---------------- события ----------------
$$('.tabs button').forEach(b => b.onclick = () => {
  $$('.tabs button').forEach(x => x.classList.toggle('on', x === b));
  $$('.tab').forEach(t => t.classList.toggle('hidden', t.id !== 'tab-' + b.dataset.tab));
  if (b.dataset.tab === 'log') { renderLog(); const box = $('#logList'); box.scrollTop = box.scrollHeight; }
});
$('#shopSel').onchange = async e => { await api('/api/profile/select', { id: e.target.value }); run = null; queueEdit = null; tplEdit = {}; await loadState(); refreshRun(); refreshLog(); loadProducts(); };
$('#quitBtn').onclick = async () => {
  const anyRun = S.shops.some(s => s.running);
  if (!(await confirmBox(`<h3>Закрыть программу?</h3>${anyRun ? '<p>Автоматизация будет приостановлена до следующего запуска программы. Пока программа закрыта, статусы заказов не меняются.</p>' : ''}`, 'Закрыть'))) return;
  await api('/api/quit', {}).catch(() => { });
  document.body.innerHTML = '<main><div class="card" style="margin-top:40px"><h2>Программа закрыта</h2><p class="muted">Эту вкладку можно закрыть.</p></div></main>';
};
$('#whReload').onclick = e => { e.preventDefault(); loadWarehouses(); };
$('#cfgSave').onclick = saveCfg;
$('#stockApply').onclick = applyStock;
$('#batchSave').onclick = saveBatches;
$('#batchAdd').onclick = () => {
  editQueue().push({ sku1: '', sku2: '', qty: '' });
  if (document.activeElement) document.activeElement.blur();
  renderBatches();
  const rows = $$('#batchTable tr.queued');
  if (rows.length) $('input', rows[rows.length - 1]).focus();
};
$('#batchTable').addEventListener('input', e => {
  const tr = e.target.closest('tr.queued'); if (!tr) return;
  editQueue()[+tr.dataset.qi][e.target.dataset.f] = e.target.value.trim();
  $('#batchSave').classList.remove('hidden'); $('#batchDirty').classList.remove('hidden');
});
$('#batchTable').addEventListener('click', async e => {
  const d = e.target.dataset;
  if (d.up !== undefined || d.down !== undefined || d.del !== undefined) {
    const q = editQueue(), i = +(d.up ?? d.down ?? d.del);
    if (d.del !== undefined) q.splice(i, 1);
    else { const j = d.up !== undefined ? i - 1 : i + 1; if (j >= 0 && j < q.length) [q[i], q[j]] = [q[j], q[i]]; }
    if (document.activeElement) document.activeElement.blur();
    renderBatches();
  }
  if (d.rm) {
    if (!(await confirmBox('<h3>Убрать партию из списка?</h3><p>Её заказы, которые уже в работе, всё равно будут доведены до «Условно доставлен».</p>', 'Убрать'))) return;
    try { await api('/api/batches/remove', { id: S.profile.id, batch_id: d.rm }); await loadState(); await refreshRun(); } catch (er) { toast(er.message, true); }
  }
});
$('#batchNext').onclick = async () => {
  const src = batchSource(), a = src.find(b => b.state === 'active'), e = src.find(b => b.state === 'next'), q = src.find(b => b.state === 'queued');
  const msg = a ? `Партия ${a.num} (SKU ${a.sku1} + ${a.sku2}) будет завершена${S.profile.keep_old_stock ? '' : ', её остаток обнулён'}. ` : '';
  const nx = e ? `Основной станет партия ${e.num} (SKU ${e.sku1} + ${e.sku2}) — она уже в наличии.`
    : q ? `Запустится партия ${q.num} (SKU ${q.sku1} + ${q.sku2}): остаток по ${q.qty} шт.` : 'В очереди нет партий — новая не запустится.';
  if (!(await confirmBox(`<h3>Перейти к следующей партии?</h3><p>${msg}${nx}</p><p class="muted">Заказы завершённой партии будут доведены до конца.</p>`, 'Перейти'))) return;
  try { await api('/api/batches/next', { id: S.profile.id }); toast('Переключаю…'); setTimeout(refreshRun, 1500); } catch (er) { toast(er.message, true); }
};

$$('#modeSeg input').forEach(i => i.onchange = () => setMode(i.value === 'live'));
$('#runBtn').onclick = toggleRun;
$('#pollNow').onclick = async () => { await api('/api/run/now', { id: S.profile.id }); setTimeout(refreshRun, 1500); };
$('#testReset').onclick = async () => {
  if (!(await confirmBox('<h3>Сбросить тестовый прогон?</h3><p>Программа забудет симулированные шаги и начнёт тест с текущих статусов Ozon.</p>', 'Сбросить'))) return;
  try { await api('/api/test/reset', { id: S.profile.id }); toast('Тестовый прогон сброшен'); setTimeout(refreshRun, 800); } catch (e) { toast(e.message, true); }
};
$$('#demoBtns button').forEach(b => b.onclick = async () => {
  try { const j = await api('/api/demo/order', { id: S.profile.id, kind: b.dataset.kind }); toast('Создан демо-заказ ' + j.posting_number); setTimeout(refreshRun, 1000); } catch (e) { toast(e.message, true); }
});
$$('#logFilter input').forEach(i => i.onchange = renderLog);

$('#sSave').onclick = async () => {
  const body = { id: S.profile.id, name: $('#sName').value, client_id: $('#sClient').value };
  if ($('#sKey').value.trim()) body.api_key = $('#sKey').value.trim();
  try { await api('/api/profile/save', body); $('#sMsg').textContent = 'Сохранено'; await loadState(); } catch (e) { toast(e.message, true); }
};
$('#sCheck').onclick = async () => {
  $('#sMsg').textContent = 'Проверяю…';
  try {
    const j = await api('/api/check', { id: S.profile.id });
    $('#sMsg').innerHTML = j.missing && j.missing.length
      ? `<span class="chip warn">Подключение есть, но ключу не хватает методов: ${esc(j.missing.join(', '))}</span>`
      : `<span class="chip ok">Подключение работает${j.roles && j.roles.length ? ' · роли: ' + esc(j.roles.join(', ')) : ''}</span>`;
  } catch (e) { $('#sMsg').innerHTML = `<span class="chip err">${esc(e.message)}</span>`; }
};
$('#pSave').onclick = async () => {
  try {
    await api('/api/profile/save', { id: S.profile.id, pause_sec: $('#pPause').value, margin_min: $('#pMargin').value, poll_sec: $('#pPoll').value, lookback_days: $('#pLook').value,
      prestock_left: $('#pPrestock').value, keep_old_stock: !$('#pZeroOld').checked, keep_on_reset: !$('#pZeroReset').checked });
    $('#pMsg').textContent = 'Сохранено'; setTimeout(() => $('#pMsg').textContent = '', 2500); await loadState();
  } catch (e) { toast(e.message, true); }
};
$('#shopNew').onclick = async () => { await api('/api/profile/new', { demo: false }); await loadState(); $$('.tabs button')[2].click(); toast('Создан новый магазин — заполните ключи'); };
$('#shopDemo').onclick = async () => { await api('/api/profile/new', { demo: true }); await loadState(); refreshRun(); };
$('#sShops').onclick = async e => {
  const sel = e.target.dataset.sel, del = e.target.dataset.del;
  if (sel) { await api('/api/profile/select', { id: sel }); await loadState(); refreshRun(); refreshLog(); loadProducts(); }
  if (del) {
    const s = S.shops.find(x => x.id === del);
    if (!(await confirmBox(`<h3>Удалить магазин «${esc(s.name)}»?</h3><p>Ключи и настройки этого магазина будут удалены с компьютера.</p>`, 'Удалить'))) return;
    await api('/api/profile/delete', { id: del }); await loadState(); refreshRun(); refreshLog();
  }
};
$('#updCheck').onclick = async () => {
  const box = $('#updRes'); box.innerHTML = '<div>Проверяю…</div>';
  try {
    const j = await api('/api/update');
    box.innerHTML = j.newer
      ? `<div class="ok">Доступна версия <b>${esc(j.latest)}</b> (у вас ${esc(j.current)}). ${j.download ? `<a href="${esc(j.download)}">Скачать ${esc(j.download.split('/').pop())}</a> · ` : ''}<a href="${esc(j.url)}" target="_blank" rel="noopener">что нового</a><br><span class="small">Закройте программу (⏻), удалите старый .exe и запустите новый. Настройки сохранятся.</span></div>`
      : `<div>У вас последняя версия (${esc(j.current)}).</div>`;
  } catch (e) { box.innerHTML = `<div class="err">${esc(e.message)}</div>`; }
};
// ---------------- сохранённые очереди и сброс ----------------
let tplEdit = {}; // id → {name, rows} — правки, ещё не сохранённые

function promptBox(html, value, yes = 'Сохранить') {
  return new Promise(res => {
    $('#modalBody').innerHTML = html + `<input id="promptIn" class="full" value="${esc(value || '')}">`;
    $('#modalYes').textContent = yes; $('#modalYes').className = 'btn primary';
    $('#modal').classList.remove('hidden');
    const inp = $('#promptIn'); inp.focus(); inp.select();
    const done = v => { $('#modal').classList.add('hidden'); $('#modalYes').onclick = $('#modalNo').onclick = inp.onkeydown = null; res(v); };
    $('#modalYes').onclick = () => done(inp.value.trim());
    $('#modalNo').onclick = () => done(null);
    inp.onkeydown = e => { if (e.key === 'Enter') done(inp.value.trim()); if (e.key === 'Escape') done(null); };
  });
}

function templates() { return (S.profile && S.profile.templates) || []; }

function renderTplSelect() {
  const sel = $('#tplSel'), cur = sel.value, list = templates();
  sel.innerHTML = list.length
    ? '<option value="">— выберите сохранённую очередь —</option>' + list.map(t => `<option value="${t.id}">${esc(t.name)} · пар: ${t.rows.length}</option>`).join('')
    : '<option value="">Сохранённых очередей нет — создайте в «Настройках»</option>';
  if (list.some(t => t.id === cur)) sel.value = cur;
}

function renderTemplates() {
  renderTplSelect();
  const box = $('#tplList');
  if (box.contains(document.activeElement)) return;
  const list = templates().map(t => tplEdit[t.id] ? { id: t.id, ...tplEdit[t.id], dirty: true } : { id: t.id, name: t.name, rows: t.rows.map(r => ({ ...r })) });
  if (tplEdit.new) list.push({ id: 'new', ...tplEdit.new, dirty: true });
  box.innerHTML = list.map(t => `<div class="tpl" data-tid="${t.id}">
      <div class="row tight tpl-h">
        <input class="tname" data-tf="name" value="${esc(t.name)}" placeholder="Имя очереди, например «Чехлы весна»">
        <span class="muted small">пар: ${t.rows.length}</span>
        ${t.dirty ? '<span class="chip warn">не сохранено</span>' : ''}
        <span class="grow"></span>
        <button class="btn primary sm" data-tsave="${t.id}">Сохранить</button>
        <button class="btn danger sm" data-tdel="${t.id}">${t.id === 'new' ? 'Отмена' : 'Удалить'}</button>
      </div>
      <table class="grid tplrows"><thead><tr><th style="width:40px">№</th><th>SKU 1</th><th>SKU 2</th><th class="r" style="width:120px">Кол-во, шт.</th><th style="width:110px"></th></tr></thead><tbody>
      ${t.rows.map((r, i) => `<tr data-ri="${i}"><td class="bn">${i + 1}</td>
        <td><input data-rf="sku1" inputmode="numeric" value="${esc(r.sku1 || '')}" placeholder="SKU Ozon"></td>
        <td><input data-rf="sku2" inputmode="numeric" value="${esc(r.sku2 || '')}" placeholder="SKU Ozon"></td>
        <td class="r"><input data-rf="qty" class="qty" type="number" min="1" value="${esc(r.qty || '')}"></td>
        <td><div class="icons"><button class="btn ghost sm" data-rup="${i}">↑</button><button class="btn ghost sm" data-rdown="${i}">↓</button><button class="btn ghost sm" data-rdel="${i}">✕</button></div></td></tr>`).join('')}
      </tbody></table>
      <button class="btn ghost sm" data-radd="${t.id}">+ Пара</button>
    </div>`).join('') || '<div class="muted">Сохранённых очередей пока нет.</div>';
}

function tplState(tid) {
  if (!tplEdit[tid]) {
    const t = templates().find(x => x.id === tid);
    tplEdit[tid] = { name: t.name, rows: t.rows.map(r => ({ ...r })) };
  }
  return tplEdit[tid];
}

$('#tplNew').onclick = () => {
  tplEdit.new = tplEdit.new || { name: '', rows: [{ sku1: '', sku2: '', qty: '' }] };
  if (document.activeElement) document.activeElement.blur();
  renderTemplates();
  const inp = $('#tplList .tpl[data-tid="new"] .tname'); if (inp) inp.focus();
};

$('#tplList').addEventListener('input', e => {
  const box = e.target.closest('.tpl'); if (!box) return;
  const st = tplState(box.dataset.tid);
  if (e.target.dataset.tf) st.name = e.target.value;
  if (e.target.dataset.rf) st.rows[+e.target.closest('tr').dataset.ri][e.target.dataset.rf] = e.target.value.trim();
});

$('#tplList').addEventListener('click', async e => {
  const d = e.target.dataset, box = e.target.closest('.tpl'); if (!box) return;
  const tid = box.dataset.tid;
  const rerender = () => { if (document.activeElement) document.activeElement.blur(); renderTemplates(); };
  if (d.radd) { tplState(tid).rows.push({ sku1: '', sku2: '', qty: '' }); rerender(); const ins = $$('#tplList .tpl[data-tid="' + tid + '"] tr[data-ri] input'); if (ins.length) ins[ins.length - 3].focus(); return; }
  if (d.rup !== undefined || d.rdown !== undefined || d.rdel !== undefined) {
    const rows = tplState(tid).rows, i = +(d.rup ?? d.rdown ?? d.rdel);
    if (d.rdel !== undefined) rows.splice(i, 1);
    else { const j = d.rup !== undefined ? i - 1 : i + 1; if (j >= 0 && j < rows.length) [rows[i], rows[j]] = [rows[j], rows[i]]; }
    rerender(); return;
  }
  if (d.tsave) {
    const st = tplState(tid);
    try {
      await api('/api/templates/save', { id: S.profile.id, template: { id: tid === 'new' ? '' : tid, name: st.name.trim(), rows: st.rows } });
      delete tplEdit[tid];
      toast('Очередь «' + st.name.trim() + '» сохранена');
      if (document.activeElement) document.activeElement.blur();
      await loadState();
    } catch (er) { toast(er.message, true); }
    return;
  }
  if (d.tdel) {
    if (tid === 'new') { delete tplEdit.new; rerender(); return; }
    const t = templates().find(x => x.id === tid);
    if (!(await confirmBox(`<h3>Удалить очередь «${esc(t.name)}»?</h3><p>Уже загруженные партии не изменятся.</p>`, 'Удалить'))) return;
    try { await api('/api/templates/delete', { id: S.profile.id, template_id: tid }); delete tplEdit[tid]; await loadState(); } catch (er) { toast(er.message, true); }
  }
});

$('#tplLoad').onclick = async () => {
  const tid = $('#tplSel').value;
  if (!tid) return toast('Выберите сохранённую очередь', true);
  const t = templates().find(x => x.id === tid);
  const queued = (queueEdit || batchSource().filter(b => b.state === 'queued')).length;
  const pairs = t.rows.map((r, i) => `${i + 1}. SKU ${r.sku1} + ${r.sku2} — ${r.qty} шт.`).join('<br>');
  const html = `<h3>Загрузить очередь «${esc(t.name)}»?</h3>
    <p>${queued ? `Партии «в очереди» (${queued}) будут заменены.` : 'В очередь добавятся:'} Партии, которые уже продаются или доставляются, не изменятся.</p>
    <div class="modal-list">${pairs}</div>`;
  if (!(await confirmBox(html, 'Загрузить'))) return;
  const keep = $$('#batchTable tr[data-keep]').map(tr => tr.dataset.keep);
  try {
    await api('/api/templates/load', { id: S.profile.id, template_id: tid, keep_ids: keep });
    queueEdit = null;
    toast('Очередь «' + t.name + '» загружена');
    await loadState(); await refreshRun(); loadProducts();
  } catch (er) { toast(er.message, true); }
};

$('#tplSaveAs').onclick = async () => {
  const src = batchSource();
  const rows = src.filter(b => b.state !== 'queued').map(b => ({ sku1: b.sku1, sku2: b.sku2, qty: b.qty }))
    .concat((queueEdit || src.filter(b => b.state === 'queued')).map(b => ({ sku1: b.sku1, sku2: b.sku2, qty: b.qty })));
  if (!rows.length) return toast('Таблица партий пуста — сохранять нечего', true);
  const name = await promptBox(`<h3>Сохранить текущую очередь</h3><p class="muted">В неё войдут все ${rows.length} пар из таблицы партий с их количеством. Потом её можно выбрать в списке и загрузить одним нажатием.</p><label class="fld">Имя очереди</label>`, '');
  if (!name) return;
  try {
    await api('/api/templates/save', { id: S.profile.id, template: { name, rows } });
    toast('Очередь «' + name + '» сохранена');
    await loadState();
  } catch (er) { toast(er.message, true); }
};

$('#queueReset').onclick = async () => {
  let info;
  try { info = await api('/api/batches/reset-preview?id=' + S.profile.id); } catch (er) { return toast(er.message, true); }
  const pair = b => `SKU ${b.sku1} + ${b.sku2}`;
  let html = `<h3>Сбросить очередь?</h3><p>Все партии будут удалены из списка, автоматизация <b>выключится</b>. Чтобы снова начать работу, загрузите или добавьте очередь и нажмите «Запустить».</p>`;
  if (info.on_sale && info.on_sale.length) {
    html += info.zero
      ? `<p><b>Будет обнулён остаток в Ozon</b>${info.live ? '' : ' (тестовый режим — только в журнале)'}:<br>${info.on_sale.map(pair).join('<br>')}</p>`
      : `<p>Остаток пар в продаже <b>не обнуляется</b> (отключено в настройках) — покупатели смогут их заказывать, но программа эти заказы обрабатывать не будет:<br>${info.on_sale.map(pair).join('<br>')}</p>`;
  }
  const u = info.undelivered || [];
  if (u.length) {
    html += `<div class="banner err"><b>Внимание: эти заказы не доставлены (${u.length})!</b><br>Программа их бросит — статусы больше меняться не будут. Переведите их вручную в кабинете Ozon.</div>
      <div class="modal-list">${u.slice(0, 50).map(x => `<div><b>${esc(x.posting_number)}</b> — ${esc(x.stage)}${x.window_end && !x.window_end.startsWith('0001') ? ', окно до ' + fmt(x.window_end, true) : ''}</div>`).join('')}${u.length > 50 ? `<div class="muted">и ещё ${u.length - 50}</div>` : ''}</div>`;
  } else {
    html += '<p class="muted">Недоставленных заказов в работе нет.</p>';
  }
  if (!(await confirmBox(html, u.length ? 'Сбросить и бросить заказы' : 'Сбросить', true))) return;
  try {
    const j = await api('/api/batches/reset', { id: S.profile.id });
    queueEdit = null;
    if (j.warning) toast(j.warning, true); else toast('Очередь сброшена, автоматизация выключена');
    await loadState(); await refreshRun(); refreshLog(); loadProducts();
  } catch (er) { toast(er.message, true); }
};

$('#dOpen').onclick = () => api('/api/open-dir', {});

(async () => {
  await loadState();
  refreshRun(); refreshLog(); loadProducts();
  setInterval(refreshRun, 3000);
  setInterval(refreshLog, 3000);
  setInterval(() => loadState(false).catch(() => { }), 15000);
})();
