import { execFileSync } from 'node:child_process';
import { randomBytes } from 'node:crypto';
import { deflateSync } from 'node:zlib';
import { expect, test } from '@playwright/test';
import { uniqueHandle } from './db-fixtures';
import { signedIn } from './db-helpers';

// A-189 (war room 2026-09-27, UI-7/UI-8, DA-10): a browser with JavaScript off requests every <img> on a page at
// once, so a vendor page is paged at 25 listings and shows only thumbnails; the image route's D7 cache header
// lets a second view reuse them without any request.

// crc32 for PNG chunks (no image package is installed for the browser tests).
const crcTable = Array.from({ length: 256 }, (_, n) => {
  let c = n;
  for (let k = 0; k < 8; k++) c = c & 1 ? 0xedb88320 ^ (c >>> 1) : c >>> 1;
  return c >>> 0;
});
function crc32(buf: Buffer): number {
  let c = 0xffffffff;
  for (const b of buf) c = crcTable[(c ^ b) & 0xff] ^ (c >>> 8);
  return (c ^ 0xffffffff) >>> 0;
}
function chunk(type: string, data: Buffer): Buffer {
  const len = Buffer.alloc(4);
  len.writeUInt32BE(data.length);
  const body = Buffer.concat([Buffer.from(type, 'latin1'), data]);
  const crc = Buffer.alloc(4);
  crc.writeUInt32BE(crc32(body));
  return Buffer.concat([len, body, crc]);
}
// An 800×600 RGB PNG: a gradient with a dark rectangle, like a product on a table.
function samplePNG(): Buffer {
  const w = 800, h = 600;
  const raw = Buffer.alloc((w * 3 + 1) * h);
  for (let y = 0; y < h; y++) {
    raw[y * (w * 3 + 1)] = 0;
    for (let x = 0; x < w; x++) {
      const o = y * (w * 3 + 1) + 1 + x * 3;
      const inside = Math.abs(x - 400) < 180 && Math.abs(y - 300) < 110;
      raw[o] = inside ? 40 : 30 + (x * 60) / w;
      raw[o + 1] = inside ? 44 : 70 + (y * 120) / h;
      raw[o + 2] = inside ? 40 : 60;
    }
  }
  const ihdr = Buffer.alloc(13);
  ihdr.writeUInt32BE(w, 0);
  ihdr.writeUInt32BE(h, 4);
  ihdr[8] = 8; // bit depth
  ihdr[9] = 2; // RGB
  return Buffer.concat([Buffer.from('\x89PNG\r\n\x1a\n', 'latin1'), chunk('IHDR', ihdr), chunk('IDAT', deflateSync(raw)), chunk('IEND', Buffer.alloc(0))]);
}

test('vendor page image budget and cache', async ({ browser, baseURL }) => {
  test.setTimeout(120_000);
  const database = process.env.E2E_DATABASE_URL!;
  const sql = (query: string) => execFileSync('psql', [database, '-v', 'ON_ERROR_STOP=1', '-qtAc', query], { encoding: 'utf8' }).trim();
  const vendorHandle = uniqueHandle('photo_vendor');
  const vendor = await signedIn(browser, baseURL, vendorHandle, 'browser-photo-vendor-123', true);
  const visitorContext = await browser.newContext({ baseURL, javaScriptEnabled: false, viewport: { width: 390, height: 844 } });
  const visitor = await visitorContext.newPage();
  try {
    // Thirty listings seeded in SQL (the listing form is covered elsewhere), newest last.
    const ids = Array.from({ length: 30 }, () => randomBytes(32).toString('hex'));
    sql(`UPDATE users SET role='vendor' WHERE handle='${vendorHandle}';
      INSERT INTO products(id,vendor_id,title,description,category,region,kind,btc,xmr,stock,created)
        SELECT v.id, u.id, 'Photo listing '||lpad(v.n::text,2,'0'), 'Seeded by db-listing-photos.spec.ts', 'Hardware', 'Worldwide', 'physical', 100000, 500000000000, 1, now()+v.n*interval '1 second'
        FROM users u, unnest(ARRAY['${ids.join("','")}']) WITH ORDINALITY AS v(id,n) WHERE u.handle='${vendorHandle}';`);
    const vendorID = sql(`SELECT id FROM users WHERE handle='${vendorHandle}'`);

    // One photo through the real upload route (scanned by the suite's fake clamd, re-encoded by the app), then
    // its stored row copied onto the other 29 listings under new ids.
    await vendor.goto('/vendor-dashboard');
    const csrf = await vendor.locator('input[name="csrf"]').first().inputValue();
    const form = new FormData();
    form.append('csrf', csrf);
    form.append('id', ids[0]);
    form.append('photo', new Blob([samplePNG()], { type: 'image/png' }), 'sample.png');
    const upload = await vendor.request.post('/listings/photo', { multipart: form, maxRedirects: 0 });
    expect(upload.status(), await upload.text()).toBe(303);
    const photoID = sql(`SELECT id FROM images WHERE product_id='${ids[0]}'`);
    expect(photoID).toMatch(/^[0-9a-f]{32}$/);
    sql(`INSERT INTO images(id,owner_user_id,product_id,kind,"full",thumb,width,height)
      SELECT md5(random()::text||p.id), i.owner_user_id, p.id, 'photo', i."full", i.thumb, i.width, i.height
      FROM images i, products p WHERE i.id='${photoID}' AND p.vendor_id='${vendorID}' AND p.id<>'${ids[0]}'`);
    const thumbBytes = Number(sql(`SELECT octet_length(thumb) FROM images WHERE id='${photoID}'`));
    expect(thumbBytes).toBeLessThanOrEqual(32 << 10);

    // Every image request the visitor's browser makes, from the DevTools protocol (routing would turn the
    // browser cache off): its URL, whether it went to the network, and the bytes transferred.
    const cdp = await visitorContext.newCDPSession(visitor);
    await cdp.send('Network.enable');
    type Req = { url: string; cached: boolean; bytes: number; done: boolean };
    let requests = new Map<string, Req>();
    cdp.on('Network.requestWillBeSent', e => {
      if (new URL(e.request.url).pathname.startsWith('/img/')) requests.set(e.requestId, { url: e.request.url, cached: false, bytes: 0, done: false });
    });
    cdp.on('Network.requestServedFromCache', e => { const r = requests.get(e.requestId); if (r) r.cached = true; });
    cdp.on('Network.responseReceived', e => {
      const r = requests.get(e.requestId);
      if (r && (e.response.fromDiskCache || e.response.fromMemoryCache)) r.cached = true;
    });
    cdp.on('Network.loadingFinished', e => { const r = requests.get(e.requestId); if (r) { r.bytes = e.encodedDataLength; r.done = true; } });

    const page1 = `/vendor?id=${vendorID}`;
    await visitor.goto(page1);
    await expect(visitor.locator('.listings-grid .product-card')).toHaveCount(25);
    await expect(visitor.locator('.product-art.has-photo img')).toHaveCount(25);
    const pager = visitor.getByRole('navigation', { name: 'Vendor listing pages' });
    await expect(pager.locator('[aria-current="page"]')).toHaveText('Page 1 of 2');
    await expect(visitor.locator('.product-card').first()).toContainText('Photo listing 30');
    await expect.poll(() => [...requests.values()].filter(r => r.done).length).toBe(25);
    const first = [...requests.values()];
    expect(first.every(r => /\/img\/[0-9a-f]{32}-t$/.test(r.url)), 'only thumbnails').toBe(true);
    expect(new Set(first.map(r => r.url)).size).toBe(25);
    const total = first.reduce((sum, r) => sum + r.bytes, 0);
    expect(total, `image bytes on the first view: ${total}`).toBeLessThanOrEqual(25 * 32 * 1024);
    for (const img of await visitor.locator('.product-art.has-photo img').all()) {
      expect(await img.evaluate(element => (element as HTMLImageElement).naturalWidth)).toBe(320);
    }

    // A second view of the same page transfers no image: every one comes from the browser's cache.
    requests = new Map();
    await visitor.goto(page1);
    await expect(visitor.locator('.product-art.has-photo img')).toHaveCount(25);
    for (const img of await visitor.locator('.product-art.has-photo img').all()) {
      expect(await img.evaluate(element => (element as HTMLImageElement).complete && (element as HTMLImageElement).naturalWidth)).toBe(320);
    }
    expect(requests.size, "the second view's image loads were observed").toBe(25);
    const network = [...requests.values()].filter(r => !r.cached);
    expect(network.map(r => r.url), 'image requests on the second view').toEqual([]);

    // Page 2: the other five, and a link back.
    await pager.getByRole('link', { name: 'Next page, page 2 of 2' }).click();
    await expect(visitor.locator('.listings-grid .product-card')).toHaveCount(5);
    await expect(pager.locator('[aria-current="page"]')).toHaveText('Page 2 of 2');
    await expect(pager.getByRole('link', { name: 'Previous page, page 1 of 2' })).toHaveAttribute('href', page1);
    expect(await visitor.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(390);
  } finally {
    await vendor.context().close();
    await visitorContext.close();
  }
});
