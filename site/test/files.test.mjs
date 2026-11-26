// Checks static site links, assets, config, and metadata for Project Fano (site/).
import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { previewAssets } from '../preview.mjs';

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../..');
const SITE = path.join(ROOT, 'site');
const ORIGIN = 'https://fano.afig.dev';
const REPO_RAW = /^https:\/\/(?:raw\.githubusercontent\.com\/a-Fig\/agent-box\/main|github\.com\/a-Fig\/agent-box\/(?:blob|raw)\/main)\/([^#?]+)/;

function stripJsoncComments(raw) {
  return raw.replace(/^\s*\/\/.*$/gm, '');
}

test('wrangler.jsonc and .assetsignore configure static assets for fano.afig.dev', () => {
  const wrangler = JSON.parse(stripJsoncComments(fs.readFileSync(path.join(SITE, 'wrangler.jsonc'), 'utf8')));
  assert.equal(wrangler.name, 'fano-site');
  assert.equal(wrangler.main, undefined, 'starter site should be static-only (no worker main)');
  assert.equal(wrangler.assets?.directory, '.');
  assert.equal(wrangler.routes?.[0]?.pattern, 'fano.afig.dev');
  assert.equal(wrangler.preview_urls, false);

  const ignore = new Set(fs.readFileSync(path.join(SITE, '.assetsignore'), 'utf8').trim().split(/\r?\n/));
  for (const entry of ['BRAND.md', 'wrangler.jsonc', 'wrangler.preview.json', '.assetsignore', '.wrangler/', 'preview.mjs', 'test', 'package.json', 'package-lock.json', 'node_modules']) {
    assert.ok(ignore.has(entry), `.assetsignore missing ${entry}`);
  }
});

test('PR previews use a dedicated Worker and a staging directory', () => {
  const config = JSON.parse(fs.readFileSync(path.join(SITE, 'wrangler.preview.json'), 'utf8'));
  assert.equal(config.name, 'fano-site-preview');
  assert.equal(config.workers_dev, false);
  assert.equal(config.preview_urls, true);
  assert.equal(config.routes, undefined);
  assert.equal(config.assets.directory, './assets');
});

test('PR preview export takes regular public assets with portable paths', () => {
  const object = 'a'.repeat(40);
  const tree = [
    ['100644', 'site/index.html'],
    ['100755', 'site/images/logo.svg'],
    ['120000', 'site/linked.html'],
    ['100644', 'site/NuL.svg'],
    ['100644', 'site/COM1.assets/index.html'],
    ['100644', 'site/images./logo.svg'],
    ['100644', 'site/../other.html'],
    ['100644', 'site/test/private.js'],
    ['100644', 'site/.env'],
    ['100644', 'site/wrangler.jsonc'],
    ['100644', 'other/index.html'],
  ].map(([mode, file]) => `${mode} blob ${object}\t${file}\0`).join('');
  assert.deepEqual(previewAssets(tree), [{ path: 'index.html', object }, { path: 'images/logo.svg', object }]);
});

test('index.html internal links and asset references resolve', () => {
  const html = fs.readFileSync(path.join(SITE, 'index.html'), 'utf8');
  assert.ok(html.includes(`<link rel="canonical" href="${ORIGIN}/">`), 'missing canonical fano.afig.dev URL');
  assert.ok(!html.includes('agent-box.afig.dev'), 'stale agent-box.afig.dev reference in index.html');

  for (const m of html.matchAll(/(?:href|src)="([^"]+)"/g)) {
    const ref = m[1];
    if (ref.startsWith('#') || ref.startsWith('data:')) continue;
    if (ref.startsWith(`${ORIGIN}/`)) {
      const rel = new URL(ref).pathname.slice(1);
      const target = rel === '' ? path.join(SITE, 'index.html') : path.join(SITE, rel);
      assert.ok(fs.existsSync(target), `missing canonical/meta target ${ref}`);
      continue;
    }
    const rawMatch = REPO_RAW.exec(ref);
    if (rawMatch) {
      assert.ok(fs.existsSync(path.join(ROOT, rawMatch[1])), `broken GitHub repo link ${ref}`);
      continue;
    }
    if (ref.startsWith('http://') || ref.startsWith('https://')) continue;
    const clean = ref.split('#')[0].split('?')[0];
    const rel = clean.startsWith('/') ? clean.slice(1) : clean;
    const target = rel === '' ? path.join(SITE, 'index.html') : path.join(SITE, rel);
    assert.ok(fs.existsSync(target), `missing local target ${ref} -> ${target}`);
  }
});

test('icons, OG card, robots.txt, sitemap.xml, llms.txt, and lockfile are valid', () => {
  for (const file of ['favicon.svg', 'favicon-32.png', 'favicon.ico', 'apple-touch-icon.png', 'og.jpg', 'BRAND.md']) {
    const p = path.join(SITE, file);
    assert.ok(fs.existsSync(p), `missing site asset: ${file}`);
    assert.ok(fs.statSync(p).size > 0, `empty site asset: ${file}`);
  }

  const robots = fs.readFileSync(path.join(SITE, 'robots.txt'), 'utf8');
  assert.ok(robots.includes(`Sitemap: ${ORIGIN}/sitemap.xml`));

  const sitemap = fs.readFileSync(path.join(SITE, 'sitemap.xml'), 'utf8');
  const locs = new Set([...sitemap.matchAll(/<loc>([^<]+)<\/loc>/g)].map(m => m[1]));
  assert.ok(locs.has(`${ORIGIN}/`), 'sitemap.xml missing /');
  assert.ok(locs.has(`${ORIGIN}/llms.txt`), 'sitemap.xml missing /llms.txt');

  const llms = fs.readFileSync(path.join(SITE, 'llms.txt'), 'utf8');
  assert.ok(!llms.includes('agent-box.afig.dev'), 'stale agent-box.afig.dev reference in llms.txt');
  for (const m of llms.matchAll(new RegExp(REPO_RAW.source, 'g'))) {
    assert.ok(fs.existsSync(path.join(ROOT, m[1])), `llms.txt points to missing repo path: ${m[1]}`);
  }

  const lock = JSON.parse(fs.readFileSync(path.join(SITE, 'package-lock.json'), 'utf8'));
  assert.ok(lock.packages?.['node_modules/wrangler'], 'package-lock.json missing wrangler');
});
