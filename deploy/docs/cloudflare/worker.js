// Serves the documentation site (built into site/ by mkdocs) from Cloudflare's edge and points Tor Browser at the
// project's own docs onion. DOCS_ONION is the bare v3 address (56 base32 characters + ".onion") that
// scripts/docs-onion.sh printed on the docs host; when it is unset or malformed no Onion-Location is sent.
// Never set it to a market's address: the docs are public and must not be linkable to a market.
const ONION = /^[a-z2-7]{56}\.onion$/;

const SECURITY_HEADERS = {
  'Content-Security-Policy':
    "default-src 'self'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; " +
    "object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'none'",
  'Referrer-Policy': 'no-referrer',
  'X-Content-Type-Options': 'nosniff',
  'X-Frame-Options': 'DENY',
};

export default {
  async fetch(request, env) {
    const asset = await env.ASSETS.fetch(request);
    const response = new Response(asset.body, asset);
    for (const [name, value] of Object.entries(SECURITY_HEADERS)) response.headers.set(name, value);
    const onion = (env.DOCS_ONION || '').trim();
    if (ONION.test(onion)) {
      const url = new URL(request.url);
      // Tor Browser offers this only on HTTPS pages; the same path on the onion.
      response.headers.set('Onion-Location', `http://${onion}${url.pathname}${url.search}`);
    }
    return response;
  },
};
