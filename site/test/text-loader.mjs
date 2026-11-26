// Loads .md files imported by src/worker.js as plain UTF-8 strings under Node's test runner,
// matching Wrangler's Text module rule.
import fs from 'node:fs';
import { fileURLToPath } from 'node:url';

export async function load(url, context, nextLoad) {
  if (url.endsWith('.md')) {
    return {
      format: 'module',
      shortCircuit: true,
      source: `export default ${JSON.stringify(fs.readFileSync(fileURLToPath(url), 'utf8'))};`,
    };
  }
  return nextLoad(url, context);
}
