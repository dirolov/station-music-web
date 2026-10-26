const $ = id => document.getElementById(id);
let library = { path: '.', entries: [] };
let status = {};
let configSnapshot = {};
let configLoaded = false;
let libraryLoaded = false;
let queueLoaded = false;
let statusLoaded = false;
let libraryRequest = 0;
let queueRenderKey = '';
let connectionError = '';
let checkedAt = '';
let toastTimer;
let uploadXHR;
let pollTimer;
let pollRunning = false;
let pollAgain = false;
let transportRequest = 0;
const selected = new Set();
const pending = new Set();
const versions = { queue: 0, transport: 0, volume: 0, config: 0 };
const queueBusy = () => pending.has('queue') || pending.has('upload') || pending.has('config');

async function api(path, options = {}) {
  const response = await fetch(path, {
    method: options.method || 'GET',
    headers: options.body ? { 'Content-Type': 'application/json' } : undefined,
    body: options.body ? JSON.stringify(options.body) : undefined,
  });
  let data;
  try { data = await response.json(); } catch { data = {}; }
  if (!response.ok) {
    const err = new Error(data.error || `Ошибка сервера (HTTP ${response.status}). Повторите действие.`);
    err.code = response.status;
    throw err;
  }
  return data;
}

function safeText(text) {
  return String(text).replace(/https?:\/\/[^\s"'<>]+/g, value => {
    try { const url = new URL(value); return `${url.origin}${url.pathname}${url.search ? '?…' : ''}`; } catch { return 'адрес источника'; }
  });
}
function message(text, kind = 'info') {
  clearTimeout(toastTimer);
  $('message').className = `message ${kind}`;
  $('message').textContent = safeText(text);
  if (kind === 'info') toastTimer = setTimeout(() => { $('message').textContent = ''; }, 6000);
}
function inlineError(id, text = '') {
  $(id).textContent = safeText(text);
  $(id).hidden = !text;
}
async function operation(lane, text, task, errorId) {
  if (pending.has(lane)) return;
  pending.add(lane);
  const key = lane === 'upload' ? 'queue' : lane === 'stop' ? 'transport' : lane;
  versions[key]++;
  if (errorId) inlineError(errorId);
  message(text, 'busy');
  updateButtons();
  try { await task(); }
  catch (err) {
    message(err.message, 'error');
    if (errorId) inlineError(errorId, err.message);
    if (err.code === 409) await refreshQueue().catch(() => {});
  } finally {
    pending.delete(lane);
    versions[key]++;
    updateButtons();
    schedulePoll(0);
  }
}
function emptyState(box, title, subtitle, symbol = '♫') {
  const item = document.createElement('li');
  item.className = 'empty';
  const icon = document.createElement('span');
  icon.className = 'empty-symbol'; icon.setAttribute('aria-hidden', 'true'); icon.textContent = symbol;
  const heading = document.createElement('strong'); heading.textContent = title;
  const hint = document.createElement('span'); hint.textContent = subtitle;
  item.append(icon, heading, hint); box.append(item);
}
function button(text, label, className, handler, focusKey) {
  const node = document.createElement('button'); node.type = 'button'; node.textContent = text;
  node.className = className; node.setAttribute('aria-label', label);
  node.addEventListener('click', handler);
  if (focusKey) node.dataset.focus = focusKey;
  return node;
}
function preserveFocus(box, render, fallback) {
  const focused = box.contains(document.activeElement) ? document.activeElement.dataset.focus : '';
  render();
  if (focused) {
    const match = [...box.querySelectorAll('[data-focus]')].find(node => node.dataset.focus === focused);
    if (match && !match.disabled) match.focus({ preventScroll: true });
    else if (fallback) fallback.focus({ preventScroll: true });
  }
}
async function loadLibrary(path = '.', focusHeading = false) {
  const request = ++libraryRequest;
  $('library-list').setAttribute('aria-busy', 'true');
  $('library-count').textContent = 'Открываем папку…';
  inlineError('library-error');
  try {
    const result = await api(`/api/library?path=${encodeURIComponent(path)}`);
    if (request !== libraryRequest) return;
    library = result; libraryLoaded = true; selected.clear(); $('library-search').value = '';
    renderLibrary();
    if (focusHeading) $('library-heading').focus({ preventScroll: true });
  } catch (err) {
    if (request === libraryRequest) {
      inlineError('library-error', `${err.message} Нажмите «Обновить», чтобы попробовать снова.`);
      $('library-count').textContent = 'Не удалось открыть папку';
      if (!libraryLoaded) { $('library-list').replaceChildren(); emptyState($('library-list'), 'Библиотека недоступна', 'Проверьте настройки папки и повторите обновление.'); }
    }
  } finally {
    if (request === libraryRequest) $('library-list').setAttribute('aria-busy', 'false');
  }
}
function visibleEntries() {
  const query = $('library-search').value.trim().toLocaleLowerCase('ru');
  return (library.entries || []).filter(entry => entry.name.toLocaleLowerCase('ru').includes(query));
}
function renderBreadcrumbs() {
  const nav = $('current-path'); nav.replaceChildren();
  const parts = library.path === '.' ? [] : library.path.split('/');
  ['Вся музыка', ...parts].forEach((part, index) => {
    if (index) nav.append(document.createTextNode('/'));
    const crumb = button(part, part, 'crumb', () => loadLibrary(index ? parts.slice(0, index).join('/') : '.', true));
    if (index === parts.length) crumb.setAttribute('aria-current', 'location');
    nav.append(crumb);
  });
  $('up').disabled = library.path === '.';
}
function renderLibrary() {
  renderBreadcrumbs();
  const box = $('library-list');
  const entries = visibleEntries();
  $('library-count').textContent = entries.length === (library.entries || []).length ? `Элементов: ${entries.length}` : `Найдено: ${entries.length} из ${library.entries.length}`;
  preserveFocus(box, () => {
    box.replaceChildren();
    if (!entries.length) emptyState(box, $('library-search').value ? 'Ничего не найдено' : 'Здесь пока нет музыки', $('library-search').value ? 'Попробуйте другое название.' : 'Загрузите файлы или выберите папку в настройках.', '⌕');
    for (const entry of entries) {
      const row = document.createElement('li'); row.className = `entry ${entry.kind}`;
      const target = document.createElement('label'); target.className = 'check-target';
      const check = document.createElement('input'); check.type = 'checkbox'; check.checked = selected.has(entry.path); check.dataset.focus = `select:${entry.path}`;
      check.setAttribute('aria-label', `Выбрать: ${entry.name}`);
      check.addEventListener('change', () => { if (check.checked) selected.add(entry.path); else selected.delete(entry.path); updateSelection(); });
      target.append(check);
      const name = entry.kind === 'dir' ? button('', `Открыть папку: ${entry.name}`, 'entry-name', () => loadLibrary(entry.path, true), `open:${entry.path}`) : document.createElement('div');
      name.className = 'entry-name';
      const title = document.createElement('span'); title.className = 'entry-title'; title.textContent = entry.name;
      const kind = document.createElement('span'); kind.className = 'kind'; kind.textContent = entry.kind === 'dir' ? 'Папка · открыть' : entry.kind === 'playlist' ? 'Плейлист M3U' : entry.name.split('.').pop().toUpperCase();
      name.append(title, kind);
      const add = button('+', `Добавить в очередь: ${entry.name}`, 'row-action add-entry', () => addToQueue([entry.path], add), `add:${entry.path}`);
      row.append(target, name, add); box.append(row);
    }
  }, $('library-heading'));
  updateButtons();
}
function updateSelection() {
  const count = selected.size;
  $('add-selected').textContent = count ? `Добавить (${count})` : 'Добавить выбранное';
  $('add-selected').disabled = queueBusy() || !count;
  const entries = visibleEntries();
  const checked = entries.filter(entry => selected.has(entry.path)).length;
  $('select-all').checked = !!entries.length && checked === entries.length;
  $('select-all').indeterminate = checked > 0 && checked < entries.length;
  $('select-all').disabled = !entries.length;
}
function applyQueueResult(result) {
  status.queue = result.queue; queueLoaded = true; renderStatus();
}
async function refreshQueue() { applyQueueResult(await api('/api/queue')); }
function additionMessage(result) {
  let text = `Добавлено в очередь: ${result.added ?? 0}. Всего: ${result.queue?.tracks.length || 0}.`;
  if (result.skipped?.length) text += ` Пропущено: ${result.skipped.length}.`;
  if ((result.queue?.tracks.length || 0) >= 1000) text += ' Лимит очереди — 1000 треков.';
  return text;
}
async function addToQueue(paths, source) {
  if (!paths.length || queueBusy()) return;
  if (source) { source.textContent = '…'; source.setAttribute('aria-busy', 'true'); }
  await operation('queue', 'Добавляем музыку… Читаем названия и длительность.', async () => {
    const result = await api('/api/queue', { method: 'POST', body: { action: 'append', paths, recursive: $('recursive').checked } });
    applyQueueResult(result);
    for (const path of paths) selected.delete(path);
    for (const check of $('library-list').querySelectorAll('input')) check.checked = selected.has(check.dataset.focus.slice(7));
    message(additionMessage(result), result.skipped?.length ? 'error' : 'info');
    if (source) source.textContent = '✓';
  });
  if (source) { source.removeAttribute('aria-busy'); if (source.textContent === '…') source.textContent = '+'; setTimeout(() => { source.textContent = '+'; }, 2500); }
}
async function addURL(event) {
  event.preventDefault(); if (queueBusy()) return;
  const url = $('remote-url').value.trim();
  try { if (!['http:', 'https:'].includes(new URL(url).protocol)) throw new Error(); }
  catch { inlineError('remote-error', 'Укажите прямую ссылку с http:// или https://.'); $('remote-url').focus(); return; }
  const body = { action: 'append', url, title: $('remote-title').value.trim(), live: $('remote-live').checked };
  await operation('queue', body.live ? 'Добавляем прямой эфир…' : 'Проверяем аудио по ссылке… До 15 секунд.', async () => {
    const result = await api('/api/queue', { method: 'POST', body }); applyQueueResult(result);
    if (result.added > 0) { $('remote-form').reset(); $('source-dialog').close(); }
    message(additionMessage(result), result.skipped?.length ? 'error' : 'info');
  }, 'remote-error');
}
function uploadRequest(form) {
  return new Promise((resolve, reject) => {
    const xhr = new XMLHttpRequest(); uploadXHR = xhr; xhr.open('POST', '/api/upload'); xhr.responseType = 'json';
    xhr.upload.onprogress = event => {
      if (event.lengthComputable) { const percent = Math.round(event.loaded / event.total * 100); $('upload-meter').value = percent; $('upload-progress').textContent = `Передача файлов: ${percent}%`; }
    };
    xhr.upload.onload = () => { $('upload-progress').textContent = 'Файлы переданы. Сохраняем и читаем метаданные…'; $('upload-meter').removeAttribute('value'); };
    xhr.onload = () => xhr.status >= 200 && xhr.status < 300 ? resolve(xhr.response) : reject(new Error(xhr.response?.error || `Ошибка загрузки (HTTP ${xhr.status}).`));
    xhr.onerror = () => reject(new Error('Не удалось загрузить файлы. Проверьте соединение и повторите выбор.'));
    xhr.onabort = () => reject(new Error('Передача отменена. Уже сохранённые файлы остаются в библиотеке.'));
    xhr.send(form);
  });
}
async function uploadFiles(input) {
  if (queueBusy()) return;
  const files = [...input.files].filter(file => /\.(aac|flac|m4a|mp3|oga|ogg|opus|wav|wma)$/i.test(file.name)); input.value = '';
  if (!files.length) { $('upload-progress').textContent = 'В выбранных файлах нет поддерживаемого аудио.'; return; }
  $('upload-dialog').open || $('upload-dialog').showModal();
  $('upload-results').replaceChildren(); $('upload-meter').hidden = false; $('upload-meter').value = 0; $('cancel-upload').hidden = false;
  await operation('upload', `Загружаем файлов: ${files.length}…`, async () => {
    const form = new FormData(); for (const file of files) form.append('files', file, file.name);
    form.append('relativePaths', JSON.stringify(files.map(file => file.webkitRelativePath || file.name)));
    const addToQueue = $('upload-add-queue').checked; form.append('addToQueue', String(addToQueue));
    const result = await uploadRequest(form);
    if (addToQueue && result.queue) applyQueueResult(result);
    const text = `Сохранено файлов: ${result.saved?.length || 0}. ${addToQueue ? additionMessage(result) : 'Файлы доступны в библиотеке.'}`;
    $('upload-progress').textContent = text;
    for (const [label, items] of [['Сохранён', result.saved], ['Переименован', result.renamed], ['Пропущен', result.skipped]]) {
      for (const item of items || []) { const li = document.createElement('li'); li.textContent = `${label}: ${safeText(item)}`; $('upload-results').append(li); }
    }
    message(text, result.skipped?.length ? 'error' : 'info'); await loadLibrary(library.path);
  });
  if ($('message').classList.contains('error')) $('upload-progress').textContent = $('message').textContent;
  uploadXHR = null; $('cancel-upload').hidden = true; $('upload-meter').hidden = true;
}
function formatDuration(seconds) {
  if (!Number.isFinite(seconds) || seconds <= 0) return '—';
  const hours = Math.floor(seconds / 3600); const minutes = Math.floor(seconds / 60) % 60;
  const tail = `${String(minutes).padStart(hours ? 2 : 1, '0')}:${String(Math.floor(seconds % 60)).padStart(2, '0')}`;
  return hours ? `${hours}:${tail}` : tail;
}
function sourceLabel(track) {
  try { return new URL(track.path).hostname; } catch { return track.path || 'Локальный файл'; }
}
function trackTitle(track) {
  if (track.title && !/^https?:\/\//i.test(track.title)) return track.title;
  try { const url = new URL(track.path); return decodeURIComponent(url.pathname.split('/').filter(Boolean).pop() || url.hostname); }
  catch { return track.title || track.path?.split('/').pop() || 'Без названия'; }
}
function trackSubtitle(track) { return [track.live ? 'Прямой эфир' : '', [track.artist, track.album].filter(Boolean).join(' · ') || sourceLabel(track)].filter(Boolean).join(' · '); }
function entryKey(track, index) { return track.entryId || `${track.id}:${index}`; }
function renderQueue() {
  if (!queueLoaded) return;
  const tracks = status.queue?.tracks || []; const current = status.active?.estimatedIndex ?? -1;
  const key = JSON.stringify([status.queue, current, status.active?.playing]);
  $('queue-count').textContent = String(tracks.length); $('queue-tab-count').textContent = String(tracks.length);
  $('jump-current').hidden = !tracks[current];
  if (key === queueRenderKey) return; queueRenderKey = key;
  const total = tracks.reduce((sum, track) => sum + (track.duration || 0), 0); const liveCount = tracks.filter(track => track.live).length;
  $('queue-summary').textContent = tracks.length ? [`Треков: ${tracks.length}`, total ? formatDuration(total) : '', liveCount ? `Прямых эфиров: ${liveCount}` : '', 'по порядку'].filter(Boolean).join(' · ') : 'Добавьте музыку из библиотеки или по ссылке';
  const box = $('queue-list');
  preserveFocus(box, () => {
    box.replaceChildren();
    if (!tracks.length) { emptyState(box, 'Что будем слушать?', 'Добавьте треки. Они будут играть в выбранном порядке.'); return; }
    tracks.forEach((track, index) => {
      const id = entryKey(track, index);
      const row = document.createElement('li'); row.className = `track${index === current ? ' current' : ''}`; row.dataset.entry = id;
      if (index === current) row.setAttribute('aria-current', 'true');
      const number = document.createElement('span'); number.className = 'track-index'; number.textContent = index === current && status.active?.playing ? '♫' : String(index + 1);
      const name = button('', `Сведения: ${trackTitle(track)}`, 'track-name quiet', () => showMetadata(track), `details:${id}`);
      const title = document.createElement('span'); title.className = 'track-title'; title.textContent = trackTitle(track);
      const sub = document.createElement('span'); sub.className = 'track-sub'; sub.textContent = trackSubtitle(track); name.append(title, sub);
      const duration = document.createElement('span'); duration.className = 'duration'; duration.textContent = track.live ? 'Эфир' : formatDuration(track.duration);
      const actions = document.createElement('div'); actions.className = 'track-actions';
      const play = button('▶', `Играть: ${trackTitle(track)}`, 'play-track', () => control('index', index, track.entryId), `play:${id}`);
      const up = button('↑', `Выше: ${trackTitle(track)}`, 'move-track', () => editQueue('move', track, index - 1), `up:${id}`); up.dataset.edge = String(index === 0); up.disabled = queueBusy() || index === 0;
      const down = button('↓', `Ниже: ${trackTitle(track)}`, 'move-track', () => editQueue('move', track, index + 1), `down:${id}`); down.dataset.edge = String(index === tracks.length - 1); down.disabled = queueBusy() || index === tracks.length - 1;
      const remove = button('×', `Удалить: ${trackTitle(track)}`, 'remove-track', () => removeTrack(track, index), `remove:${id}`);
      actions.append(play, up, down, remove); row.append(number, name, duration, actions); box.append(row);
    });
  }, $('queue-heading'));
}
async function editQueue(action, track, toIndex) {
  if (queueBusy()) return;
  const focusKey = document.activeElement?.dataset.focus;
  await operation('queue', action === 'move' ? 'Меняем порядок…' : 'Удаляем трек…', async () => {
    const result = await api('/api/queue', { method: 'POST', body: { action, entryId: track.entryId, toIndex, queueId: status.queue.id, version: status.queue.version } });
    if (action === 'remove' && status.queue.tracks[status.active?.estimatedIndex]?.entryId === track.entryId) status.active = null;
    else if (status.active) {
      const activeID = status.queue.tracks[status.active.estimatedIndex]?.entryId;
      status.active = { ...status.active, estimatedIndex: result.queue.tracks.findIndex(item => item.entryId === activeID) };
    }
    applyQueueResult(result); message(action === 'move' ? 'Порядок изменён.' : 'Трек удалён из очереди.');
  });
  const target = [...$('queue-list').querySelectorAll('[data-focus]')].find(node => node.dataset.focus === focusKey);
  if (target && !target.disabled) target.focus({ preventScroll: true });
}
async function removeTrack(track, index) {
  if (index === status.active?.estimatedIndex && !await confirmAction('Удалить текущий трек?', 'Музыка остановится. Остальные треки останутся в очереди.', 'Остановить и удалить')) return;
  await editQueue('remove', track, 0);
}
function renderStatus() {
  if (status.active?.entryId && status.queue?.tracks) {
    const index = status.queue.tracks.findIndex(track => track.entryId === status.active.entryId);
    if (index >= 0) status.active.estimatedIndex = index;
  }
  const active = status.active; const ready = Boolean(status.stationReady); const tracks = status.queue?.tracks || [];
  $('station-ready').textContent = connectionError ? 'Статус устарел' : !statusLoaded && status.stationReady === undefined ? 'Проверяем подключение…' : !ready ? 'Настройте подключение' : status.volume?.error || active?.stationError ? 'Проверьте подключение' : statusLoaded ? 'Станция на связи' : 'Проверяем подключение…';
  $('station-ready').className = `chip ${connectionError || status.volume?.error || active?.stationError ? 'bad' : statusLoaded && ready ? 'ok' : ''}`;
  const current = tracks[active?.estimatedIndex ?? 0];
  const liveConnecting = Boolean(current?.live && active && !active.playing && ['starting', 'no_sound'].includes(active.state));
  const labels = { playing: 'Играет', starting: 'Запускаем…', no_sound: 'Звук не подтверждён', stopping: 'Останавливаем…', unknown: 'Нет связи', finished: 'Завершено' };
  $('play-state').textContent = connectionError ? 'Статус устарел' : active?.playing ? 'Играет' : liveConnecting ? 'Подключаем эфир…' : labels[active?.state] || (statusLoaded ? (tracks.length ? 'Готово к запуску' : 'Остановлено') : 'Проверяем…');
  $('play-state').className = `chip ${connectionError || active?.state === 'unknown' || active?.state === 'no_sound' && !liveConnecting ? 'bad' : active?.playing ? 'ok' : liveConnecting || active?.state === 'starting' ? 'warn' : ''}`;
  $('now-playing').querySelector('.title').textContent = current ? trackTitle(current) : queueLoaded ? 'Добавьте свою музыку' : 'Музыка на вашей Станции';
  $('now-playing').querySelector('.sub').textContent = current ? `${!active ? 'Следующий · ' : ''}${trackSubtitle(current)}` : 'Библиотека, файлы или прямая ссылка';
  $('track-details').disabled = !current;
  $('public-base').textContent = status.publicBaseResolved || '—'; $('station-target').textContent = ready ? `${status.stationMode}: ${status.stationTarget || '—'}` : 'Не настроено';
  $('playback-health').textContent = !active ? 'Воспроизведение не запущено' : active.stationError || (active.remoteProcess ? `ALSA ${active.audioState || 'unknown'} · указатель ${active.audioAdvancing ? 'движется' : 'не движется'}` : 'Процесс плеера не найден');
  const warning = connectionError ? `${connectionError}${checkedAt ? ` Последняя проверка: ${checkedAt}.` : ''}` : active?.error || active?.stationError || (liveConnecting ? 'Прямой эфир подключается. Это может занять несколько секунд.' : active?.state === 'no_sound' ? 'Станция не подтверждает звук. Проверьте громкость и диагностику.' : '');
  $('health-warning').textContent = safeText(warning); $('health-warning').hidden = !warning;
  $('health-warning').className = `health-warning${connectionError || active?.error || active?.stationError ? ' error' : ''}`;
  $('retry-status').hidden = !connectionError && !active?.stationError;
  applyVolume(); renderQueue(); updateButtons();
}
function applyVolume() {
  const volume = status.volume; const warning = $('volume-warning');
  if (!volume || volume.error) { $('volume-value').textContent = '—'; warning.textContent = volume?.error ? safeText(volume.error) : ''; }
  else {
    if (document.activeElement !== $('volume') && !pending.has('volume')) $('volume').value = String(volume.percent);
    $('volume-value').textContent = `${pending.has('volume') ? $('volume').value : volume.percent}%`;
    warning.textContent = volume.percent === 0 ? 'Громкость — 0%. Прибавьте, чтобы услышать музыку.' : volume.percent < 10 ? 'Громкость очень низкая.' : '';
  }
}
function updateButtons() {
  const tracks = status.queue?.tracks || []; const current = status.active?.estimatedIndex ?? 0;
  const transportBusy = pending.has('transport') || pending.has('stop') || pending.has('config'); const running = pending.has('transport') || !!status.active && !status.active.finished;
  $('play').disabled = transportBusy || !tracks.length || !status.stationReady || !!status.active?.playing && !connectionError;
  $('stop').disabled = pending.has('stop') || !running;
  $('prev').disabled = transportBusy || !tracks.length || !status.stationReady || current <= 0;
  $('next').disabled = transportBusy || !tracks.length || !status.stationReady || current >= tracks.length - 1;
  $('clear-queue').disabled = queueBusy() || !tracks.length;
  for (const id of ['add-folder', 'upload-button', 'upload-folder-button', 'add-url']) $(id).disabled = queueBusy();
  $('save-settings').disabled = !configLoaded || queueBusy();
  $('upload-add-queue').disabled = pending.has('upload');
  document.querySelectorAll('.add-entry, .remove-track').forEach(node => { node.disabled = queueBusy(); });
  document.querySelectorAll('.move-track').forEach(node => { node.disabled = queueBusy() || node.dataset.edge === 'true'; });
  document.querySelectorAll('.play-track').forEach(node => { node.disabled = transportBusy || !status.stationReady; });
  $('volume').disabled = pending.has('volume') || !status.stationReady || !status.volume || !!status.volume.error;
  $('remote-form').setAttribute('aria-busy', String(pending.has('queue')));
  $('remote-progress').textContent = pending.has('queue') ? 'Проверяем источник… Окно можно закрыть и продолжить управлять музыкой.' : '';
  $('add-url').textContent = pending.has('queue') ? 'Добавляем…' : 'Добавить в очередь';
  updateSelection();
}
async function control(action, index = 0, entryId) {
  const lane = action === 'stop' ? 'stop' : 'transport';
  if (pending.has(lane)) return;
  // Stop supersedes a pending start response, without waiting for source preparation.
  const request = ++transportRequest;
  await operation(lane, action === 'stop' ? 'Останавливаем музыку…' : 'Запускаем трек…', async () => {
    const data = await api('/api/control', { method: 'POST', body: { action, index, ...(entryId ? { entryId } : {}) } });
    if (transportRequest !== request) return;
    status.active = data.active; statusLoaded = true; connectionError = ''; renderStatus();
    message(action === 'stop' ? 'Воспроизведение остановлено.' : data.active?.playing ? 'Воспроизведение началось.' : 'Команда отправлена. Проверяем звук…');
  });
}
async function setVolume(percent) {
  await operation('volume', 'Меняем громкость…', async () => {
    status.volume = await api('/api/volume', { method: 'POST', body: { percent } }); applyVolume(); message(`Громкость: ${status.volume.percent}%.`);
  });
  applyVolume();
}
function updateConnectionFields() { $('adb-settings').hidden = $('station-mode').value !== 'adb'; $('ssh-settings').hidden = $('station-mode').value !== 'ssh'; }
const configFields = { 'music-root': 'musicRoot', 'public-base-input': 'publicBase', 'station-mode': 'stationMode', 'station-serial': 'stationSerial', 'station-host': 'stationHost', 'station-port': 'stationPort', 'station-user': 'stationUser' };
async function loadConfig() {
  const cfg = await api('/api/config'); configSnapshot = cfg;
  configLoaded = true; inlineError('settings-error');
  for (const [id, key] of Object.entries(configFields)) $(id).value = cfg[key] ?? '';
  Object.assign(status, { stationReady: cfg.stationReady, stationMode: cfg.stationMode, stationTarget: cfg.stationMode === 'ssh' ? `${cfg.stationUser}@${cfg.stationHost}:${cfg.stationPort}` : cfg.stationSerial, publicBaseResolved: cfg.publicBaseResolved });
  updateConnectionFields(); renderStatus();
}
async function saveConfig(event) {
  event.preventDefault();
  if (!configLoaded) return;
  const body = Object.fromEntries(Object.entries(configFields).map(([id, key]) => [key, key === 'stationPort' ? Number($(id).value) : $(id).value]));
  const changed = Object.keys(body).some(key => String(body[key]) !== String(configSnapshot[key] ?? ''));
  if (!changed) { $('settings-status').textContent = 'Настройки уже сохранены.'; return; }
  $('settings-status').textContent = '';
  const rootChanged = body.musicRoot !== configSnapshot.musicRoot;
  if (!await confirmAction('Сохранить новые настройки?', rootChanged ? 'Музыка остановится, очередь очистится. Файлы на диске сохранятся.' : 'Музыка остановится. Очередь сохранится; её можно будет запустить снова.', 'Сохранить')) return;
  await operation('config', 'Сохраняем настройки…', async () => {
    await api('/api/config', { method: 'POST', body }); status.active = null;
    await Promise.all([refreshQueue(), loadConfig(), loadLibrary('.')]); $('settings').close(); message('Настройки сохранены.');
  }, 'settings-error');
}
function confirmAction(title, text, action) {
  if ($('confirm-dialog').open) return Promise.resolve(false);
  $('confirm-heading').textContent = title; $('confirm-text').textContent = text; $('confirm-action').textContent = action;
  const dialog = $('confirm-dialog'); dialog.returnValue = ''; dialog.showModal();
  return new Promise(resolve => dialog.addEventListener('close', () => resolve(dialog.returnValue === 'confirm'), { once: true }));
}
function showMetadata(track) {
  const dl = $('metadata-content'); dl.replaceChildren();
  for (const [label, value] of [['Название', trackTitle(track)], ['Исполнитель', track.artist], ['Альбом', track.album], ['Длительность', track.live ? 'Прямой эфир' : formatDuration(track.duration)]]) {
    if (!value) continue; const dt = document.createElement('dt'); dt.textContent = label; const dd = document.createElement('dd'); dd.textContent = value; dl.append(dt, dd);
  }
  $('metadata-path').value = track.path || ''; $('copy-result').textContent = ''; $('metadata-dialog').showModal();
}
function showView(view) {
  document.querySelector('.workspace').dataset.view = view;
  $('library-tab').setAttribute('aria-pressed', String(view === 'library')); $('queue-tab').setAttribute('aria-pressed', String(view === 'queue'));
}
$('library-tab').addEventListener('click', () => showView('library'));
$('queue-tab').addEventListener('click', () => showView('queue'));
document.querySelector('.skip-link').addEventListener('click', () => showView('library'));
$('open-remote').addEventListener('click', () => { $('source-dialog').showModal(); $('remote-url').focus(); });
$('open-upload').addEventListener('click', () => $('upload-dialog').showModal());
document.querySelector('.settings-link').addEventListener('click', () => $('settings').showModal());
document.querySelectorAll('[data-close]').forEach(node => node.addEventListener('click', () => $(node.dataset.close).close()));
$('confirm-cancel').addEventListener('click', () => $('confirm-dialog').close('cancel'));
$('confirm-action').addEventListener('click', () => $('confirm-dialog').close('confirm'));
$('up').addEventListener('click', () => loadLibrary(library.parent || '.', true));
$('refresh').addEventListener('click', () => loadLibrary(library.path));
$('library-search').addEventListener('input', renderLibrary);
$('select-all').addEventListener('change', event => { for (const entry of visibleEntries()) { if (event.target.checked) selected.add(entry.path); else selected.delete(entry.path); } renderLibrary(); });
$('add-selected').addEventListener('click', () => addToQueue([...selected]));
$('add-folder').addEventListener('click', () => addToQueue([library.path]));
$('clear-queue').addEventListener('click', async () => {
  if (!await confirmAction('Очистить очередь?', `Будут удалены все треки (${status.queue?.tracks.length || 0}). Музыка остановится. Файлы в библиотеке сохранятся.`, 'Очистить очередь')) return;
  await operation('queue', 'Очищаем очередь…', async () => { const result = await api('/api/queue', { method: 'DELETE' }); status.active = null; applyQueueResult(result); message('Очередь очищена.'); });
});
$('upload-button').addEventListener('click', () => $('upload-input').click());
$('upload-folder-button').addEventListener('click', () => $('upload-folder-input').click());
$('upload-input').addEventListener('change', () => uploadFiles($('upload-input')));
$('upload-folder-input').addEventListener('change', () => uploadFiles($('upload-folder-input')));
$('cancel-upload').addEventListener('click', () => uploadXHR?.abort());
$('prev').addEventListener('click', () => control('prev'));
$('play').addEventListener('click', () => control('play', status.active?.estimatedIndex ?? 0));
$('stop').addEventListener('click', () => control('stop'));
$('next').addEventListener('click', () => control('next'));
$('volume').addEventListener('input', () => { $('volume-value').textContent = `${$('volume').value}%`; });
$('volume').addEventListener('change', () => setVolume(Number($('volume').value)));
$('settings-form').addEventListener('submit', saveConfig);
$('remote-form').addEventListener('submit', addURL);
$('station-mode').addEventListener('change', updateConnectionFields);
$('retry-status').addEventListener('click', () => schedulePoll(0));
$('jump-current').addEventListener('click', () => { const row = document.querySelector('.track.current'); if (row) { row.scrollIntoView({ block: 'center' }); row.querySelector('.track-name').focus({ preventScroll: true }); } });
$('track-details').addEventListener('click', () => { const track = status.queue?.tracks[status.active?.estimatedIndex ?? 0]; if (track) showMetadata(track); });
$('copy-path').addEventListener('click', async () => {
  try { await navigator.clipboard.writeText($('metadata-path').value); $('copy-result').textContent = 'Адрес скопирован.'; }
  catch { $('metadata-path').focus(); $('metadata-path').select(); $('copy-result').textContent = 'Выделен полный адрес. Скопируйте его вручную.'; }
});
const mobile = matchMedia('(max-width: 900px)');
function syncPlayer() { document.querySelector('.player-extra').open = !mobile.matches; }
mobile.addEventListener('change', syncPlayer); syncPlayer();
new ResizeObserver(entries => { document.documentElement.style.setProperty('--player-height', `${Math.ceil(entries[0].target.getBoundingClientRect().height)}px`); }).observe(document.querySelector('.player-panel'));
function schedulePoll(delay = 1500) {
  clearTimeout(pollTimer);
  if (pollRunning) { pollAgain = true; return; }
  pollTimer = setTimeout(pollStatus, delay);
}
async function pollStatus() {
  if (pollRunning) return; pollRunning = true; pollAgain = false;
  const snapshot = { ...versions };
  try {
    const result = await api('/api/status');
    if (versions.config === snapshot.config && !pending.has('config')) {
      for (const key of ['stationReady', 'stationMode', 'stationTarget', 'publicBaseResolved']) status[key] = result[key];
      if (!queueBusy() && versions.queue === snapshot.queue) { status.queue = result.queue; queueLoaded = true; }
      if (!pending.has('transport') && !pending.has('stop') && versions.transport === snapshot.transport) status.active = result.active;
      if (!pending.has('volume') && versions.volume === snapshot.volume) status.volume = result.volume;
      connectionError = ''; statusLoaded = true; checkedAt = new Date().toLocaleTimeString('ru', { hour: '2-digit', minute: '2-digit', second: '2-digit' }); renderStatus();
    }
  } catch {
    connectionError = 'Нет связи с сервером.'; renderStatus();
  } finally { pollRunning = false; schedulePoll(pollAgain ? 0 : 1500); }
}
async function boot() {
  renderStatus();
  await Promise.allSettled([loadConfig().catch(err => { message(err.message, 'error'); inlineError('settings-error', 'Не удалось получить настройки. Обновите страницу перед сохранением.'); }), loadLibrary('.'), refreshQueue().catch(err => { $('queue-list').replaceChildren(); emptyState($('queue-list'), 'Не удалось открыть очередь', 'Повторяем проверку соединения…'); message(err.message, 'error'); })]);
  schedulePoll(0);
}
boot();
