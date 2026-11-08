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

function confirmBox(html, yes = 'Да') {
  return new Promise(res => {
    $('#modalBody').innerHTML = html; $('#modalYes').textContent = yes;
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
  if (document.activeElement !== $('#sku1')) $('#sku1').value = p.sku1 || '';
  if (document.activeElement !== $('#sku2')) $('#sku2').value = p.sku2 || '';
  if (document.activeElement !== $('#stockQty')) $('#stockQty').value = p.stock_qty ?? '';
  const ws = $('#whSel');
  if (!ws.options.length || ws.dataset.shop !== p.id) {
    ws.dataset.shop = p.id;
    ws.innerHTML = p.warehouse_id ? `<option value="${p.warehouse_id}">${esc(p.warehouse_name || p.warehouse_id)}</option>` : '<option value="">— нажмите ↻, чтобы загрузить склады —</option>';
  }
  $('#sName').value = p.name; $('#sClient').value = p.client_id || '';
  $('#sKey').value = ''; $('#sKey').placeholder = p.api_key_set ? 'сохранён (введите новый, чтобы заменить)' : '';
  $('#sName').disabled = false; $('#sClient').disabled = $('#sKey').disabled = p.demo;
  $('#pPause').value = p.pause_sec; $('#pMargin').value = p.margin_min; $('#pPoll').value = p.poll_sec; $('#pLook').value = p.lookback_days;
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

async function loadProducts() {
  const p = S.profile, box = $('#prodInfo');
  if (!p.sku1 && !p.sku2) { box.innerHTML = ''; return; }
  if (!p.demo && !(p.client_id && p.api_key_set)) { box.innerHTML = ''; return; }
  try {
    const j = await api('/api/products?id=' + p.id);
    box.innerHTML = j.products.map(x => x.missing
      ? `<div class="prod miss"><div class="t">SKU ${x.sku}</div>Товар не найден в магазине</div>`
      : `<div class="prod"><div class="t">${esc(x.name)}</div><div class="muted small">SKU ${x.sku} · артикул ${esc(x.offer_id)} · product_id ${x.product_id}</div>
         <span class="stk">${x.present != null ? `На складе: <b>${x.present}</b> шт. · в резерве: ${x.reserved}` : 'Остаток на выбранном складе: нет данных'}</span></div>`).join('')
      + (j.stock_error ? `<div class="muted small">Остатки не получены: ${esc(j.stock_error)}</div>` : '');
  } catch (e) { box.innerHTML = `<div class="prod miss">${esc(e.message)}</div>`; }
}

async function saveCfg() {
  const ws = $('#whSel'), opt = ws.selectedOptions[0];
  const body = { id: S.profile.id, sku1: $('#sku1').value.trim(), sku2: $('#sku2').value.trim() };
  if (ws.value) { body.warehouse_id = ws.value; body.warehouse_name = opt ? opt.textContent.split(' · ')[0] : ''; }
  try {
    await api('/api/profile/save', body);
    $('#cfgMsg').textContent = 'Сохранено'; setTimeout(() => $('#cfgMsg').textContent = '', 2500);
    await loadState(); loadProducts();
  } catch (e) { toast(e.message, true); }
}

async function applyStock() {
  const p = S.profile, qty = $('#stockQty').value.trim();
  if (qty === '') return toast('Укажите количество', true);
  if (p.live && !(await confirmBox(`<h3>Выставить остатки?</h3><p>Обоим SKU (${p.sku1} и ${p.sku2}) на складе «${esc(p.warehouse_name)}» будет выставлено <b>${qty} шт.</b> Это изменит остатки в Ozon.</p>`, 'Выставить'))) return;
  try {
    const j = await api('/api/stock', { id: p.id, qty });
    $('#stockMsg').innerHTML = j.messages.map(m => `<div class="${m.startsWith('[ТЕСТ]') ? 'test' : 'ok'}">${esc(m)}</div>`).join('');
    loadState(); setTimeout(loadProducts, 800);
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
  if (v.done[i] && i <= v.stage) {
    if (!t || t.startsWith('0001')) return '<span class="st done">✓</span>';
    const simu = !S.profile.live;
    return `<span class="st ${simu ? 'simu' : 'done'}" title="${simu ? 'выполнено в тесте' : 'выполнено'}">✓ ${fmt(t)}</span>`;
  }
  if (v.final) return '<span class="muted">—</span>';
  if (!t || t.startsWith('0001')) return '<span class="muted">—</span>';
  return `<span class="st plan">~ ${fmt(t)}</span>`;
}

function renderRun() {
  const p = S.profile;
  const btn = $('#runBtn'), on = run && run.running;
  btn.textContent = on ? 'Остановить' : 'Запустить';
  btn.className = 'btn lg ' + (on ? 'stop' : 'primary');
  $('#runDot').className = 'dot' + (on ? ' on' : '') + (on && !p.live ? ' test' : '');
  let info = on ? `Работает с ${fmt(run.started, true)} · последняя проверка ${fmt(run.last_poll)} · следующая ${rel(run.next_poll)}` : 'Остановлена';
  if (on && run.last_error) info += `<div class="e">Ошибка: ${esc(run.last_error)}</div>`;
  $('#runInfo').innerHTML = info;

  const rows = (run && run.postings) || [];
  $('#ordersEmpty').classList.toggle('hidden', rows.length > 0);
  $('#ordersEmpty').textContent = on ? 'Пока нет целевых заказов.' : 'Запустите автоматизацию, чтобы увидеть заказы.';
  $('#ordersTable tbody').innerHTML = rows.map(v => {
    let now;
    if (v.final) now = `<span class="chip ${v.final.includes('доставлен') ? 'ok' : 'warn'}">${esc(v.final)}</span>`;
    else if (v.next_action) now = `<span class="chip next">${esc(v.next_action)}</span> <span class="muted small">${v.next_at && !v.next_at.startsWith('0001') ? rel(v.next_at) : ''}</span>`;
    else now = '';
    const notes = (v.error ? `<div class="note err">${esc(v.error)}</div>` : '') + (v.warning ? `<div class="note warn">${esc(v.warning)}</div>` : '');
    const win = v.window_end && !v.window_end.startsWith('0001') ? `${fmt(v.window_begin, true)} – ${fmt(v.window_end, true).slice(-5)}` : '<span class="note warn">нет данных</span>';
    return `<tr class="${v.final ? 'fin' : ''}">
      <td><div class="pn">${esc(v.posting_number)}</div><div class="muted small">создан ${fmt(v.created, true)} · Ozon: ${esc(v.status)}</div></td>
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
$('#shopSel').onchange = async e => { await api('/api/profile/select', { id: e.target.value }); run = null; await loadState(); refreshRun(); refreshLog(); loadProducts(); };
$('#quitBtn').onclick = async () => {
  const anyRun = S.shops.some(s => s.running);
  if (!(await confirmBox(`<h3>Закрыть программу?</h3>${anyRun ? '<p>Автоматизация будет приостановлена до следующего запуска программы. Пока программа закрыта, статусы заказов не меняются.</p>' : ''}`, 'Закрыть'))) return;
  await api('/api/quit', {}).catch(() => { });
  document.body.innerHTML = '<main><div class="card" style="margin-top:40px"><h2>Программа закрыта</h2><p class="muted">Эту вкладку можно закрыть.</p></div></main>';
};
$('#whReload').onclick = e => { e.preventDefault(); loadWarehouses(); };
$('#cfgSave').onclick = saveCfg;
$('#stockApply').onclick = applyStock;
$('#stockReload').onclick = loadProducts;
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
    await api('/api/profile/save', { id: S.profile.id, pause_sec: $('#pPause').value, margin_min: $('#pMargin').value, poll_sec: $('#pPoll').value, lookback_days: $('#pLook').value });
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
$('#dOpen').onclick = () => api('/api/open-dir', {});

(async () => {
  await loadState();
  refreshRun(); refreshLog(); loadProducts();
  setInterval(refreshRun, 3000);
  setInterval(refreshLog, 3000);
  setInterval(() => loadState(false).catch(() => { }), 15000);
})();
