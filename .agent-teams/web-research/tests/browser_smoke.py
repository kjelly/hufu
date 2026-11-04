"""Manual MCP browser smoke test; requires installed Chromium and network access."""

import json
import os
import queue
import signal
import subprocess
import threading
import time

root = os.environ['TMPDIR']
results = []
for track in ['researcher', 'countercheck', 'verifier']:
    with open(root + '/browser-smoke-stderr.log', 'a') as errors:
        process = subprocess.Popen([
            'npx', '-y', '@playwright/mcp@0.0.84',
            '--headless', '--isolated', '--browser', 'chromium', '--image-responses', 'omit',
            '--snapshot-mode', 'none', '--codegen', 'none', '--no-webmcp',
        ], stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=errors, text=True,
           start_new_session=True, env={**os.environ, 'PLAYWRIGHT_MCP_OUTPUT_DIR': root, 'PLAYWRIGHT_SKIP_BROWSER_GC': '1'})
        received = queue.Queue()
        def read_messages():
            try:
                for line in process.stdout:
                    received.put(json.loads(line))
            except Exception as error:
                received.put(error)
            finally:
                received.put(EOFError('MCP stdout closed'))
        reader = threading.Thread(target=read_messages, daemon=True)
        reader.start()
        def send(message):
            process.stdin.write(json.dumps({'jsonrpc': '2.0', **message}) + '\n')
            process.stdin.flush()
        def call(number, method, params):
            send({'id': number, 'method': method, 'params': params})
            deadline = time.monotonic() + 50
            while True:
                message = received.get(timeout=max(0.1, deadline - time.monotonic()))
                if isinstance(message, Exception):
                    raise message
                if message.get('id') != number:
                    continue
                if 'error' in message:
                    raise RuntimeError(message['error'])
                return message['result']
        try:
            initialized = call(1, 'initialize', {'protocolVersion': '2024-11-05',
                'capabilities': {}, 'clientInfo': {'name': 'hufu-browser-smoke', 'version': '1'}})
            send({'method': 'notifications/initialized'})
            navigation = call(2, 'tools/call', {'name': 'browser_navigate', 'arguments': {'url': 'https://example.com/'}})
            if navigation.get('isError'):
                raise RuntimeError(navigation)
            snapshot = call(3, 'tools/call', {'name': 'browser_snapshot', 'arguments': {}})
            if snapshot.get('isError'):
                raise RuntimeError(snapshot)
            text = '\n'.join(x.get('text', '') for x in snapshot['content'])
            with open(root + '/browser-snapshot-' + track + '.txt', 'w') as output:
                output.write(text)
            snapshot_body = text.split('### Snapshot', 1)[-1] if '### Snapshot' in text else ''
            if 'This domain is for use in documentation examples' not in snapshot_body or '- Page URL: https://example.com/' not in text:
                raise RuntimeError('Snapshot lacks expected page identity or content: ' + text)
            result = {'track': track, 'navigate': 'passed', 'snapshot': 'passed', 'url': 'https://example.com/', 'protocol': initialized['protocolVersion']}
            results.append(result)
            print(json.dumps(result), flush=True)
        finally:
            process.stdin.close()
            try:
                process.wait(timeout=5)
            except subprocess.TimeoutExpired:
                os.killpg(process.pid, signal.SIGTERM)
                try:
                    process.wait(timeout=3)
                except subprocess.TimeoutExpired:
                    os.killpg(process.pid, signal.SIGKILL)
                    process.wait()
            reader.join(timeout=3)
            process.stdout.close()
with open(root + '/browser-smoke.json', 'w') as output:
    json.dump(results, output, indent=2)
