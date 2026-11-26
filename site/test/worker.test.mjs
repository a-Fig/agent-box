// Unit tests for src/accept.js, src/notfound.js, and src/worker.js routing & Range handling.
import test from 'node:test';
import assert from 'node:assert/strict';
import worker from '../src/worker.js';
import { prefersMarkdown } from '../src/accept.js';
import { NOT_FOUND_MD } from '../src/notfound.js';

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

test('root serves HTML with Vary+Link for browsers and markdown setup for agents', async () => {
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
    // 1. Cold + upstream failure -> bundled deployed-copy
    globalThis.fetch = async () => { throw new Error('offline'); };
    const env = { ASSETS: { fetch: async () => new Response('unused', { status: 404 }) } };
    const rFallback = await worker.fetch(new Request('https://agent-box.afig.dev/join.md'), env, makeCtx());
    assert.equal(rFallback.headers.get('X-Agent-Box-Source'), 'deployed-copy');
    const fbText = await rFallback.text();
    assert.match(fbText, /GitHub couldn't be reached just now/);
    assert.match(fbText, /agent-box/i);

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

    // 5. HEAD and POST method rules
    const rHead = await worker.fetch(new Request('https://agent-box.afig.dev/setup', { method: 'HEAD' }), env, makeCtx());
    assert.equal(rHead.status, 200);
    assert.equal(await rHead.text(), '');
    const rPost = await worker.fetch(new Request('https://agent-box.afig.dev/setup', { method: 'POST' }), env, makeCtx());
    assert.equal(rPost.status, 405);
    assert.equal(rPost.headers.get('Allow'), 'GET, HEAD');
  } finally {
    globalThis.caches = prevCaches;
    globalThis.fetch = prevFetch;
  }
});

test('mp4 requests support byte ranges and 404 adapts to Accept header', async () => {
  const bytes = new Uint8Array([0, 1, 2, 3, 4, 5, 6, 7, 8, 9]);
  const env = {
    ASSETS: {
      fetch: async req => {
        if (new URL(req.url).pathname === '/how-wide.mp4') {
          return new Response(bytes, { status: 200, headers: { 'Content-Type': 'video/mp4' } });
        }
        return new Response('missing', { status: 404, headers: { 'Content-Type': 'text/plain' } });
      },
    },
  };

  const full = await worker.fetch(new Request('https://agent-box.afig.dev/how-wide.mp4'), env, makeCtx());
  assert.equal(full.status, 200);
  assert.equal(full.headers.get('Accept-Ranges'), 'bytes');

  const partial = await worker.fetch(new Request('https://agent-box.afig.dev/how-wide.mp4', { headers: { Range: 'bytes=2-5' } }), env, makeCtx());
  assert.equal(partial.status, 206);
  assert.equal(partial.headers.get('Content-Range'), 'bytes 2-5/10');
  assert.deepEqual([...new Uint8Array(await partial.arrayBuffer())], [2, 3, 4, 5]);

  const badRange = await worker.fetch(new Request('https://agent-box.afig.dev/how-wide.mp4', { headers: { Range: 'bytes=20-25' } }), env, makeCtx());
  assert.equal(badRange.status, 416);

  const nfHtml = await worker.fetch(new Request('https://agent-box.afig.dev/nope'), env, makeCtx());
  assert.equal(nfHtml.status, 404);
  assert.equal(await nfHtml.text(), 'missing');

  const nfMd = await worker.fetch(new Request('https://agent-box.afig.dev/nope', { headers: { Accept: 'text/markdown' } }), env, makeCtx());
  assert.equal(nfMd.status, 404);
  assert.equal(nfMd.headers.get('Content-Type'), 'text/markdown; charset=utf-8');
  assert.equal(await nfMd.text(), NOT_FOUND_MD);
});
