"""Real CS2/OBS integration proof, reusing the dedicated headless session.
Run with Project Autocs2video's Python environment; no secrets are printed.
"""
import json, os, signal, subprocess, sys, time, urllib.request
from pathlib import Path

ROOT = Path(__file__).resolve().parent
sys.path.insert(0, '/home/water/Project Autocs2video')
from recorder.headless import HeadlessSession
from recorder.vconsole import Netcon

session = HeadlessSession(ROOT / 'headless.json')
obs = session.obs()
assert not obs.get_record_status().output_active, 'Existing recording is active'
obs.disconnect()
data = ROOT / ('run-' + time.strftime('%Y%m%d-%H%M%S'))
data.mkdir(mode=0o700)
log = open(data / 'server.log', 'w')
os.chmod(data / 'server.log', 0o600)
process = subprocess.Popen(['/tmp/project-replay-native', '-data', str(data), '-listen', '127.0.0.1:17789', '-no-browser'], stdout=log, stderr=log)
try:
    for _ in range(100):
        if (data / 'access-token').exists(): break
        time.sleep(.1)
    token = (data / 'access-token').read_text().strip()
    def api(path, value=None):
        req = urllib.request.Request('http://127.0.0.1:17789/api/' + path, data=None if value is None else json.dumps(value).encode(), headers={'Authorization': 'Bearer ' + token, 'Content-Type': 'application/json'})
        with urllib.request.urlopen(req, timeout=10) as r: return json.load(r)
    config = api('state')['state']['config']
    config.update(mode='live', tracking_mode='native', strict=False, match='native-proof', map='de_mirage', netcon='127.0.0.1:2121', obs_url='ws://127.0.0.1:4466', obs_password=os.environ['OBS_PASSWORD'], session_lock='/home/water/Project Autocs2video/jobs/headless-session/session.lock')
    api('config', config)
    epoch = api('state')['state']['config']['epoch']
    n = Netcon(2121, 5)
    n.response('demo_pause')
    kill_tick = int(os.environ.get('REPLAY_PROOF_TICK', '14633'))
    n.response('demo_gototick ' + str(kill_tick-633))
    time.sleep(1)
    n.response('demo_ui_mode 0; cl_draw_only_deathnotices 0')
    n.response('demo_resume')
    time.sleep(.4)
    before = time.time()
    tick, total = n.info()
    after = time.time()
    event = next(json.loads(line) for line in (ROOT / 'all-events.jsonl').read_text().splitlines() if json.loads(line)['tick'] == kill_tick)
    target = round((before + after) * 500 + (event['tick'] - tick) / 64 * 1000)
    offset = target - event['time']
    event['time'] = target
    event['epoch'] = epoch
    event['uncertainty'] = .2
    for point in event['utility']['track']: point['time'] += offset
    (data / 'submitted.json').write_text(json.dumps(event, indent=2))
    api('calibrate', {'delta': 8, 'uncertainty': .2})
    api('pause', {'paused': False})
    api('events', event)
    print(json.dumps({'data': str(data), 'start_tick': tick, 'kill_in_seconds': (target / 1000 - time.time()), 'query_ms': (after-before)*1000}), flush=True)
    for _ in range(240):
        result = api('state')['state']
        if result['jobs'] and result['jobs'][-1]['status'] in ('READY','FAILED'):
            (data / 'result.json').write_text(json.dumps(result, indent=2))
            job = result['jobs'][-1]
            print(json.dumps({'status':job['status'], 'reason':job['error'], 'artifacts':job['artifacts']},ensure_ascii=False), flush=True)
            break
        time.sleep(.25)
    else: raise RuntimeError('capture timed out')
    api('pause', {'paused': True})
    n.response('demo_pause')
finally:
    process.send_signal(signal.SIGTERM)
    process.wait(timeout=15)
    log.close()
