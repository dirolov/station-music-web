"""Chromium regression checks with all API traffic intercepted, never a Station."""
import asyncio
import copy
import functools
import http.server
import json
import os
import pathlib
import shutil
import tempfile
import threading
from urllib.parse import parse_qs, urlsplit

from playwright.async_api import async_playwright

ROOT = pathlib.Path(__file__).resolve().parents[1]
ARTIFACTS = pathlib.Path(os.environ.get('UI_ARTIFACTS') or tempfile.mkdtemp(prefix='station-ui-smoke-'))
ARTIFACTS.mkdir(parents=True, exist_ok=True)
CHROMIUM = os.environ.get('CHROMIUM_PATH') or shutil.which('chromium') or shutil.which('chromium-browser')
if not CHROMIUM:
    raise RuntimeError('Set CHROMIUM_PATH to a Chromium executable')


class QuietHandler(http.server.SimpleHTTPRequestHandler):
    def log_message(self, *args):
        pass


server = http.server.ThreadingHTTPServer(('127.0.0.1', 0), functools.partial(QuietHandler, directory=str(ROOT / 'static')))
threading.Thread(target=server.serve_forever, daemon=True).start()


async def main():
    async with async_playwright() as p:
        browser = await p.chromium.launch(executable_path=CHROMIUM, headless=True, args=['--no-sandbox'])
        page = await browser.new_page(viewport={'width': 1440, 'height': 900}, device_scale_factor=1)
        errors, calls, queue_posts, scenarios, geometry = [], [], [], [], []
        page.on('pageerror', lambda error: errors.append(str(error)))
        cfg = {'musicRoot': '/fixture/Music', 'publicBase': 'http://192.0.2.9:8080', 'stationMode': 'adb', 'stationSerial': 'fixture:5555', 'stationHost': '', 'stationPort': 2222, 'stationUser': 'root', 'stationReady': True}
        entries = [{'name': 'Альбом', 'path': 'Альбом', 'kind': 'dir'}, {'name': 'Вечер.m3u', 'path': 'Вечер.m3u', 'kind': 'playlist'}] + [{'name': f'{i:02} — Трек.mp3', 'path': f'{i:02}.mp3', 'kind': 'audio'} for i in range(1, 17)]
        tracks = [{'id': f't{i}', 'entryId': f'e{i}', 'path': f'Альбом/{i}.mp3', 'title': f'Композиция {i}', 'artist': 'Исполнитель', 'album': 'Альбом', 'duration': 185 + i} for i in range(13)]
        state = {'queue': {'id': 'fixture', 'version': 1, 'tracks': copy.deepcopy(tracks)}, 'active': None, 'volume': {'percent': 40}, 'stationReady': True, 'stationMode': 'adb', 'stationTarget': 'fixture:5555', 'publicBaseResolved': 'http://192.0.2.9:8080'}
        fail_next = False
        offline = False
        conflict_next = False
        append_gate = None
        start_gate = None
        upload_gate = None
        poll_count = 0
        upload_count = 0
        uploads_to_queue = []

        async def route_handler(route):
            nonlocal fail_next, conflict_next, poll_count, upload_count
            req = route.request
            path = urlsplit(req.url).path.removeprefix('/api/')
            calls.append((path, req.method))
            data, code = {}, 200
            if path == 'config':
                if req.method == 'POST':
                    cfg.update(req.post_data_json)
                    state['active'] = None
                data = cfg
            elif path == 'library':
                folder = parse_qs(urlsplit(req.url).query).get('path', ['.'])[0]
                data = {'path': folder, 'parent': '.', 'entries': entries if folder == '.' else entries[2:]}
            elif path == 'status':
                poll_count += 1
                if offline:
                    await route.abort('failed')
                    return
                data = copy.deepcopy(state)
                await asyncio.sleep(.03)
            elif path == 'queue' and req.method == 'GET':
                data = {'queue': state['queue']}
            elif path == 'queue' and req.method == 'POST':
                body = req.post_data_json
                queue_posts.append(body)
                if append_gate and body['action'] == 'append':
                    await append_gate.wait()
                if fail_next:
                    code, data, fail_next = 400, {'error': 'Тестовая ошибка чтения файла'}, False
                elif conflict_next:
                    code, data, conflict_next = 409, {'error': 'Очередь изменилась; повторите действие'}, False
                elif body['action'] in ['move', 'remove']:
                    q = state['queue']
                    assert body['queueId'] == q['id'] and body['version'] == q['version']
                    index = next(i for i, track in enumerate(q['tracks']) if track['entryId'] == body['entryId'])
                    active_id = q['tracks'][state['active']['estimatedIndex']]['entryId'] if state['active'] else None
                    track = q['tracks'].pop(index)
                    if body['action'] == 'move':
                        q['tracks'].insert(body['toIndex'], track)
                    elif active_id == track['entryId']:
                        state['active'] = None
                    if state['active']:
                        state['active']['estimatedIndex'] = next(i for i, track in enumerate(q['tracks']) if track['entryId'] == active_id)
                    q['version'] += 1
                    data = {'queue': q}
                else:
                    q = state['queue'] or {'id': 'fixture-new', 'version': 0, 'tracks': []}
                    track = {'id': 'radio', 'path': body['url'], 'title': body.get('title'), 'live': body.get('live'), 'duration': 0} if 'url' in body else copy.deepcopy(tracks[0])
                    track['entryId'] = f'e-add-{len(queue_posts)}'
                    q['tracks'].append(track)
                    q['version'] += 1
                    state['queue'] = q
                    data = {'queue': q, 'added': 1, 'skipped': []}
            elif path == 'queue' and req.method == 'DELETE':
                state['queue'], state['active'] = None, None
                data = {'queue': None}
            elif path == 'control':
                body = req.post_data_json
                index = body.get('index', 0)
                if body.get('entryId'):
                    index = next(i for i, track in enumerate(state['queue']['tracks']) if track['entryId'] == body['entryId'])
                state['active'] = None if body['action'] == 'stop' else {'entryId': state['queue']['tracks'][index]['entryId'], 'estimatedIndex': index, 'playing': True, 'state': 'playing', 'finished': False, 'remoteProcess': True, 'audioState': 'RUNNING', 'audioAdvancing': True}
                data = copy.deepcopy(state)
                if start_gate and body['action'] != 'stop':
                    await start_gate.wait()
            elif path == 'volume':
                state['volume'] = {'percent': req.post_data_json['percent']}
                data = state['volume']
            elif path == 'upload':
                upload_count += 1
                raw = req.post_data_buffer
                assert b'unrelated.txt' not in raw
                add = b'\r\ntrue\r\n' in raw
                uploads_to_queue.append(add)
                if upload_gate:
                    await upload_gate.wait()
                    return  # Aborted request: no server-side queue commit in this fixture.
                if add:
                    q = state['queue']
                    q['tracks'].append({**tracks[0], 'entryId': f'e-upload-{upload_count}'})
                    q['version'] += 1
                data = {'queue': state['queue'] if add else None, 'added': 1 if add else 0, 'saved': ['test (1).mp3'], 'renamed': ['test (1).mp3'], 'skipped': ['broken.mp3: повреждён'] if add else []}
            else:
                raise AssertionError(f'Unexpected API: {path}')
            await route.fulfill(status=code, content_type='application/json', body=json.dumps(data))

        async def poll():
            count = poll_count
            await page.evaluate('schedulePoll(0)')
            for _ in range(100):
                if poll_count > count:
                    break
                await asyncio.sleep(.02)
            await page.wait_for_timeout(80)

        async def idle():
            await page.wait_for_function('pending.size === 0')

        async def check_geometry(width, height, label):
            await page.set_viewport_size({'width': width, 'height': height})
            await page.evaluate("showView('library'); scrollTo(0,0)")
            await page.wait_for_timeout(80)
            result = await page.evaluate('''() => {
              const rect = selector => {const r=document.querySelector(selector).getBoundingClientRect(); return {x:r.x,y:r.y,width:r.width,height:r.height,bottom:r.bottom};};
              const player=rect('.player-panel');
              return {width:innerWidth,height:innerHeight,scrollWidth:document.documentElement.scrollWidth,player,search:rect('#library-search'),firstRow:rect('.entry'),title:rect('#now-playing .title'),visibleRows:[...document.querySelectorAll('.entry')].filter(n=>n.getBoundingClientRect().bottom<=player.y).length};
            }''')
            result['fixture'] = label
            geometry.append(result)
            assert result['scrollWidth'] <= width, result
            assert abs(result['player']['bottom'] - height) <= 1, result
            assert result['player']['height'] < height * .45, result
            assert result['search']['bottom'] < result['player']['y'], result
            assert result['visibleRows'] >= (2 if height >= 800 else 1), result
            assert result['title']['height'] <= 41, result
            small_targets = await page.evaluate('''() => [...document.querySelectorAll('button, .inline, .check-target, .player-extra summary')].filter(node => {
              const r=node.getBoundingClientRect();
              return r.width && r.height && getComputedStyle(node).visibility!=='hidden' && (r.width<24 || r.height<24);
            }).map(node=>node.id || node.className)''')
            assert not small_targets, (width, small_targets)
            if width <= 900:
                await page.locator('#queue-tab').click()
                assert await page.locator('#queue-panel').is_visible()
                assert not await page.locator('#library-panel').is_visible()
            else:
                assert await page.locator('#queue-panel').is_visible()
            await page.locator('.track').last.scroll_into_view_if_needed()
            await page.evaluate('scrollTo(0, document.documentElement.scrollHeight)')
            last = await page.locator('.track').last.bounding_box()
            player = await page.locator('.player-panel').bounding_box()
            assert last['y'] + last['height'] <= player['y'] + 1, (label, width, last, player)
            await page.screenshot(path=str(ARTIFACTS / f'{label}-{width}.png'), full_page=True)

        await page.route('**/api/**', route_handler)
        await page.goto(f'http://127.0.0.1:{server.server_port}')
        await page.wait_for_function('statusLoaded && configLoaded && queueLoaded && libraryLoaded')
        assert await page.locator('.track').count() == 13
        await page.locator('#library-search').fill('01')
        assert await page.locator('.entry').count() == 1
        await page.locator('#select-all').check()
        await page.locator('#library-search').fill('')
        assert await page.locator('.entry input:checked').count() == 1
        scenarios.append('search and selection persist across filtering')

        # Delay metadata while transport and volume remain independent.
        await page.locator('.play-track').nth(2).click()
        await idle()
        append_gate = asyncio.Event()
        count = len(queue_posts)
        await page.locator('#add-selected').click()
        await page.wait_for_function("pending.has('queue')")
        assert await page.locator('#stop').is_enabled()
        assert await page.locator('#volume').is_enabled()
        before = poll_count
        await poll()
        assert poll_count > before
        await page.locator('#volume').fill('55')
        await page.locator('#volume').dispatch_event('change')
        await page.wait_for_function('!pending.has("volume")')
        await page.locator('#stop').click()
        await page.wait_for_function('!pending.has("stop")')
        append_gate.set()
        await idle()
        append_gate = None
        assert len(queue_posts) == count + 1
        assert await page.locator('.track').count() == 14
        assert state['active'] is None
        assert 'Готово к запуску' in await page.locator('#play-state').inner_text()
        assert 'Добавлено в очередь: 1' in await page.locator('#message').inner_text()
        scenarios.append('delayed append keeps polling, Stop and volume available; response preserves Stop')

        # A late response from Start cannot undo a more recent Stop.
        start_gate = asyncio.Event()
        await page.locator('#play').click()
        await page.wait_for_function('pending.has("transport")')
        assert await page.locator('#stop').is_enabled()
        await page.locator('#stop').click()
        await page.wait_for_function('!pending.has("stop")')
        start_gate.set()
        await idle()
        start_gate = None
        assert 'Готово к запуску' in await page.locator('#play-state').inner_text()
        scenarios.append('Stop supersedes a delayed Start response')

        # Same queue responses keep DOM and keyboard focus stable.
        first = page.locator('.track').first
        await first.evaluate('(node)=>node.dataset.persist="yes"')
        await first.locator('.play-track').focus()
        await poll()
        assert await first.get_attribute('data-persist') == 'yes'
        assert await first.locator('.play-track').evaluate('(node)=>node===document.activeElement')
        await page.locator('.track').nth(2).locator('.play-track').click()
        await idle()
        current_id = await page.locator('.track.current').get_attribute('data-entry')
        await page.locator('.track.current .move-track').first.focus()
        await page.keyboard.press('Space')
        await idle()
        assert await page.locator('.track.current').get_attribute('data-entry') == current_id
        assert await page.locator('.track.current .move-track').first.evaluate('(node)=>node===document.activeElement')
        conflict_next = True
        await page.locator('.track').last.locator('.move-track').first.click()
        await idle()
        assert 'Очередь изменилась' in await page.locator('#message').inner_text()
        await page.locator('.track.current .remove-track').click()
        assert await page.locator('#confirm-dialog').is_visible()
        assert await page.locator('#confirm-cancel').evaluate('(node)=>node===document.activeElement')
        await page.keyboard.press('Escape')
        assert await page.locator('.track.current').count() == 1
        await page.locator('.track.current .remove-track').click()
        await page.locator('#confirm-action').click()
        await idle()
        assert await page.locator('.track.current').count() == 0
        scenarios.append('stable poll DOM/focus, keyboard reorder, conflict recovery, current-track confirmation')

        # URL validation and loading feedback stay in the source dialog.
        await page.locator('#open-remote').click()
        assert await page.locator('#remote-url').evaluate('(node)=>node===document.activeElement')
        for _ in range(7):
            await page.keyboard.press('Tab')
            # Native dialogs may let Tab reach browser chrome; background page
            # controls must remain unreachable while document focus is active.
            assert await page.evaluate("!document.hasFocus() || document.activeElement.closest('#source-dialog') !== null")
        assert await page.evaluate("getComputedStyle(document.activeElement).outlineWidth === '3px'")
        await page.locator('#remote-url').fill('https://example.com/stream?token=secret-fixture')
        await page.locator('#remote-title').fill('Моё радио')
        await page.locator('#remote-live').check()
        fail_next = True
        await page.locator('#add-url').click()
        await idle()
        assert await page.locator('#remote-error').is_visible()
        assert 'token=secret-fixture' in await page.locator('#remote-url').input_value()
        await page.locator('#add-url').click()
        await idle()
        assert not await page.locator('#source-dialog').is_visible()
        assert await page.locator('#remote-url').input_value() == ''
        assert await page.locator('#open-remote').evaluate('(node)=>node===document.activeElement')
        state['active'] = {'estimatedIndex': len(state['queue']['tracks'])-1, 'playing': False, 'state': 'no_sound', 'finished': False}
        await poll()
        assert 'Подключаем эфир' in await page.locator('#play-state').inner_text()
        assert 'Прямой эфир подключается' in await page.locator('#health-warning').inner_text()
        state['active'] = None
        await poll()
        scenarios.append('source dialog focus, inline error preserves input, radio connecting state')

        await page.get_by_role('button', name='Открыть папку: Альбом', exact=True).click()
        await page.wait_for_function('library.path === "Альбом"')
        assert await page.locator('#library-heading').evaluate('(node)=>node===document.activeElement')
        await page.locator('#up').click()
        await page.wait_for_function('library.path === "."')
        await page.locator('.settings-link').click()
        await page.locator('#station-mode').select_option('ssh')
        assert await page.locator('#ssh-settings').is_visible()
        assert not await page.locator('#adb-settings').is_visible()
        await page.locator('#station-mode').select_option('adb')
        before = calls.count(('config', 'POST'))
        await page.locator('#save-settings').click()
        assert calls.count(('config', 'POST')) == before
        assert 'уже сохранены' in await page.locator('#settings-status').inner_text()
        await page.locator('#music-root').fill('/fixture/NewMusic')
        await page.locator('#save-settings').click()
        assert 'очередь очистится' in await page.locator('#confirm-text').inner_text()
        await page.locator('#confirm-cancel').click()
        assert calls.count(('config', 'POST')) == before
        await page.keyboard.press('Escape')
        assert await page.locator('.settings-link').evaluate('(node)=>node===document.activeElement')
        scenarios.append('folder heading focus, settings disclosure, unchanged save, root-change cancellation')

        files = [{'name': 'test.mp3', 'mimeType': 'audio/mpeg', 'buffer': b'music'}, {'name': 'unrelated.txt', 'mimeType': 'text/plain', 'buffer': b'not audio'}]
        await page.locator('#open-upload').click()
        await page.locator('#upload-add-queue').uncheck()
        before = len(state['queue']['tracks'])
        await page.locator('#upload-input').set_input_files(files)
        await idle()
        assert 'Сохранено файлов: 1' in await page.locator('#upload-progress').inner_text()
        assert 'Переименован: test (1).mp3' in await page.locator('#upload-results').inner_text()
        assert len(state['queue']['tracks']) == before
        await page.locator('#upload-add-queue').check()
        await page.locator('#upload-input').set_input_files(files)
        await idle()
        assert len(state['queue']['tracks']) == before + 1
        assert 'Пропущен: broken.mp3' in await page.locator('#upload-results').inner_text()
        upload_gate = asyncio.Event()
        await page.locator('#upload-input').set_input_files(files)
        await page.wait_for_function('pending.has("upload")')
        await page.locator('#cancel-upload').click()
        await idle()
        assert 'Передача отменена' in await page.locator('#upload-progress').inner_text()
        upload_gate.set()
        upload_gate = None
        await page.keyboard.press('Escape')
        assert uploads_to_queue[:2] == [False, True]
        scenarios.append('upload library-only/append modes, rename and partial results, cancellation')

        offline = True
        await poll()
        assert await page.locator('#retry-status').is_visible()
        assert 'Последняя проверка' in await page.locator('#health-warning').inner_text()
        offline = False
        await page.locator('#retry-status').click()
        await page.wait_for_function('connectionError === ""')
        scenarios.append('offline retains last state; explicit retry recovers')

        for width, height in [(320, 740), (390, 844), (768, 1024), (1440, 900), (1920, 1080)]:
            await check_geometry(width, height, 'ordinary')
        state['queue']['tracks'][0].update({'title': 'Длинное название без пробелов ' + 'Ы' * 450, 'artist': 'Артист' * 50, 'path': 'https://example.com/' + 'segment/' * 40 + 'file.mp3?token=' + 'secret' * 200})
        state['queue']['version'] += 1
        state['active'] = {'estimatedIndex': 0, 'playing': True, 'state': 'playing', 'finished': False}
        await poll()
        for width, height in [(320, 740), (390, 844), (768, 1024), (1440, 900), (1920, 1080)]:
            await check_geometry(width, height, 'long')
        assert 'token=' not in await page.locator('#now-playing').inner_text()
        await page.locator('#track-details').click()
        assert 'token=' in await page.locator('#metadata-path').input_value()
        await page.keyboard.press('Escape')
        # Toast must stay on screen without moving content, even at the page end.
        before_y = await page.evaluate('scrollY')
        await page.evaluate("message('Проверка видимой обратной связи')")
        toast = await page.locator('#message').bounding_box()
        assert toast['y'] >= 0 and toast['y'] + toast['height'] <= 1080
        assert await page.evaluate('scrollY') == before_y
        scenarios.append('five viewport sizes, ordinary/long fixtures, reachable last row, bounded player, toast without shift')

        await page.locator('#clear-queue').click()
        await page.locator('#confirm-cancel').click()
        assert await page.locator('.track').count() > 0
        await page.locator('#clear-queue').click()
        await page.locator('#confirm-action').click()
        await idle()
        assert await page.locator('.track').count() == 0
        assert await page.locator('#play').is_disabled()
        assert await page.locator('#clear-queue').is_disabled()
        assert not errors, errors
        scenarios.append('clear confirmation and final empty state')
        # Slow first load and partial failure must not masquerade as an empty collection.
        loading_page = await browser.new_page(viewport={'width': 390, 'height': 844})
        loading_page.on('pageerror', lambda error: errors.append(str(error)))
        initial_gate = asyncio.Event()
        library_fails = True

        async def loading_route(route):
            path = urlsplit(route.request.url).path.removeprefix('/api/')
            if path in ['config', 'library', 'queue']:
                await initial_gate.wait()
            if path == 'status':
                data, code = state, 200
            elif path == 'library' and not library_fails:
                data, code = {'path': '.', 'entries': entries}, 200
            else:
                data, code = {'error': 'Ошибка первого подключения'}, 503
            await route.fulfill(status=code, content_type='application/json', body=json.dumps(data))

        await loading_page.route('**/api/**', loading_route)
        await loading_page.goto(f'http://127.0.0.1:{server.server_port}')
        assert 'Открываем' in await loading_page.locator('#library-list').inner_text()
        assert 'Открываем' in await loading_page.locator('#queue-list').inner_text()
        assert await loading_page.locator('#save-settings').is_disabled()
        initial_gate.set()
        await loading_page.wait_for_function('statusLoaded')
        assert 'Библиотека недоступна' in await loading_page.locator('#library-list').inner_text()
        await loading_page.locator('.settings-link').click()
        assert await loading_page.locator('#settings-error').is_visible()
        assert await loading_page.locator('#save-settings').is_disabled()
        await loading_page.keyboard.press('Escape')
        library_fails = False
        await loading_page.locator('#refresh').click()
        await loading_page.wait_for_function('libraryLoaded')
        assert await loading_page.locator('.entry').count() == len(entries)
        await loading_page.close()
        assert not errors, errors
        scenarios.append('slow initial load, config-save guard, unavailable library and refresh recovery')
        output = {'result': 'PASS', 'browser': browser.version, 'apiCalls': len(calls), 'statusPolls': poll_count, 'queuePosts': len(queue_posts), 'scenarios': scenarios, 'geometry': geometry, 'jsErrors': errors, 'screenshots': str(ARTIFACTS), 'pixelInspection': 'NOT_RUN; screenshots generated, not visually inspected'}
        (ARTIFACTS / 'observations.json').write_text(json.dumps(output, ensure_ascii=False, indent=2))
        print(json.dumps(output, ensure_ascii=False))
        await browser.close()


try:
    asyncio.run(main())
finally:
    server.shutdown()
