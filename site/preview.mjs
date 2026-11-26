import { execFileSync } from 'node:child_process';
import { copyFileSync, mkdirSync, mkdtempSync, realpathSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, extname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

const repo = 'a-Fig/agent-box';
const root = fileURLToPath(new URL('..', import.meta.url));
const assetExtensions = new Set(['.css', '.gif', '.html', '.ico', '.jpeg', '.jpg', '.js', '.png', '.svg', '.txt', '.webp', '.woff2', '.xml']);

function isAsset(path) {
  const parts = path.split('/');
  return parts.every((part) => /^[a-z0-9][a-z0-9_.-]*$/i.test(part)) &&
    !['node_modules', 'test'].includes(parts[0].toLowerCase()) && assetExtensions.has(extname(path).toLowerCase());
}

function git(...args) {
  return execFileSync('git', args, { cwd: root, maxBuffer: 32 * 1024 * 1024 });
}

function main() {
  const [number, ...extra] = process.argv.slice(2);
  if (!/^[1-9][0-9]*$/.test(number ?? '') || extra.length) {
    throw new Error('Usage: npm --prefix site run preview -- <PR number>');
  }

  const pr = JSON.parse(execFileSync('gh', ['pr', 'view', number, '--repo', repo,
    '--json', 'headRefOid,isCrossRepository,files'], { cwd: root, encoding: 'utf8' }));
  if (pr.isCrossRepository || !pr.files.some(({ path }) => path.startsWith('site/') && isAsset(path.slice(5)))) {
    throw new Error('Choose a pull request from this repository that changes public site assets.');
  }

  const head = pr.headRefOid;
  if (!/^[0-9a-f]{40}$/.test(head ?? '')) throw new Error('GitHub did not return a valid PR commit.');
  git('fetch', '--no-tags', '--no-write-fetch-head', `https://github.com/${repo}.git`, head);

  const tempRoot = realpathSync(tmpdir());
  const staging = mkdtempSync(join(tempRoot, 'fano-preview-'));
  try {
    const entries = git('ls-tree', '-r', '-z', head, '--', 'site/').toString().split('\0').filter(Boolean);
    for (const entry of entries) {
      const tab = entry.indexOf('\t');
      const [mode, , object] = entry.slice(0, tab).split(' ');
      const path = entry.slice(tab + 1 + 'site/'.length);
      if (!['100644', '100755'].includes(mode) || !isAsset(path)) continue;
      const target = join(staging, 'assets', path);
      mkdirSync(dirname(target), { recursive: true });
      writeFileSync(target, git('cat-file', 'blob', object), { flag: 'wx' });
    }

    copyFileSync(new URL('./wrangler.preview.json', import.meta.url), join(staging, 'wrangler.json'));
    const wrangler = fileURLToPath(new URL('./node_modules/wrangler/bin/wrangler.js', import.meta.url));
    const output = execFileSync(process.execPath, [wrangler, 'preview', '--name', `pr-${number}`,
      '--message', `PR #${number} at ${head}`, '--json'], { cwd: staging, encoding: 'utf8' });
    const jsonStart = output.search(/^\{/m);
    if (jsonStart < 0) throw new Error('Wrangler did not return a preview deployment.');
    const { preview, deployment } = JSON.parse(output.slice(jsonStart));
    if (!preview.urls.length || !deployment.urls.length) throw new Error('The preview Worker has no enabled URL.');
    console.log(`PR #${number} at ${head}\nPreview ${preview.urls[0]}\nDeployment ${deployment.urls[0]}`);
  } finally {
    if (dirname(staging) === tempRoot) rmSync(staging, { recursive: true, force: true });
  }
}

try {
  main();
} catch (error) {
  console.error(error.message);
  process.exitCode = 1;
}
