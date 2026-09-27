import { deflateSync } from 'node:zlib';
import { expect, test, type Browser, type Page } from '@playwright/test';
import { ADMIN, uniqueHandle } from './db-fixtures';
import { setRole, signedIn, submitStatus } from './db-helpers';

// A-188: listing photos and avatars through the real forms with JavaScript disabled, against the fake clamd the
// Playwright config starts (tests/e2e/fixtures/fake-clamd.py; not a malware scanner: it reports a stream FOUND
// only when it carries the test marker). Fixture pictures are generated here, never committed.
// Tests tagged @firefox also run in the database-firefox project (playwright.config.ts, A-192): the upload and
// refusal cases in Firefox, which shows a connection reset instead of a page when an answer races the upload.

const MARKER = 'OPSECMKT-CLAMD-TEST-MARKER';
const PLANTED = 'E2E-FIXTURE-TEXT-CHUNK';

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

// An RGB PNG of width×height with a gradient and a tEXt chunk carrying text (which re-encoding must drop).
function png(width: number, height: number, text: string): Buffer {
  const ihdr = Buffer.alloc(13);
  ihdr.writeUInt32BE(width, 0);
  ihdr.writeUInt32BE(height, 4);
  ihdr.set([8, 2, 0, 0, 0], 8);
  const rows: number[] = [];
  for (let y = 0; y < height; y++) {
    rows.push(0);
    for (let x = 0; x < width; x++) rows.push((x * 4) & 0xff, (y * 5) & 0xff, 160);
  }
  return Buffer.concat([
    Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]),
    chunk('IHDR', ihdr),
    chunk('tEXt', Buffer.from(`Comment\0${text}`, 'latin1')),
    chunk('IDAT', deflateSync(Buffer.from(rows))),
    chunk('IEND', Buffer.alloc(0)),
  ]);
}

// A fresh vendor, promoted by the shared administrator.
async function newVendor(browser: Browser, baseURL: string | undefined, role = 'vendor'): Promise<{ page: Page; handle: string }> {
  const handle = uniqueHandle('photos');
  const page = await signedIn(browser, baseURL, handle, 'browser-photos-password-123', true);
  const admin = await signedIn(browser, baseURL, ADMIN.handle, ADMIN.password, false);
  await setRole(admin, handle, role);
  await admin.context().close();
  return { page, handle };
}

// Publishes a listing from the vendor desk; publishing lands on the new listing's Photos panel.
async function publish(vendor: Page, title: string): Promise<string> {
  await vendor.goto('/vendor-dashboard');
  await vendor.getByText('+ Publish a new listing', { exact: true }).click();
  await vendor.getByLabel('Listing title').fill(title);
  await vendor.getByLabel('Description', { exact: true }).fill('Listing photo regression.');
  await vendor.getByLabel('Bitcoin reference price').fill('0.001');
  await vendor.getByLabel('Monero reference price').fill('0.5');
  await vendor.getByRole('button', { name: 'Publish listing' }).click();
  await expect(vendor).toHaveURL(/\/listing-edit\?id=[0-9a-f]{64}&saved=1#photos$/);
  return new URL(vendor.url()).searchParams.get('id')!;
}

async function uploadPhoto(vendor: Page, picture: Buffer, description = ''): Promise<number> {
  const form = vendor.locator('form[action="/listings/photo"]');
  if (description) await form.getByLabel('Photo description (optional)').fill(description);
  await form.getByLabel('Photo', { exact: true }).setInputFiles({ name: 'IMG_0001 home.png', mimeType: 'image/png', buffer: picture });
  return submitStatus(vendor, () => form.getByRole('button', { name: 'Upload photo' }).click());
}

test('publish lands on photos', async ({ browser, baseURL }) => {
  const { page: vendor } = await newVendor(browser, baseURL);
  await publish(vendor, `Photos ${uniqueHandle('land')}`);
  await expect(vendor.getByRole('status')).toHaveText('Changes saved.');
  const panel = vendor.getByRole('region', { name: 'Photos' });
  await expect(panel).toContainText('0 of 4 photos · 0 MB of 50 MB');
  await expect(panel.getByLabel('Photo', { exact: true })).toBeEnabled();
  await vendor.context().close();
});

test('uploaded photo persists after reload, re-encoded', { tag: '@firefox' }, async ({ browser, baseURL }) => {
  const { page: vendor } = await newVendor(browser, baseURL);
  const listing = await publish(vendor, `Photos ${uniqueHandle('keep')}`);
  expect(await uploadPhoto(vendor, png(96, 72, PLANTED), 'A grey box on a wooden table')).toBe(303);
  await expect(vendor).toHaveURL(new RegExp(`/listing-edit\\?id=${listing}&photo=saved#photos$`));
  await expect(vendor.getByRole('status')).toHaveText('Photo saved as a re-encoded JPEG; location, camera and other metadata were removed. Check the picture itself for anything that identifies you.');
  await vendor.goto(`/listing-edit?id=${listing}`); // a fresh load: the list comes from the database
  const panel = vendor.getByRole('region', { name: 'Photos' });
  await expect(panel).toContainText(/1 of 4 photos · 0\.1 MB of 50 MB/);
  const item = panel.locator('.photo-item');
  await expect(item).toHaveCount(1);
  await expect(item).toContainText(/Photo 1: JPEG, 96×72, \d+ KB/);
  await expect(item).toContainText('A grey box on a wooden table');
  await expect(panel).not.toContainText('IMG_0001');
  const thumb = item.locator('img');
  const src = (await thumb.getAttribute('src'))!;
  expect(src).toMatch(/^\/desk\/img\/[0-9a-f]{32}-t$/);
  expect(await thumb.evaluate(img => (img as HTMLImageElement).naturalWidth)).toBe(320);
  const full = await vendor.request.get(src.replace(/-t$/, '-f'));
  expect(full.status()).toBe(200);
  expect(full.headers()['content-type']).toBe('image/jpeg');
  const bytes = await full.body();
  expect([...bytes.subarray(0, 3)]).toEqual([0xff, 0xd8, 0xff]);
  expect(bytes.includes(Buffer.from(PLANTED))).toBe(false);
  expect(bytes.includes(Buffer.from('tEXt'))).toBe(false);
  await vendor.context().close();
});

test('vendor removes own photo', async ({ browser, baseURL }) => {
  const { page: vendor } = await newVendor(browser, baseURL);
  const listing = await publish(vendor, `Photos ${uniqueHandle('remove')}`);
  expect(await uploadPhoto(vendor, png(64, 48, PLANTED))).toBe(303);
  const panel = vendor.getByRole('region', { name: 'Photos' });
  await expect(panel).toContainText('No description');
  const src = (await panel.locator('.photo-item img').getAttribute('src'))!;
  await panel.getByRole('button', { name: 'Remove photo 1' }).click();
  await expect(vendor).toHaveURL(new RegExp(`/listing-edit\\?id=${listing}&photo=removed#photos$`));
  await expect(vendor.getByRole('status')).toHaveText('Photo removed.');
  await vendor.reload();
  await expect(panel).toContainText('0 of 4 photos');
  await expect(panel.locator('.photo-item')).toHaveCount(0);
  expect((await vendor.request.get(src)).status()).toBe(404);
  await vendor.context().close();
});

test('scanner found stores nothing', { tag: '@firefox' }, async ({ browser, baseURL }) => {
  const { page: vendor } = await newVendor(browser, baseURL);
  const listing = await publish(vendor, `Photos ${uniqueHandle('found')}`);
  expect(await uploadPhoto(vendor, png(64, 48, MARKER))).toBe(422);
  await expect(vendor.getByRole('alert')).toContainText('Photo refused: the malware scanner flagged this file. Nothing was stored.');
  await expect(vendor.locator('main')).not.toContainText('Opsecmkt.Test.Marker');
  await vendor.getByRole('link', { name: 'Back to the listing' }).click();
  await expect(vendor).toHaveURL(new RegExp(`/listing-edit\\?id=${listing}#photos$`));
  await expect(vendor.getByRole('region', { name: 'Photos' })).toContainText('0 of 4 photos');
  await vendor.goto('/account');
  await expect(vendor.getByRole('region', { name: 'Recent activity' })).toContainText('Upload refused by the malware scanner');
  await vendor.context().close();
});

// Ruling C3: a file just under the Content-Length ceiling (cap + 4 MiB), so nearly 4 MiB of it is still unsent when
// the server refuses at the 4 MB cap. The server reads that rest into nothing before it answers, so the browser shows
// the page naming the limit, not a connection reset. A file only just over the cap would not test this: Go's server
// discards up to 256 KiB of unread body by itself, and Firefox showed the page with no drain at all (PR #26 review
// R-1: without the drain, a ~4 MiB remainder reset the connection 4 times in 4).
test('oversized photo shows the limit without JS', { tag: '@firefox' }, async ({ browser, baseURL }) => {
  const { page: vendor } = await newVendor(browser, baseURL);
  const listing = await publish(vendor, `Photos ${uniqueHandle('big')}`);
  const big = Buffer.alloc(8 * 1024 * 1024 - 2048); // with the form's other parts, still under the ceiling
  png(64, 48, PLANTED).copy(big);
  expect(await uploadPhoto(vendor, big)).toBe(413);
  await expect(vendor.getByRole('alert')).toHaveText('Photo not saved: the file is larger than 4 MB, the most accepted. Nothing was stored.');
  // A refusal while the form is read links to the vendor desk, not the listing (uploads.go sets the listing link
  // only once the whole form has been read).
  await expect(vendor.getByRole('link', { name: 'Back to the vendor desk' })).toHaveAttribute('href', '/vendor-dashboard');
  await vendor.goto(`/listing-edit?id=${listing}`);
  await expect(vendor.getByRole('region', { name: 'Photos' })).toContainText('0 of 4 photos');
  await vendor.context().close();
});

test('staff avatar and hide choice on /account', async ({ browser, baseURL }) => {
  const { page: moderator } = await newVendor(browser, baseURL, 'moderator');
  await moderator.goto('/account');
  const panel = moderator.getByRole('region', { name: 'Avatar' });
  await expect(panel).toContainText('a staff photo links your identity across every dispute decision');
  await panel.getByLabel('Avatar', { exact: true }).setInputFiles({ name: 'me.png', mimeType: 'image/png', buffer: png(300, 200, PLANTED) });
  expect(await submitStatus(moderator, () => panel.getByRole('button', { name: 'Upload avatar' }).click())).toBe(303);
  await expect(moderator).toHaveURL(/\/account\?avatar=saved#avatar$/);
  await moderator.reload();
  await expect(panel).toContainText(/Your avatar: JPEG, 200×200, \d+ KB/);
  expect(await panel.locator('img').evaluate(img => (img as HTMLImageElement).naturalWidth)).toBe(200);
  const staff = moderator.getByRole('region', { name: 'Public staff list' });
  const hide = staff.getByLabel('Hide me from the public staff list');
  await expect(hide).toBeChecked();
  await hide.uncheck();
  await staff.getByRole('button', { name: 'Save choice' }).click();
  await expect(moderator).toHaveURL(/\/account\?saved=1#staff-list$/);
  await expect(hide).not.toBeChecked();
  await expect(moderator.getByRole('region', { name: 'Recent activity' })).toContainText('Public staff list choice changed: hidden → shown');
  await panel.getByRole('button', { name: 'Remove avatar' }).click();
  await expect(moderator).toHaveURL(/\/account\?avatar=removed#avatar$/);
  await expect(panel).toContainText('No avatar.');
  await moderator.context().close();
});
