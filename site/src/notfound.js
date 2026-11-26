// What a path no page has answers. A browser keeps the static assets' own 404 (an empty body, so it
// shows its usual error page); an agent that asks for markdown gets a short page saying where to go,
// since an empty 404 leaves it guessing.

import { prefersMarkdown } from './accept.js';

const SITE = 'https://agent-box.afig.dev';

export const NOT_FOUND_MD = `# Not found

There is no page at this address on agent-box.afig.dev, agent-box's website.

agent-box runs slow, noisy builds and tests on your own Linux server or gaming PC, inside isolated
Incus containers, and lets coding agents push dirty worktrees and pull artifacts back. The pages
for coding agents:

- [Set up agent-box](${SITE}/setup): for an agent whose user asked to set up or install agent-box
- [Join agent-box](${SITE}/join): for an agent setting up the \`box\` client on a laptop
- [Use agent-box](${SITE}/use): how \`box_run\`, \`box_job\`, \`box_pull\`, and \`box_cancel\` work

An index for agents is at ${SITE}/llms.txt, and every page is listed in ${SITE}/sitemap.xml.
`;

// assetResponse is what the static assets answered; it is returned as it is unless the request
// prefers markdown.
export function notFound(request, assetResponse) {
  if (!prefersMarkdown(request.headers.get('Accept'))) {
    const headers = new Headers(assetResponse.headers);
    headers.append('Vary', 'Accept');
    return new Response(assetResponse.body, { status: assetResponse.status, headers });
  }
  return new Response(request.method === 'HEAD' ? null : NOT_FOUND_MD, {
    status: 404,
    headers: {
      'Content-Type': 'text/markdown; charset=utf-8',
      'Cache-Control': 'public, max-age=300',
      'Vary': 'Accept',
      'X-Robots-Tag': 'noindex',
    },
  });
}
