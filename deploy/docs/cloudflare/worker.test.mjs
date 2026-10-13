// node --test deploy/docs/cloudflare/worker.test.mjs (CI's docs job): the Worker's headers, without Cloudflare.
import assert from 'node:assert/strict';
import test from 'node:test';
import worker from './worker.js';

const ONION = 'b4lc2q256duou22r5rhdsmhqeplsvuhgpff3ws4cuvcifr6xan6rsfid.onion';
const assets = { fetch: async () => new Response('<h1>docs</h1>', { status: 200, headers: { 'Content-Type': 'text/html' } }) };
const get = (env, path = '/install/tor/?x=1') => worker.fetch(new Request('https://docs.example' + path), { ASSETS: assets, ...env });

test('points Tor Browser at the same page on the docs onion', async () => {
  const response = await get({ DOCS_ONION: ONION });
  assert.equal(response.status, 200);
  assert.equal(response.headers.get('Onion-Location'), `http://${ONION}/install/tor/?x=1`);
  assert.equal(await response.text(), '<h1>docs</h1>');
});

test('sends no Onion-Location when the address is unset or malformed', async () => {
  for (const value of [undefined, '', 'market.example.onion', `${ONION}/evil`, `http://${ONION}`, ONION.toUpperCase()]) {
    assert.equal((await get({ DOCS_ONION: value })).headers.get('Onion-Location'), null, String(value));
  }
});

test('adds the security headers to every response', async () => {
  const headers = (await get({})).headers;
  assert.match(headers.get('Content-Security-Policy'), /default-src 'self'/);
  assert.equal(headers.get('Referrer-Policy'), 'no-referrer');
  assert.equal(headers.get('X-Frame-Options'), 'DENY');
  assert.equal(headers.get('X-Content-Type-Options'), 'nosniff');
});
