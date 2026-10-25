const $ = id => document.getElementById(id);
let library = { path: '.', entries: [] };
let status = {};
const selected = new Set();
let lastVolume = null;
let busy = false;
let revision = 0;
let libraryRequest = 0;
let queueRenderKey = '';
let connectionError = '';

async function api(path, options = {}) {
  const body = options.body;
  const response = await fetch(path, {
    method: options.method || 'GET',
    headers: body && !(body instanceof FormData) ? { 'Content-Type': 'application/json' } : undefined,
    body: body && !(body instanceof FormData) ? JSON.stringify(body) : body,
  });
  let data;
  try { data = await response.json(); } catch { data = {}; }
  if (!response.ok) throw new Error(data.error || `Ошибка сервера (HTTP ${response.status})`);
  return data;
}

function message(text, kind = 'info') {
  $('message').className = `message ${kind}`;
  $('message').textContent = text;
}

async function operation(text, task) {
  if (busy) return;
  busy = true;
  revision++;
  message(text, 'busy');
  updateButtons();
  try { await task(); }
  catch (err) { message(err.message, 'error'); }
  finally {
    busy = false;
    revision++;
    updateButtons();
  }
}

function emptyState(box, title, subtitle, symbol = '♫') {
  const empty = document.createElement('div');
  empty.className = 'empty';
  const icon = document.createElement('span');
  icon.className = 'empty-symbol';
  icon.setAttribute('aria-hidden', 'true');
  icon.textContent = symbol;
  const heading = document.createElement('strong');
  heading.textContent = title;
  const hint = document.createElement('span');
  hint.textContent = subtitle;
  empty.append(icon, heading, hint);
  box.append(empty);
}

async function loadLibrary(path = '.') {
  const request = ++libraryRequest;
  $('library-list').setAttribute('aria-busy', 'true');
  $('library-count').textContent = 'Открываем папку…';
  try {
    const result = await api(`/api/library?path=${encodeURIComponent(path)}`);
    if (request !== libraryRequest) return;
    library = result;
    selected.clear();
    $('library-search').value = '';
    renderLibrary();
  } catch (err) {
    if (request === libraryRequest) message(err.message, 'error');
  } finally {
    if (request === libraryRequest) $('library-list').setAttribute('aria-busy', 'false');
  }
}

function visibleEntries() {
  const query = $('library-search').value.trim().toLocaleLowerCase('ru');
  return (library.entries || []).filter(entry => entry.name.toLocaleLowerCase('ru').includes(query));
}

function renderBreadcrumbs() {
  const nav = $('current-path');
  nav.replaceChildren();
  const parts = library.path === '.' ? [] : library.path.split('/');
  ['Вся музыка', ...parts].forEach((part, index) => {
    if (index) nav.append(document.createTextNode(' / '));
    const button = document.createElement('button');
    button.type = 'button';
    button.className = 'crumb';
    button.textContent = part;
    if (index === parts.length) button.setAttribute('aria-current', 'location');
    button.addEventListener('click', () => loadLibrary(index ? parts.slice(0, index).join('/') : '.'));
    nav.append(button);
  });
  $('up').disabled = library.path === '.';
}

function renderLibrary() {
  renderBreadcrumbs();
  const box = $('library-list');
  box.replaceChildren();
  const entries = visibleEntries();
  $('library-count').textContent = entries.length === (library.entries || []).length
    ? `Элементов: ${entries.length}` : `Найдено: ${entries.length} из ${library.entries.length}`;
  if (!entries.length) emptyState(box, $('library-search').value ? 'Ничего не найдено' : 'Здесь пока нет музыки', $('library-search').value ? 'Попробуйте другое название.' : 'Загрузите файлы или выберите другую папку в настройках.', '⌕');
  const fragment = document.createDocumentFragment();
  for (const entry of entries) {
    const row = document.createElement('div');
    row.className = `entry ${entry.kind}`;
    const check = document.createElement('input');
    check.type = 'checkbox';
    check.checked = selected.has(entry.path);
    check.setAttribute('aria-label', `Выбрать: ${entry.name}`);
    check.addEventListener('change', () => {
      if (check.checked) selected.add(entry.path); else selected.delete(entry.path);
      updateSelection();
    });
    const name = document.createElement(entry.kind === 'dir' ? 'button' : 'div');
    name.className = 'entry-name';
    name.textContent = entry.name;
    name.title = entry.path;
    if (entry.kind === 'dir') {
      name.type = 'button';
      name.setAttribute('aria-label', `Открыть папку: ${entry.name}`);
      name.addEventListener('click', () => loadLibrary(entry.path));
    }
    const kind = document.createElement('span');
    kind.className = 'kind';
    kind.textContent = entry.kind === 'dir' ? 'Папка · нажмите, чтобы открыть' : entry.kind === 'playlist' ? 'Плейлист M3U' : entry.name.split('.').pop().toUpperCase();
    name.append(kind);
    const add = document.createElement('button');
    add.type = 'button';
    add.className = 'row-action add-entry';
    add.textContent = '+ Добавить';
    add.setAttribute('aria-label', `Добавить в очередь: ${entry.name}`);
    add.addEventListener('click', () => addToQueue([entry.path]));
    row.append(check, name, add);
    fragment.append(row);
  }
  box.append(fragment);
  updateButtons();
}

function updateSelection() {
  const count = selected.size;
  $('add-selected').textContent = count ? `Добавить выбранное (${count})` : 'Добавить выбранное';
  $('add-selected').disabled = busy || !count;
  const entries = visibleEntries();
  const checked = entries.filter(entry => selected.has(entry.path)).length;
  $('select-all').checked = !!entries.length && checked === entries.length;
  $('select-all').indeterminate = checked > 0 && checked < entries.length;
  $('select-all').disabled = !entries.length;
}

function applyQueueResult(result) {
  status = { ...status, queue: result.queue, active: null };
  renderStatus();
}

function additionMessage(result) {
  const count = result.added ?? 0;
  let text = `Добавлено в очередь: ${count}. Всего: ${result.queue?.tracks.length || 0}.`;
  if (result.skipped?.length) text += ` Не удалось прочитать файлов: ${result.skipped.length}.`;
  if ((result.queue?.tracks.length || 0) >= 1000) text += ' Достигнут лимит очереди — 1000 треков.';
  text += count ? ' Нажмите «Слушать», чтобы начать.' : '';
  return text;
}

async function addToQueue(paths) {
  if (!paths.length) return;
  await operation('Добавляем музыку… Читаем названия и длительность треков.', async () => {
    const result = await api('/api/queue', { method: 'POST', body: { action: 'append', paths, recursive: $('recursive').checked } });
    applyQueueResult(result);
    selected.clear();
    renderLibrary();
    message(additionMessage(result));
  });
}

async function addURL(event) {
  event.preventDefault();
  const url = $('remote-url').value.trim();
  try {
    if (!['http:', 'https:'].includes(new URL(url).protocol)) throw new Error();
  } catch {
    message('Укажите прямую ссылку на аудио, начинающуюся с http:// или https://.', 'error');
    $('remote-url').focus();
    return;
  }
  const body = { action: 'append', url, title: $('remote-title').value.trim(), live: $('remote-live').checked };
  await operation(body.live ? 'Добавляем прямой эфир…' : 'Проверяем аудио по ссылке… Это может занять до 15 секунд.', async () => {
    const result = await api('/api/queue', { method: 'POST', body });
    applyQueueResult(result);
    if (result.added > 0) $('remote-form').reset();
    message(additionMessage(result));
  });
}

// Upload progress covers file transfer; metadata preparation is a separate stage.
function uploadRequest(form) {
  return new Promise((resolve, reject) => {
    const xhr = new XMLHttpRequest();
    xhr.open('POST', '/api/upload');
    xhr.responseType = 'json';
    xhr.upload.onprogress = event => {
      if (event.lengthComputable) message(`Загрузка файлов: ${Math.round(event.loaded / event.total * 100)}%`, 'busy');
    };
    xhr.upload.onload = () => message('Файлы переданы. Читаем метаданные и добавляем в очередь…', 'busy');
    xhr.onload = () => {
      if (xhr.status >= 200 && xhr.status < 300) resolve(xhr.response);
      else reject(new Error(xhr.response?.error || `Ошибка загрузки (HTTP ${xhr.status})`));
    };
    xhr.onerror = () => reject(new Error('Не удалось загрузить файлы. Проверьте соединение с сервером.'));
    xhr.onabort = () => reject(new Error('Загрузка прервана.'));
    xhr.send(form);
  });
}

async function uploadFiles(input) {
  const files = [...input.files].filter(file => /\.(aac|flac|m4a|mp3|oga|ogg|opus|wav|wma)$/i.test(file.name));
  input.value = '';
  if (!files.length) {
    message('В выбранных файлах нет поддерживаемого аудио. Выберите MP3, FLAC, WAV или другой аудиоформат.', 'error');
    return;
  }
  await operation(`Готовим к загрузке файлов: ${files.length}…`, async () => {
    const form = new FormData();
    for (const file of files) form.append('files', file, file.name);
    form.append('relativePaths', JSON.stringify(files.map(file => file.webkitRelativePath || file.name)));
    form.append('addToQueue', 'true');
    const result = await uploadRequest(form);
    applyQueueResult(result);
    message(`Сохранено файлов: ${result.saved?.length || 0}. ${additionMessage(result)}`);
    await loadLibrary(library.path);
  });
}

function formatDuration(seconds) {
  if (!Number.isFinite(seconds) || seconds <= 0) return '—';
  const hours = Math.floor(seconds / 3600);
  const minutes = Math.floor(seconds / 60) % 60;
  const tail = `${String(minutes).padStart(hours ? 2 : 1, '0')}:${String(Math.floor(seconds % 60)).padStart(2, '0')}`;
  return hours ? `${hours}:${tail}` : tail;
}

function renderQueue() {
  const tracks = status.queue?.tracks || [];
  const current = status.active?.estimatedIndex ?? -1;
  const key = `${status.queue?.id || ''}:${current}:${Boolean(status.active?.playing)}`;
  if (key === queueRenderKey) return;
  queueRenderKey = key;
  const box = $('queue-list');
  $('queue-count').textContent = String(tracks.length);
  const total = tracks.reduce((sum, track) => sum + (track.duration || 0), 0);
  const liveCount = tracks.filter(track => track.live).length;
  const summary = [`Треков: ${tracks.length}`];
  if (total) summary.push(`${formatDuration(total)}${liveCount ? ' + эфир' : ''}`);
  if (liveCount) summary.push(`Прямых эфиров: ${liveCount}`);
  summary.push('по порядку');
  $('queue-summary').textContent = tracks.length ? summary.join(' · ') : 'Треки будут играть по порядку';
  box.replaceChildren();
  if (!tracks.length) {
    emptyState(box, 'Что будем слушать?', 'Вставьте ссылку на аудио или добавьте музыку из библиотеки.');
    return;
  }
  const fragment = document.createDocumentFragment();
  tracks.forEach((track, index) => {
    const row = document.createElement('div');
    row.className = `track${index === current ? ' current' : ''}`;
    if (index === current) row.setAttribute('aria-current', 'true');
    const number = document.createElement('span');
    number.className = 'track-index';
    number.textContent = index === current && status.active?.playing ? '♫' : String(index + 1);
    const name = document.createElement('div');
    name.className = 'track-name';
    name.textContent = track.title || track.path;
    const sub = document.createElement('span');
    sub.className = 'track-sub';
    sub.textContent = [track.live ? 'Прямой эфир' : '', [track.artist, track.album].filter(Boolean).join(' · ') || track.path].filter(Boolean).join(' · ');
    name.append(sub);
    const duration = document.createElement('span');
    duration.className = 'duration';
    duration.textContent = track.live ? 'Эфир' : formatDuration(track.duration);
    const play = document.createElement('button');
    play.type = 'button';
    play.className = 'row-action play-track';
    play.textContent = '▶ Играть';
    play.setAttribute('aria-label', `Играть: ${track.title || track.path}`);
    play.addEventListener('click', () => control('index', index));
    row.append(number, name, duration, play);
    fragment.append(row);
  });
  box.append(fragment);
}

function renderStatus() {
  const active = status.active;
  const ready = Boolean(status.stationReady);
  $('station-ready').textContent = connectionError ? 'Нет связи с сервером' : !ready ? 'Настройте подключение' : status.volume?.error || active?.stationError ? 'Проверьте подключение' : status.volume ? 'Станция на связи' : 'Проверяем подключение…';
  $('station-ready').className = `chip ${connectionError || !ready || status.volume?.error || active?.stationError ? 'bad' : status.volume ? 'ok' : ''}`;
  const tracks = status.queue?.tracks || [];
  const currentTrack = tracks[active?.estimatedIndex];
  const liveConnecting = Boolean(currentTrack?.live && !active?.playing && (active?.state === 'starting' || active?.state === 'no_sound'));
  const labels = { playing: 'Играет', starting: 'Запускаем…', no_sound: 'Звук не подтверждён', stopping: 'Остановлено', unknown: 'Нет связи', finished: 'Завершено' };
  $('play-state').textContent = active?.playing ? 'Играет' : liveConnecting ? 'Подключаем эфир…' : labels[active?.state] || 'Остановлено';
  $('play-state').className = `chip ${active?.playing ? 'ok' : liveConnecting || active?.state === 'starting' ? 'warn' : active?.state === 'no_sound' || active?.state === 'unknown' ? 'bad' : ''}`;
  $('public-base').textContent = status.publicBaseResolved || '—';
  $('station-target').textContent = ready ? `${status.stationMode}: ${status.stationTarget || '—'}` : 'не настроено';
  applyVolume(status.volume, document.activeElement !== $('volume'));
  const now = $('now-playing');
  const current = tracks[active?.estimatedIndex ?? 0];
  now.replaceChildren();
  const title = document.createElement('div');
  title.className = 'title';
  title.textContent = current ? current.title || current.path : 'Добавьте свою музыку';
  const sub = document.createElement('div');
  sub.className = 'sub';
  sub.textContent = current ? [current.artist, current.album].filter(Boolean).join(' · ') || current.path : 'Выберите треки в библиотеке ниже';
  now.append(title, sub);
  $('playback-health').textContent = !active ? 'Воспроизведение не запущено' : active.stationError || (active.remoteProcess ? `ALSA ${active.audioState || 'unknown'} · указатель ${active.audioAdvancing ? 'движется' : 'не движется'}` : 'Процесс плеера не найден');
  const warning = connectionError || active?.error || active?.stationError || (liveConnecting
    ? 'Прямой эфир подключается. Это может занять несколько секунд.'
    : active?.state === 'no_sound' ? 'Станция не подтверждает вывод звука. Проверьте громкость или откройте техническую диагностику в настройках.' : '');
  $('health-warning').textContent = warning;
  $('health-warning').hidden = !warning;
  renderQueue();
  updateButtons();
}

function applyVolume(volume, updateSlider = true) {
  lastVolume = volume || null;
  const warning = $('volume-warning');
  $('volume').disabled = busy || !volume || !!volume.error;
  if (!volume || volume.error) {
    $('volume-value').textContent = '—';
    warning.textContent = volume?.error || '';
  } else {
    if (updateSlider) $('volume').value = String(volume.percent);
    $('volume-value').textContent = `${volume.percent}%`;
    warning.textContent = volume.percent === 0 ? 'Громкость — 0%. Прибавьте, чтобы услышать музыку.' : volume.percent < 10 ? 'Громкость очень низкая. Музыка может быть почти не слышна.' : '';
  }
  warning.className = `volume-warning${warning.textContent ? ' visible' : ''}`;
}

function updateButtons() {
  const tracks = status.queue?.tracks || [];
  const current = status.active?.estimatedIndex ?? 0;
  const running = !!status.active && !status.active.finished;
  $('play').disabled = busy || !tracks.length || !status.stationReady || !!status.active?.playing;
  $('stop').disabled = busy || !running;
  $('prev').disabled = busy || !tracks.length || !status.stationReady || current <= 0;
  $('next').disabled = busy || !tracks.length || !status.stationReady || current >= tracks.length - 1;
  $('clear-queue').disabled = busy || !tracks.length;
  for (const id of ['add-folder', 'upload-button', 'upload-folder-button', 'save-settings', 'add-url', 'remote-url', 'remote-title', 'remote-live']) $(id).disabled = busy;
  document.querySelectorAll('.add-entry').forEach(button => { button.disabled = busy; });
  document.querySelectorAll('.play-track').forEach(button => { button.disabled = busy || !status.stationReady; });
  $('volume').disabled = busy || !lastVolume || !!lastVolume.error;
  document.querySelector('.library-panel').setAttribute('aria-busy', String(busy));
  $('remote-form').setAttribute('aria-busy', String(busy));
  updateSelection();
}

async function control(action, index = 0) {
  await operation(action === 'stop' ? 'Останавливаем воспроизведение…' : 'Запускаем трек на Станции…', async () => {
    status = await api('/api/control', { method: 'POST', body: { action, index } });
    connectionError = '';
    renderStatus();
    message(action === 'stop' ? 'Воспроизведение остановлено.' : status.active?.playing ? 'Воспроизведение началось.' : 'Команда отправлена. Проверяем звук на Станции…');
  });
}

async function setVolume(percent) {
  await operation('Меняем громкость…', async () => {
    try {
      const volume = await api('/api/volume', { method: 'POST', body: { percent } });
      status.volume = volume;
      message(`Громкость: ${volume.percent}%.`);
    } finally {
      applyVolume(status.volume);
    }
  });
}

function updateConnectionFields() {
  $('adb-settings').hidden = $('station-mode').value !== 'adb';
  $('ssh-settings').hidden = $('station-mode').value !== 'ssh';
}

async function loadConfig() {
  const cfg = await api('/api/config');
  const fields = { 'music-root': 'musicRoot', 'public-base-input': 'publicBase', 'station-mode': 'stationMode', 'station-serial': 'stationSerial', 'station-host': 'stationHost', 'station-port': 'stationPort', 'station-user': 'stationUser' };
  for (const [id, key] of Object.entries(fields)) $(id).value = cfg[key] ?? '';
  status = { ...status, stationReady: cfg.stationReady, stationMode: cfg.stationMode, stationTarget: cfg.stationMode === 'ssh' ? `${cfg.stationUser}@${cfg.stationHost}:${cfg.stationPort}` : cfg.stationSerial, publicBaseResolved: cfg.publicBaseResolved };
  updateConnectionFields();
  renderStatus();
}

async function saveConfig(event) {
  event.preventDefault();
  await operation('Сохраняем настройки…', async () => {
    const body = { musicRoot: $('music-root').value, publicBase: $('public-base-input').value, stationMode: $('station-mode').value, stationSerial: $('station-serial').value, stationHost: $('station-host').value, stationPort: Number($('station-port').value), stationUser: $('station-user').value };
    await api('/api/config', { method: 'POST', body });
    const queue = await api('/api/queue');
    applyQueueResult(queue);
    await Promise.all([loadConfig(), loadLibrary('.')]);
    message('Настройки сохранены.');
  });
}

$('up').addEventListener('click', () => loadLibrary(library.parent || '.'));
$('refresh').addEventListener('click', () => loadLibrary(library.path));
$('library-search').addEventListener('input', renderLibrary);
$('select-all').addEventListener('change', event => {
  for (const entry of visibleEntries()) {
    if (event.target.checked) selected.add(entry.path); else selected.delete(entry.path);
  }
  renderLibrary();
});
$('add-selected').addEventListener('click', () => addToQueue([...selected]));
$('add-folder').addEventListener('click', () => addToQueue([library.path]));
$('clear-queue').addEventListener('click', () => operation('Очищаем очередь…', async () => {
  applyQueueResult(await api('/api/queue', { method: 'DELETE' }));
  message('Очередь очищена. Выберите новую музыку в библиотеке.');
}));
$('upload-button').addEventListener('click', () => $('upload-input').click());
$('upload-folder-button').addEventListener('click', () => $('upload-folder-input').click());
$('upload-input').addEventListener('change', () => uploadFiles($('upload-input')));
$('upload-folder-input').addEventListener('change', () => uploadFiles($('upload-folder-input')));
$('prev').addEventListener('click', () => control('prev'));
$('play').addEventListener('click', () => control('play', status.active?.estimatedIndex ?? 0));
$('stop').addEventListener('click', () => control('stop'));
$('next').addEventListener('click', () => control('next'));
$('volume').addEventListener('input', () => { $('volume-value').textContent = `${$('volume').value}%`; });
$('volume').addEventListener('change', () => setVolume(Number($('volume').value)));
$('settings-form').addEventListener('submit', saveConfig);
$('remote-form').addEventListener('submit', addURL);
$('station-mode').addEventListener('change', updateConnectionFields);
document.querySelector('.settings-link').addEventListener('click', () => { $('settings').open = true; });

// Do not overlap station polls or let a slow, old response overwrite a mutation.
async function pollStatus() {
  const startedRevision = revision;
  try {
    if (!busy) {
      const result = await api('/api/status');
      if (!busy && revision === startedRevision) {
        status = result;
        connectionError = '';
        renderStatus();
      }
    }
  } catch {
    if (!busy && revision === startedRevision) {
      connectionError = 'Не удаётся связаться с сервером. Проверьте соединение; статус может быть устаревшим.';
      renderStatus();
    }
  } finally { setTimeout(pollStatus, 1500); }
}

async function boot() {
  renderLibrary();
  renderStatus();
  await Promise.all([loadConfig(), loadLibrary('.'), api('/api/queue').then(result => {
    status.queue = result.queue;
    renderStatus();
  })]);
}
boot().catch(err => message(err.message, 'error')).finally(pollStatus);
