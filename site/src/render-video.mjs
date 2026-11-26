// Renders site/src/how-scene.html to how-wide.mp4, how-tall.mp4, and first/last webp posters.
// Requires playwright-core (e.g., via NODE_PATH pointing to node_modules), Chromium, and ffmpeg.
import { execFileSync, spawn } from 'node:child_process';
import fs from 'node:fs';
import path from 'node:path';
import { pathToFileURL } from 'node:url';

const { chromium } = await import(process.env.PLAYWRIGHT_CORE_PATH ? pathToFileURL(process.env.PLAYWRIGHT_CORE_PATH).href : 'playwright-core');

const [source = 'site/src/how-scene.html', outDir = 'site', only] = process.argv.slice(2);
const FPS = 30, RUN = 12, HOLD = 2.5;
const FORMATS = {
  wide: { viewport: { width: 1600, height: 980 }, scale: 1, crf: 22 },
  tall: { viewport: { width: 392, height: 800 }, scale: 2, crf: 23 },
};
fs.mkdirSync(outDir, { recursive: true });

const br = await chromium.launch(process.env.CHROME_PATH ? { executablePath: process.env.CHROME_PATH } : undefined);
try {
  for (const [name, f] of Object.entries(FORMATS)) {
    if (only && only !== name) continue;
    const ctx = await br.newContext({ viewport: f.viewport, deviceScaleFactor: f.scale });
    const page = await ctx.newPage();
    await page.goto(pathToFileURL(path.resolve(source)).href);
    await page.evaluate(() => document.fonts.ready);
    await page.evaluate(() => window.__setup());
    const out = path.join(outDir, `how-${name}.mp4`);
    const ff = spawn('ffmpeg', [
      '-y', '-loglevel', 'error', '-f', 'image2pipe', '-c:v', 'png', '-framerate', String(FPS), '-i', '-',
      '-vf', 'scale=in_range=pc:out_range=tv:out_color_matrix=bt709,format=yuv420p',
      '-c:v', 'libx264', '-preset', 'slow', '-crf', String(f.crf), '-tune', 'animation',
      '-colorspace', 'bt709', '-color_primaries', 'bt709', '-color_trc', 'bt709', '-color_range', 'tv',
      '-movflags', '+faststart', '-an', out,
    ], { stdio: ['pipe', 'inherit', 'inherit'] });
    const done = new Promise((ok, no) => ff.on('close', c => c ? no(new Error(`ffmpeg ${c}`)) : ok()));
    const frames = Math.round((RUN + HOLD) * FPS);
    let png;
    const firstPng = path.join(outDir, `how-${name}-first.png`);
    const lastPng = path.join(outDir, `how-${name}-last.png`);
    for (let i = 0; i < frames; i++) {
      const p = i / FPS / RUN;
      if (p <= 1 || !png) {
        await page.evaluate(v => window.__paint(window.__tAt(v)), p);
        png = await page.screenshot({ type: 'png' });
        if (i === 0) fs.writeFileSync(firstPng, png);
      }
      if (!ff.stdin.write(png)) await new Promise(r => ff.stdin.once('drain', r));
    }
    fs.writeFileSync(lastPng, png);
    ff.stdin.end();
    await done;
    for (const [srcPng, dstWebp] of [[firstPng, path.join(outDir, `how-${name}-first.webp`)], [lastPng, path.join(outDir, `how-${name}-last.webp`)]]) {
      execFileSync('ffmpeg', ['-y', '-loglevel', 'error', '-i', srcPng, '-c:v', 'libwebp', '-quality', '90', dstWebp]);
      fs.rmSync(srcPng, { force: true });
    }
    console.log(out, (fs.statSync(out).size / 1e3).toFixed(0), 'KB');
    await ctx.close();
  }
} finally {
  await br.close();
}
