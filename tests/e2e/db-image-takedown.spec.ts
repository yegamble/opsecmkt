import { execFileSync } from 'node:child_process';
import { randomBytes } from 'node:crypto';
import { deflateSync } from 'node:zlib';
import { expect, test } from '@playwright/test';
import { uniqueHandle } from './db-fixtures';
import { signedIn, submitAndLoad } from './db-helpers';

// A-190 (war room 2026-09-27, ruling D4): a moderator takes a vendor's photo down from the moderation desk's Recent
// photos with a reason, JavaScript off. The row is deleted: the image URL answers like an unknown id, and the vendor
// reads the reason on the listing's edit page (after a reload too) and in a notification. The photo goes in through
// the real upload route, scanned by the suite's fake clamd (not a malware scanner) and re-encoded by the app.

const crcTable = Array.from({ length: 256 }, (_, n) => {
  let c = n;
  for (let k = 0; k < 8; k++) c = c & 1 ? 0xedb88320 ^ (c >>> 1) : c >>> 1;
  return c >>> 0;
});
function crc32(data: Buffer): number {
  let c = 0xffffffff;
  for (const b of data) c = crcTable[(c ^ b) & 0xff] ^ (c >>> 8);
  return (c ^ 0xffffffff) >>> 0;
}
function chunk(type: string, body: Buffer): Buffer {
  const head = Buffer.alloc(4);
  head.writeUInt32BE(body.length);
  const typed = Buffer.concat([Buffer.from(type, 'latin1'), body]);
  const crc = Buffer.alloc(4);
  crc.writeUInt32BE(crc32(typed));
  return Buffer.concat([head, typed, crc]);
}
// A 120×90 RGB gradient PNG.
function png(): Buffer {
  const w = 120, h = 90;
  const rows: number[] = [];
  for (let y = 0; y < h; y++) {
    rows.push(0);
    for (let x = 0; x < w; x++) rows.push((x * 2) & 0xff, (y * 3) & 0xff, 90);
  }
  const ihdr = Buffer.alloc(13);
  ihdr.writeUInt32BE(w, 0);
  ihdr.writeUInt32BE(h, 4);
  ihdr.set([8, 2, 0, 0, 0], 8);
  return Buffer.concat([Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]), chunk('IHDR', ihdr), chunk('IDAT', deflateSync(Buffer.from(rows))), chunk('IEND', Buffer.alloc(0))]);
}

test('moderator removes a photo from the desk; URL 404s; vendor sees the reason', async ({ browser, baseURL }) => {
  test.setTimeout(90_000);
  const database = process.env.E2E_DATABASE_URL!;
  const sql = (query: string) => execFileSync('psql', [database, '-v', 'ON_ERROR_STOP=1', '-qtAc', query], { encoding: 'utf8' }).trim();
  const vendorHandle = uniqueHandle('td_vendor');
  const moderatorHandle = uniqueHandle('td_moderator');
  const vendor = await signedIn(browser, baseURL, vendorHandle, 'browser-takedown-vendor-123', true);
  const moderator = await signedIn(browser, baseURL, moderatorHandle, 'browser-takedown-moderator-123', true);
  try {
    // The roles and the listing in SQL (their forms are covered elsewhere); the photo through the upload route.
    const listing = randomBytes(32).toString('hex');
    const title = `Takedown ${uniqueHandle('listing')}`;
    sql(`UPDATE users SET role='vendor' WHERE handle='${vendorHandle}'; UPDATE users SET role='moderator' WHERE handle='${moderatorHandle}';
      INSERT INTO products(id,vendor_id,title,description,category,region,kind,btc,xmr,stock)
        SELECT '${listing}', id, '${title}', 'Seeded by db-image-takedown.spec.ts', 'Hardware', 'Worldwide', 'physical', 100000, 500000000000, 1 FROM users WHERE handle='${vendorHandle}';`);
    await vendor.goto(`/listing-edit?id=${listing}`);
    const form = new FormData();
    form.append('csrf', await vendor.locator('form[action="/listings/photo"] input[name="csrf"]').inputValue());
    form.append('id', listing);
    form.append('photo', new Blob([png()], { type: 'image/png' }), 'item.png');
    const upload = await vendor.request.post('/listings/photo', { multipart: form, maxRedirects: 0 });
    expect(upload.status(), await upload.text()).toBe(303);
    const photo = sql(`SELECT id FROM images WHERE product_id='${listing}'`);
    expect(photo).toMatch(/^[0-9a-f]{32}$/);

    // The moderator finds it in Recent photos: its public thumbnail, listing and vendor links, and a remove form.
    await moderator.goto('/moderator');
    const recent = moderator.getByRole('region', { name: 'Recent photos' });
    const item = recent.getByRole('listitem').filter({ has: moderator.getByRole('link', { name: title }) });
    await expect(item).toHaveCount(1);
    await expect(item.locator('img')).toHaveAttribute('src', `/img/${photo}-t`);
    await expect(item.getByRole('link', { name: vendorHandle })).toHaveAttribute('href', /^\/vendor\?id=/);
    await expect(item).toContainText(/Added \d{4}-\d{2}-\d{2} \d{2}:\d{2} UTC/);
    const thumb = await moderator.request.get(`/img/${photo}-t`);
    expect(thumb.status()).toBe(200);

    const reason = 'The photo shows a street sign; upload one of the item alone.';
    await item.locator('summary').click();
    await item.getByLabel('Reason for the owner').fill(reason);
    await submitAndLoad(moderator, item.getByRole('button', { name: 'Remove photo' }));
    await expect(moderator).toHaveURL(/\/moderator\?image=removed#recent-photos$/);
    await expect(moderator.getByRole('status')).toHaveText('Image removed. Its owner was notified with your reason.');
    await expect(recent.getByRole('link', { name: title })).toHaveCount(0);
    expect(sql(`SELECT count(*) FROM images WHERE id='${photo}'`)).toBe('0');

    // Both sizes on both routes answer exactly as an unknown id does.
    const unknown = await moderator.request.get(`/img/${randomBytes(16).toString('hex')}-t`);
    expect(unknown.status()).toBe(404);
    const unknownBody = await unknown.text();
    for (const path of [`/img/${photo}-t`, `/img/${photo}-f`, `/desk/img/${photo}-t`, `/desk/img/${photo}-f`]) {
      const r = await vendor.request.get(path);
      expect(r.status(), path).toBe(404);
      expect(await r.text(), path).toBe(unknownBody);
    }

    // The vendor reads the reason on the listing's edit page, on a fresh load and after a reload, and is notified.
    await vendor.goto(`/listing-edit?id=${listing}`);
    const panel = vendor.getByRole('region', { name: 'Photos' });
    await expect(panel).toContainText(`Photo removed by staff: ${reason}`);
    await expect(panel).toContainText('0 of 4 photos');
    await vendor.reload();
    await expect(vendor.getByRole('region', { name: 'Photos' })).toContainText(`Photo removed by staff: ${reason}`);
    await vendor.goto('/notifications');
    await expect(vendor.locator('main')).toContainText(`with the reason “${reason}”`);
    // The public product page never shows the reason.
    const visitor = await browser.newContext({ baseURL, javaScriptEnabled: false });
    const page = await visitor.newPage();
    await page.goto(`/product?id=${listing}`);
    await expect(page.locator('main')).not.toContainText('street sign');
    await visitor.close();
  } finally {
    await vendor.context().close();
    await moderator.context().close();
  }
});
