// Unit tests for src/accept.js, src/notfound.js, and src/worker.js routing & Range handling.
import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import worker from '../src/worker.js';
import { prefersMarkdown } from '../src/accept.js';
import { NOT_FOUND_MD } from '../src/notfound.js';

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../..');
const JOIN_MD = fs.readFileSync(path.join(ROOT, 'docs/agents/join.md'), 'utf8');

function makeCaches() {
  const store = new Map();
  return {
    store,
    default: {
      async match(key) {
        const hit = store.get(typeof key === 'string' ? key : key.url);
        return hit ? hit.clone() : undefined;
      },
      async put(key, res) {
        store.set(typeof key === 'string' ? key : key.url, res.clone());
      },
    },
  };
}

function makeCtx() {
  const waits = [];
  return { waitUntil: p => waits.push(Promise.resolve(p)), flush: () => Promise.all(waits) };
}

test('prefersMarkdown handles quality values accurately', () => {
  assert.equal(prefersMarkdown(null), false);
  assert.equal(prefersMarkdown('text/html,application/xhtml+xml,*/*;q=0.8'), false);
  assert.equal(prefersMarkdown('text/markdown'), true);
  assert.equal(prefersMarkdown('text/html, text/markdown'), true);
  assert.equal(prefersMarkdown('text/markdown;q=0.1, text/*'), false);
  assert.equal(prefersMarkdown('text/markdown;q=0.8, text/html;q=0.5'), true);
});

test('root serves HTML with Vary+Link for browsers and markdown setup for agents (including HEAD)', async () => {
  const cacheObj = makeCaches();
  const prevCaches = globalThis.caches;
  const prevFetch = globalThis.fetch;
  globalThis.caches = cacheObj;
  globalThis.fetch = async () => new Response('# Fresh Setup\nfrom github', { status: 200 });
  try {
    const env = {
      ASSETS: {
        fetch: async req => new Response(`<html>root:${new URL(req.url).pathname}</html>`, { headers: { 'Content-Type': 'text/html' } }),
      },
    };
    const resHtml = await worker.fetch(new Request('https://agent-box.afig.dev/', { headers: { Accept: 'text/html' } }), env, makeCtx());
    assert.equal(resHtml.status, 200);
    assert.equal(resHtml.headers.get('Vary'), 'Accept');
    assert.match(resHtml.headers.get('Link') || '', /\/setup>; rel="alternate"; type="text\/markdown"/);

    const ctx = makeCtx();
    const resMd = await worker.fetch(new Request('https://agent-box.afig.dev/', { headers: { Accept: 'text/markdown' } }), env, ctx);
    await ctx.flush();
    assert.equal(resMd.status, 200);
    assert.equal(resMd.headers.get('Content-Type'), 'text/markdown; charset=utf-8');
    assert.equal(resMd.headers.get('Vary'), 'Accept');
    assert.equal(resMd.headers.get('X-Agent-Box-Source'), 'github');
    const body = await resMd.text();
    assert.match(body, /This is agent-box's website/);
    assert.match(body, /# Fresh Setup/);

    const headMd = await worker.fetch(new Request('https://agent-box.afig.dev/', { method: 'HEAD', headers: { Accept: 'text/markdown' } }), env, makeCtx());
    assert.equal(headMd.status, 200);
    assert.equal(headMd.headers.get('X-Agent-Box-Source'), 'cache');
    assert.equal(await headMd.text(), '');
  } finally {
    globalThis.caches = prevCaches;
    globalThis.fetch = prevFetch;
  }
});

test('agent markdown endpoints support cache, kept-copy, deployed-copy fallback, HEAD, and 405', async () => {
  const cacheObj = makeCaches();
  const prevCaches = globalThis.caches;
  const prevFetch = globalThis.fetch;
  globalThis.caches = cacheObj;
  try {
    // 1. Cold + upstream failure -> exact bundled deployed-copy
    globalThis.fetch = async () => { throw new Error('offline'); };
    const env = { ASSETS: { fetch: async () => new Response('unused', { status: 404 }) } };
    const rFallback = await worker.fetch(new Request('https://agent-box.afig.dev/join.md'), env, makeCtx());
    assert.equal(rFallback.headers.get('X-Agent-Box-Source'), 'deployed-copy');
    const fbText = await rFallback.text();
    assert.match(fbText, /^> GitHub couldn't be reached just now/);
    assert.ok(fbText.endsWith(JOIN_MD), 'fallback must end with exact bundled docs/agents/join.md content');

    // 2. Upstream ok populates fresh + kept cache
    globalThis.fetch = async () => new Response('# Live Use\nok', { status: 200 });
    const ctx = makeCtx();
    const rGh = await worker.fetch(new Request('https://agent-box.afig.dev/use'), env, ctx);
    await ctx.flush();
    assert.equal(rGh.headers.get('X-Agent-Box-Source'), 'github');
    assert.match(await rGh.text(), /# Live Use/);

    // 3. Cached hit skips network fetch
    globalThis.fetch = async () => { throw new Error('should not be called'); };
    const rCache = await worker.fetch(new Request('https://agent-box.afig.dev/use/'), env, makeCtx());
    assert.equal(rCache.headers.get('X-Agent-Box-Source'), 'cache');

    // 4. Fresh expires -> upstream fails -> kept-copy served
    cacheObj.store.delete('https://agent-box.afig.dev/_cache/fresh/use.md');
    const rKept = await worker.fetch(new Request('https://agent-box.afig.dev/use'), env, makeCtx());
    assert.equal(rKept.headers.get('X-Agent-Box-Source'), 'kept-copy');
    assert.match(await rKept.text(), /last copy fetched from it/);

    // 5. HEAD with working upstream explicitly fetches from GitHub and sends empty body
    globalThis.fetch = async () => new Response('# Live Setup\nhead ok', { status: 200 });
    const headCtx = makeCtx();
    const rHead = await worker.fetch(new Request('https://agent-box.afig.dev/setup', { method: 'HEAD' }), env, headCtx);
    await headCtx.flush();
    assert.equal(rHead.status, 200);
    assert.equal(rHead.headers.get('X-Agent-Box-Source'), 'github');
    assert.equal(await rHead.text(), '');

    // 6. POST rejected with 405
    const rPost = await worker.fetch(new Request('https://agent-box.afig.dev/setup', { method: 'POST' }), env, makeCtx());
    assert.equal(rPost.status, 405);
    assert.equal(rPost.headers.get('Allow'), 'GET, HEAD');
  } finally {
    globalThis.caches = prevCaches;
    globalThis.fetch = prevFetch;
  }
});

test('mp4 requests support closed, suffix, and open-ended byte ranges, and 404 adapts including HEAD', async () => {
  const bytes = new Uint8Array([0, 1, 2, 3, 4, 5, 6, 7, 8, 9]);
  const env = {
    ASSETS: {
      fetch: async req => {
        if (new URL(req.url).pathname === '/how-wide.mp4') {
          if (req.method === 'HEAD') {
            return new Response(null, { status: 200, headers: { 'Content-Type': 'video/mp4', 'Content-Length': '10' } });
          }
          return new Response(bytes, { status: 200, headers: { 'Content-Type': 'video/mp4' } });
        }
        return new Response(req.method === 'HEAD' ? null : 'missing', { status: 404, headers: { 'Content-Type': 'text/plain' } });
      },
    },
  };

  const full = await worker.fetch(new Request('https://agent-box.afig.dev/how-wide.mp4'), env, makeCtx());
  assert.equal(full.status, 200);
  assert.equal(full.headers.get('Accept-Ranges'), 'bytes');

  const headWithRange = await worker.fetch(new Request('https://agent-box.afig.dev/how-wide.mp4', { method: 'HEAD', headers: { Range: 'bytes=2-5' } }), env, makeCtx());
  assert.equal(headWithRange.status, 200);
  assert.equal(headWithRange.headers.get('Accept-Ranges'), 'bytes');
  assert.equal(await headWithRange.text(), '');

  const closed = await worker.fetch(new Request('https://agent-box.afig.dev/how-wide.mp4', { headers: { Range: 'bytes=2-5' } }), env, makeCtx());
  assert.equal(closed.status, 206);
  assert.equal(closed.headers.get('Content-Range'), 'bytes 2-5/10');
  assert.deepEqual([...new Uint8Array(await closed.arrayBuffer())], [2, 3, 4, 5]);

  const suffix = await worker.fetch(new Request('https://agent-box.afig.dev/how-wide.mp4', { headers: { Range: 'bytes=-4' } }), env, makeCtx());
  assert.equal(suffix.status, 206);
  assert.equal(suffix.headers.get('Content-Range'), 'bytes 6-9/10');
  assert.deepEqual([...new Uint8Array(await suffix.arrayBuffer())], [6, 7, 8, 9]);

  const openEnded = await worker.fetch(new Request('https://agent-box.afig.dev/how-wide.mp4', { headers: { Range: 'bytes=7-' } }), env, makeCtx());
  assert.equal(openEnded.status, 206);
  assert.equal(openEnded.headers.get('Content-Range'), 'bytes 7-9/10');
  assert.deepEqual([...new Uint8Array(await openEnded.arrayBuffer())], [7, 8, 9]);

  const badRange = await worker.fetch(new Request('https://agent-box.afig.dev/how-wide.mp4', { headers: { Range: 'bytes=20-25' } }), env, makeCtx());
  assert.equal(badRange.status, 416);

  const nfHtml = await worker.fetch(new Request('https://agent-box.afig.dev/nope'), env, makeCtx());
  assert.equal(nfHtml.status, 404);
  assert.equal(await nfHtml.text(), 'missing');

  const nfHtmlHead = await worker.fetch(new Request('https://agent-box.afig.dev/nope', { method: 'HEAD' }), env, makeCtx());
  assert.equal(nfHtmlHead.status, 404);
  assert.equal(await nfHtmlHead.text(), '');

  const nfMd = await worker.fetch(new Request('https://agent-box.afig.dev/nope', { headers: { Accept: 'text/markdown' } }), env, makeCtx());
  assert.equal(nfMd.status, 404);
  assert.equal(nfMd.headers.get('Content-Type'), 'text/markdown; charset=utf-8');
  assert.equal(await nfMd.text(), NOT_FOUND_MD);

  const nfMdHead = await worker.fetch(new Request('https://agent-box.afig.dev/nope', { method: 'HEAD', headers: { Accept: 'text/markdown' } }), env, makeCtx());
  assert.equal(nfMdHead.status, 404);
  assert.equal(await nfMdHead.text(), '');
});
