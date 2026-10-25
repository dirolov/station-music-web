import asyncio,json,threading,functools,http.server,pathlib,os,shutil,tempfile
from playwright.async_api import async_playwright
# All API calls are mocked: this check never contacts the Station.
ROOT=pathlib.Path(__file__).resolve().parents[1]
ARTIFACTS=pathlib.Path(tempfile.mkdtemp(prefix='station-ui-smoke-'))
CHROMIUM=os.environ.get('CHROMIUM_PATH') or shutil.which('chromium') or shutil.which('chromium-browser')
if not CHROMIUM: raise RuntimeError('Set CHROMIUM_PATH to a Chromium executable')
class QuietHandler(http.server.SimpleHTTPRequestHandler):
 def log_message(self,*args): pass
server=http.server.ThreadingHTTPServer(('127.0.0.1',0),functools.partial(QuietHandler,directory=str(ROOT/'static')))
threading.Thread(target=server.serve_forever,daemon=True).start()
async def main():
 async with async_playwright() as p:
  browser=await p.chromium.launch(executable_path=CHROMIUM,headless=True,args=['--no-sandbox'])
  page=await browser.new_page(viewport={'width':1440,'height':1100},device_scale_factor=1)
  errors=[]; page.on('pageerror',lambda error:errors.append(str(error)))
  state={'queue':None,'active':None,'volume':{'percent':40},'stationReady':True,'stationMode':'adb','stationTarget':'192.168.1.7:5555','publicBaseResolved':'http://192.168.1.9:8080'}
  cfg={'musicRoot':'/home/dirolov/Music','publicBase':'http://192.168.1.9:8080','stationMode':'adb','stationSerial':'192.168.1.7:5555','stationHost':'','stationPort':2222,'stationUser':'root','stationReady':True}
  base_entries=[{'name':'Exmilitary','path':'Exmilitary','kind':'dir'},{'name':'Плейлист на вечер.m3u','path':'Плейлист на вечер.m3u','kind':'playlist'},{'name':'01 — Первый трек.mp3','path':'01 — Первый трек.mp3','kind':'audio'},{'name':'02 — Второй трек.flac','path':'02 — Второй трек.flac','kind':'audio'}]
  tracks=[{'id':str(i),'path':f'Exmilitary/{i}.mp3','title':title,'artist':'Death Grips','album':'Exmilitary','duration':185+i*23} for i,title in enumerate(['Beware','Guillotine','Spread Eagle Cross the Block','Lord of the Game','Takyon (Death Yon)','Cut Throat','Klink','Culture Shock','5D','Thru the Walls','Known for It','I Want It I Need It','Blood Creepin'])]
  remote_tracks=[{'id':'radio','path':'https://example.com/stream','title':'Моё радио','artist':'','album':'','duration':0,'live':True}]
  calls=[]; queue_posts=[]; fail_next=False; status_polls=0
  async def route_handler(route):
   nonlocal fail_next,status_polls
   req=route.request; path=req.url.split('/api/',1)[1]; calls.append((path,req.method))
   data={}; code=200
   if path=='config': data=cfg
   elif path.startswith('library?'):
    from urllib.parse import parse_qs,urlsplit
    folder=parse_qs(urlsplit(req.url).query).get('path',['.'])[0]
    data={'path':folder,'parent':'.','entries':base_entries if folder=='.' else [{'name':t['title']+'.mp3','path':t['path'],'kind':'audio'} for t in tracks]}
   elif path=='status':
    status_polls+=1
    data=json.loads(json.dumps(state))
    await asyncio.sleep(.7 if status_polls==1 else .02)
   elif path=='queue' and req.method=='GET': data={'queue':state['queue']}
   elif path=='queue' and req.method=='POST':
    await asyncio.sleep(.35)
    if fail_next: code=400; data={'error':'Тестовая ошибка чтения файла'}; fail_next=False
    elif req.post_data_json.get('url') is not None:
     queue_posts.append(req.post_data_json); state['queue']={'id':'remote','tracks':remote_tracks}; state['active']=None
     data={'queue':state['queue'],'added':1,'skipped':[]}
    else:
     data={'queue':{'id':str(len(calls)),'tracks':tracks},'added':len(tracks),'skipped':[]};state['queue']=data['queue'];state['active']=None
   elif path=='queue' and req.method=='DELETE': state['queue']=None; state['active']=None;data={'queue':None}
   elif path=='control':
    body=req.post_data_json; index=body.get('index',0)
    state['active']=None if body['action']=='stop' else {'estimatedIndex':index,'playing':True,'state':'playing','finished':False,'remoteProcess':True,'audioState':'RUNNING','audioAdvancing':True}
    data=state
   elif path=='upload':
    assert b'unrelated.txt' not in req.post_data_buffer
    state['queue']={'id':'upload','tracks':tracks[:1]};state['active']=None
    data={'queue':state['queue'],'added':1,'saved':['test.mp3'],'skipped':[]}
   else: raise Exception(path)
   await route.fulfill(status=code,content_type='application/json',body=json.dumps(data))
  await page.route('**/api/**',route_handler)
  await page.goto(f'http://127.0.0.1:{server.server_port}')
  await page.locator('.add-entry').first.wait_for()
  await page.locator('#remote-url').fill('https://example.com/stream?token=test')
  await page.locator('#remote-title').fill('Моё радио')
  await page.locator('#remote-live').check()
  await page.locator('#add-url').click()
  assert await page.locator('#add-url').is_disabled()
  await page.wait_for_function("document.querySelectorAll('.track').length===1")
  assert queue_posts[-1]=={'action':'append','url':'https://example.com/stream?token=test','title':'Моё радио','live':True}
  assert await page.locator('#remote-url').input_value()==''
  assert 'Добавлено в очередь: 1' in await page.locator('#message').inner_text()
  state['active']={'estimatedIndex':0,'playing':False,'state':'no_sound','finished':False}
  await page.wait_for_function("document.querySelector('#play-state').textContent==='Подключаем эфир…'")
  assert await page.locator('#play-state').get_attribute('class')=='chip warn'
  assert await page.locator('#health-warning').is_visible()
  assert 'Прямой эфир подключается' in await page.locator('#health-warning').inner_text()
  state['active']=None
  assert 'Прямых эфиров: 1' in await page.locator('#queue-summary').inner_text()
  assert await page.locator('.track .duration').inner_text()=='Эфир'
  assert await page.locator('#add-selected').is_disabled()
  await page.locator('#library-search').fill('Первый')
  assert await page.locator('.entry').count()==1
  await page.locator('#select-all').check()
  assert '(1)' in await page.locator('#add-selected').inner_text()
  await page.locator('#library-search').fill('')
  assert await page.locator('.entry input:checked').count()==1
  await page.locator('#add-selected').click()
  assert await page.locator('#message').get_attribute('class')=='message busy'
  assert await page.locator('#add-folder').is_disabled()
  await page.wait_for_function("document.querySelectorAll('.track').length===13")
  assert 'Добавлено в очередь: 13' in await page.locator('#message').inner_text()
  await page.wait_for_timeout(900)
  assert await page.locator('.track').count()==13, 'Stale status overwrote added queue'
  await page.locator('#queue-list').evaluate('(node)=>node.dataset.persist="yes"')
  await page.locator('.track').first.evaluate('(node)=>node.dataset.persist="yes"')
  await page.wait_for_timeout(1900)
  assert await page.locator('.track').first.get_attribute('data-persist')=='yes','poll rebuilt queue'
  await page.locator('.settings-link').click()
  assert await page.locator('#settings').get_attribute('open') is not None
  await page.locator('#station-mode').select_option('ssh')
  assert await page.locator('#ssh-settings').is_visible()
  assert not await page.locator('#adb-settings').is_visible()
  await page.locator('#station-mode').select_option('adb')
  await page.locator('#settings').evaluate('(node)=>node.open=false')
  await page.locator('.play-track').nth(2).click()
  await page.wait_for_function("document.querySelector('#play-state').textContent==='Играет'")
  assert await page.locator('.track.current .track-name').inner_text()=='Spread Eagle Cross the Block\nDeath Grips · Exmilitary'
  assert await page.locator('#play').is_disabled()
  await page.locator('#stop').click()
  await page.wait_for_function("document.querySelector('#play-state').textContent==='Остановлено'")
  fail_next=True
  await page.locator('#add-folder').click()
  await page.wait_for_function("document.querySelector('#message').classList.contains('error')")
  await page.wait_for_timeout(1800)
  assert 'Тестовая ошибка' in await page.locator('#message').inner_text()
  await page.get_by_role('button',name='Открыть папку: Exmilitary',exact=True).click()
  await page.wait_for_function("document.querySelectorAll('.entry').length===13")
  assert not await page.locator('#up').is_disabled()
  await page.locator('#add-folder').click()
  await page.wait_for_function("document.querySelector('#message').className==='message info'")
  await page.screenshot(path=str(ARTIFACTS/'desktop.png'),full_page=True)
  for width in [390,320]:
   await page.set_viewport_size({'width':width,'height':844})
   assert await page.evaluate('document.documentElement.scrollWidth <= innerWidth'),f'overflow at {width}'
   await page.screenshot(path=str(ARTIFACTS/f'mobile-{width}.png'),full_page=True)
  await page.locator('#upload-input').set_input_files([{'name':'test.mp3','mimeType':'audio/mpeg','buffer':b'test music'},{'name':'unrelated.txt','mimeType':'text/plain','buffer':b'not audio'}])
  await page.wait_for_function("document.querySelector('#message').textContent.includes('Сохранено файлов: 1')")
  assert await page.locator('.track').count()==1
  await page.locator('#clear-queue').click()
  await page.wait_for_function("document.querySelectorAll('.track').length===0")
  assert await page.locator('#play').is_disabled()
  assert await page.locator('#clear-queue').is_disabled()
  assert not errors,errors
  print(json.dumps({'result':'PASS','api_calls':len(calls),'queue_posts':len(queue_posts),'status_polls':status_polls,'desktop':1440,'mobile':[390,320],'errors':errors,'screenshots':str(ARTIFACTS)}))
  await browser.close()
try: asyncio.run(main())
finally: server.shutdown()
