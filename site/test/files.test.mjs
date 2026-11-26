// Checks internal links, media files, sitemaps, agent docs, and asset exclusions for site/.
import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../..');
const SITE = path.join(ROOT, 'site');
const HTML_PAGES = ['index.html', 'about/index.html', 'how/index.html', 'hosts/index.html', 'privacy/index.html', 'contact/index.html'];
const AGENT_ROUTES = new Set(['/setup', '/setup.md', '/join', '/join.md', '/use', '/use.md']);
const REPO_RAW = /^https:\/\/(?:raw\.githubusercontent\.com\/a-Fig\/agent-box\/main|github\.com\/a-Fig\/agent-box\/(?:blob|raw)\/main)\/([^#?]+)/;

function stripJsoncComments(raw) {
  return raw.replace(/^\s*\/\/.*$/gm, '');
}

test('wrangler.jsonc and .assetsignore cover required routes and non-asset files', () => {
  const wrangler = JSON.parse(stripJsoncComments(fs.readFileSync(path.join(SITE, 'wrangler.jsonc'), 'utf8')));
  assert.equal(wrangler.name, 'agent-box-site');
  assert.equal(wrangler.routes[0].pattern, 'agent-box.afig.dev');
  for (const r of ['/', '/*.mp4', '/setup', '/setup.md', '/join', '/join.md', '/use', '/use.md']) {
    assert.ok(wrangler.assets.run_worker_first.includes(r), `run_worker_first missing ${r}`);
  }
  const ignore = new Set(fs.readFileSync(path.join(SITE, '.assetsignore'), 'utf8').trim().split(/\r?\n/));
  for (const dir of ['wrangler.jsonc', '.assetsignore', 'src', 'test', 'package.json', 'package-lock.json', 'node_modules']) {
    assert.ok(ignore.has(dir), `.assetsignore missing ${dir}`);
  }
});

test('all HTML internal links, assets, and GitHub file links resolve', () => {
  for (const page of HTML_PAGES) {
    const html = fs.readFileSync(path.join(SITE, page), 'utf8');
    // Match href/src/poster/data-src/data-last values
    for (const m of html.matchAll(/(?:href|src|poster|data-src|data-last)="([^"]+)"/g)) {
      const ref = m[1];
      if (ref.startsWith('#') || ref.startsWith('data:')) continue;
      if (ref.startsWith('https://agent-box.afig.dev/')) {
        const rel = new URL(ref).pathname;
        if (AGENT_ROUTES.has(rel)) continue;
        const target = rel.endsWith('/') ? path.join(SITE, rel.slice(1), 'index.html') : path.join(SITE, rel.slice(1));
        assert.ok(fs.existsSync(target), `${page}: missing canonical/meta target ${ref}`);
        continue;
      }
      const rawMatch = REPO_RAW.exec(ref);
      if (rawMatch) {
        const repoPath = path.join(ROOT, rawMatch[1]);
        assert.ok(fs.existsSync(repoPath), `${page}: broken GitHub repo link ${ref}`);
        continue;
      }
      if (ref.startsWith('http://') || ref.startsWith('https://')) continue;
      const clean = ref.split('#')[0].split('?')[0];
      if (AGENT_ROUTES.has(clean)) continue;
      const rel = clean.startsWith('/') ? clean.slice(1) : clean;
      const target = rel === '' ? path.join(SITE, 'index.html') : rel.endsWith('/') ? path.join(SITE, rel, 'index.html') : path.join(SITE, rel);
      assert.ok(fs.existsSync(target), `${page}: missing local target ${ref} -> ${target}`);
    }
    // Also check inline CSS mask url(/os/...) references
    for (const m of html.matchAll(/url\((\/[^)]+)\)/g)) {
      const target = path.join(SITE, m[1].slice(1));
      assert.ok(fs.existsSync(target), `${page}: missing CSS mask url ${m[1]}`);
    }
  }
});

test('every page has consistent footer nav and sitemap coverage', () => {
  const sitemap = fs.readFileSync(path.join(SITE, 'sitemap.xml'), 'utf8');
  const locs = new Set([...sitemap.matchAll(/<loc>([^<]+)<\/loc>/g)].map(m => m[1]));
  for (const page of HTML_PAGES) {
    const html = fs.readFileSync(path.join(SITE, page), 'utf8');
    const urlPath = page === 'index.html' ? '/' : `/${page.replace(/index\.html$/, '')}`;
    assert.ok(locs.has(`https://agent-box.afig.dev${urlPath}`), `sitemap.xml missing ${urlPath}`);
    assert.match(html, /class="site-foot"/, `${page}: missing footer`);
    for (const navPath of ['/about/', '/how/', '/hosts/', '/privacy/', '/contact/', 'https://github.com/a-Fig/agent-box']) {
      assert.ok(html.includes(`href="${navPath}"`), `${page}: footer missing ${navPath}`);
    }
  }
  for (const route of ['/setup', '/join', '/use']) {
    assert.ok(locs.has(`https://agent-box.afig.dev${route}`), `sitemap.xml missing ${route}`);
    assert.ok(fs.existsSync(path.join(ROOT, 'docs/agents', `${route.slice(1)}.md`)), `missing docs/agents${route}.md`);
  }
});

test('brand icons, OG card, videos, posters, and agent/os attribution files exist', () => {
  const required = [
    'favicon.svg', 'favicon-32.png', 'favicon.ico', 'apple-touch-icon.png', 'og.jpg',
    'how-wide.mp4', 'how-wide-first.webp', 'how-wide-last.webp',
    'how-tall.mp4', 'how-tall-first.webp', 'how-tall-last.webp',
    'ui-overview.webp',
    'ui-mode.mp4', 'ui-mode-first.webp', 'ui-mode-last.webp',
    'ui-job.mp4', 'ui-job-first.webp', 'ui-job-last.webp',
    'ui-history.mp4', 'ui-history-first.webp', 'ui-history-last.webp',
  ];
  for (const file of required) {
    const p = path.join(SITE, file);
    assert.ok(fs.existsSync(p), `missing media/icon asset: ${file}`);
    assert.ok(fs.statSync(p).size > 0, `empty asset: ${file}`);
  }
  for (const dir of ['agents', 'os']) {
    const sources = fs.readFileSync(path.join(SITE, dir, 'SOURCES.txt'), 'utf8');
    for (const name of fs.readdirSync(path.join(SITE, dir))) {
      if (name === 'SOURCES.txt') continue;
      assert.ok(sources.includes(name), `${dir}/SOURCES.txt missing entry for ${name}`);
    }
  }
});

test('site package-lock.json and llms.txt are valid', () => {
  const lock = JSON.parse(fs.readFileSync(path.join(SITE, 'package-lock.json'), 'utf8'));
  assert.ok(lock.packages?.['node_modules/wrangler'], 'package-lock.json missing wrangler');
  const llms = fs.readFileSync(path.join(SITE, 'llms.txt'), 'utf8');
  for (const m of llms.matchAll(new RegExp(REPO_RAW.source, 'g'))) {
    assert.ok(fs.existsSync(path.join(ROOT, m[1])), `llms.txt points to missing repo path: ${m[1]}`);
  }
});
