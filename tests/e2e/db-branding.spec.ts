import { deflateSync } from 'node:zlib';
import { expect, test, type Page } from '@playwright/test';
import { ADMIN, FIXTURE_LISTING } from './db-fixtures';
import { expectNotice, signedIn, submitStatus, withinViewport } from './db-helpers';
import { waitForStyles } from './styles';

// Real administrator forms and image uploads, with JavaScript disabled. The
// disposable database project serializes changes to global market settings.
const SVG = Buffer.from('<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 240 60"><path fill="#f59e0b" d="M0 0h240v60H0z"/></svg>');
const PHOTO_METADATA = 'BRANDING-TEST-PRIVATE-PHOTO-METADATA';

// Generate a very narrow portrait rather than committing a binary fixture. Its
// metadata must disappear when the scanner-approved PNG is re-encoded as JPEG.
function tallPhoto(): Buffer {
  function chunk(type: string, body: Buffer): Buffer {
    const length = Buffer.alloc(4);
    length.writeUInt32BE(body.length);
    const content = Buffer.concat([Buffer.from(type), body]);
    let crc = 0xffffffff;
    for (const byte of content) {
      crc ^= byte;
      for (let bit = 0; bit < 8; bit++) crc = crc & 1 ? 0xedb88320 ^ (crc >>> 1) : crc >>> 1;
    }
    const checksum = Buffer.alloc(4);
    checksum.writeUInt32BE((crc ^ 0xffffffff) >>> 0);
    return Buffer.concat([length, content, checksum]);
  }
  const header = Buffer.alloc(13);
  header.writeUInt32BE(8, 0);
  header.writeUInt32BE(1200, 4);
  header.set([8, 2, 0, 0, 0], 8);
  const rows = Buffer.alloc((1 + 8 * 3) * 1200, 160);
  for (let row = 0; row < 1200; row++) rows[row * 25] = 0;
  return Buffer.concat([
    Buffer.from([137, 80, 78, 71, 13, 10, 26, 10]), chunk('IHDR', header),
    chunk('tEXt', Buffer.from(`Comment\0${PHOTO_METADATA}`)),
    chunk('IDAT', deflateSync(rows)), chunk('IEND', Buffer.alloc(0)),
  ]);
}

async function fixedBranding(page: Page, name: string, logoURL: string | null) {
  for (const width of [320, 1280]) {
    await page.setViewportSize({ width, height: 900 });
    await waitForStyles(page);
    const home = page.getByRole('link', { name: `${name} home`, exact: true });
    const footer = page.locator('.footer-wordmark');
    for (const [element, expectedWidth, expectedHeight] of [
      [home, width === 320 ? 192 : 224, width === 320 ? 48 : 56],
      [footer, 192, 48],
    ] as const) {
      await expect(element).toBeVisible();
      const box = (await element.boundingBox())!;
      expect(box.width).toBeCloseTo(expectedWidth, 2);
      expect(box.height).toBeCloseTo(expectedHeight, 2);
      expect(box.x).toBeGreaterThanOrEqual(0);
      expect(box.x + box.width).toBeLessThanOrEqual(width);
      if (logoURL) {
        const image = element.getByRole('img', { name, exact: true });
        await expect(image).toHaveAttribute('src', logoURL);
        await expect(image).toHaveCSS('object-fit', 'contain');
        expect(await image.evaluate((node: HTMLImageElement) => node.complete && node.naturalWidth > 0)).toBe(true);
        const imageBox = (await image.boundingBox())!;
        expect(imageBox.width).toBeCloseTo(expectedWidth, 2);
        expect(imageBox.height).toBeCloseTo(expectedHeight, 2);
      } else {
        await expect(element.locator('.market-logo')).toHaveCount(0);
        await expect(element).toContainText(name);
      }
    }
    expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(width);
  }
}

test('administrator branding reaches the whole site and awkward images keep fixed layout boxes', async ({ page: guest, browser, baseURL }) => {
  test.setTimeout(120_000);
  const admin = await signedIn(browser, baseURL, ADMIN.handle, ADMIN.password, false, 320);
  await admin.goto('/admin/site');
  const originalName = await admin.getByLabel('Market name', { exact: true }).inputValue();
  const longName = 'Very Long Market Name '.repeat(4).slice(0, 80);

  async function saveName(name: string) {
    await admin.goto('/admin/site#branding');
    const panel = admin.locator('#branding');
    await panel.getByLabel('Market name', { exact: true }).fill(name);
    expect(await submitStatus(admin, () => panel.getByRole('button', { name: 'Save market name' }).click())).toBe(303);
    await expectNotice(admin, 'Marketplace settings saved.');
    await expect(panel.getByLabel('Market name', { exact: true })).toHaveValue(name);
  }
  async function saveAccess(mode: 'public' | 'members', registrations: 'open' | 'closed') {
    await admin.goto('/admin/security');
    const panel = admin.locator('#access');
    await panel.locator(`input[name="site_access"][value="${mode}"]`).check();
    await panel.locator(`input[name="registrations"][value="${registrations}"]`).check();
    await panel.getByLabel('Current password', { exact: true }).fill(ADMIN.password);
    expect(await submitStatus(admin, () => panel.getByRole('button', { name: 'Save access settings' }).click())).toBe(303);
    await expectNotice(admin, 'Site access and registration settings saved.');
  }
  async function upload(name: string, mimeType: string, buffer: Buffer): Promise<string> {
    await admin.goto('/admin/site#branding');
    const authorization = admin.locator('form[action="/admin/branding/authorize"]');
    await authorization.getByLabel('Current password', { exact: true }).fill(ADMIN.password);
    expect(await submitStatus(admin, () => authorization.getByRole('button', { name: 'Authorize logo upload' }).click())).toBe(303);
    await expect(admin).toHaveURL(/\/admin\/branding\/upload$/);
    await expect(admin.getByRole('heading', { name: 'Upload market logo' })).toBeVisible();
    // The public upload capability contains no reusable administrator credential.
    await expect(admin.locator('form input[name]')).toHaveCount(2);
    await expect(admin.locator('form input[type="password"], form input[name="csrf"], form input[name="form_nonce"]')).toHaveCount(0);
    await admin.getByLabel('Logo file (maximum 4 MiB; SVG maximum 256 KiB)', { exact: true }).setInputFiles({ name, mimeType, buffer });
    expect(await submitStatus(admin, () => admin.getByRole('button', { name: 'Upload logo', exact: true }).click())).toBe(200);
    await expect(admin.getByRole('heading', { name: 'Logo saved', exact: true })).toBeVisible();
    await admin.goto('/admin/site#branding');
    await withinViewport(admin.locator('#branding'), 320);
    const url = (await admin.locator('.brand .market-logo').getAttribute('src'))!;
    expect(url).toMatch(/^\/branding\/logo\/[0-9a-f]{32}$/);
    return url;
  }
  async function reset() {
    await admin.goto('/admin/site#branding');
    const resetForm = admin.locator('form[action="/admin/branding/reset"]');
    if (await resetForm.count() === 0) return;
    await resetForm.getByLabel('Current password', { exact: true }).fill(ADMIN.password);
    expect(await submitStatus(admin, () => resetForm.getByRole('button', { name: 'Use text wordmark' }).click())).toBe(303);
    await expect(admin).toHaveURL(/\/admin\/site#branding$/);
    await expect(admin.locator('.brand .market-logo')).toHaveCount(0);
  }

  try {
    await saveName(longName);
    await guest.goto('/');
    await expect(guest).toHaveTitle(new RegExp(longName));
    await fixedBranding(guest, longName, null);
    // Enlarged text must truncate inside the same fixed box, rather than push
    // sign-in/search controls offscreen. This changes text size, not page script.
    // Extend the same-origin test stylesheet so the production CSP stays active.
    await guest.route('**/static/style.css', async route => {
      const stylesheet = await route.fetch();
      await route.fulfill({ response: stylesheet, body: `${await stylesheet.text()}\nhtml { font-size: 200% !important; }` });
    });
    await guest.reload();
    await fixedBranding(guest, longName, null);
    await guest.unroute('**/static/style.css');

    const svgURL = await upload('wide-logo.svg', 'image/svg+xml', SVG);
    const svg = await guest.request.get(svgURL);
    expect(svg.status()).toBe(200);
    expect(svg.headers()['content-type']).toBe('image/svg+xml');
    expect(svg.headers()['cache-control']).toBe('no-store');
    for (const path of ['/', '/login', '/canary']) {
      expect((await guest.goto(path))?.status()).toBe(200);
      await fixedBranding(guest, longName, svgURL);
      await expect(guest.locator('script')).toHaveCount(0);
    }

    const photoURL = await upload('awkward-portrait.png', 'image/png', tallPhoto());
    expect(photoURL).not.toBe(svgURL);
    expect((await guest.request.get(svgURL)).status()).toBe(404);
    const photo = await guest.request.get(photoURL);
    expect(photo.status()).toBe(200);
    expect(photo.headers()['content-type']).toBe('image/jpeg');
    expect((await photo.body()).includes(Buffer.from(PHOTO_METADATA))).toBe(false);
    await guest.goto('/');
    await fixedBranding(guest, longName, photoURL);

    await saveAccess('members', 'closed');
    await guest.goto(`/?q=${encodeURIComponent(FIXTURE_LISTING)}`);
    await expect(guest).toHaveURL(/\/login$/);
    await expect(guest.locator('body')).not.toContainText(FIXTURE_LISTING);
    await expect(guest.locator('.header-search, .sidebar, nav[aria-label="Main navigation"]')).toHaveCount(0);
    await expect(guest.getByRole('link', { name: 'Join market', exact: true })).toHaveCount(0);
    await fixedBranding(guest, longName, photoURL);
    // Branding is intentionally visible on sign-in screens while catalog data is
    // gated. The asset itself remains fetchable without a member session.
    expect((await guest.request.get(photoURL)).status()).toBe(200);
    await admin.goto('/');
    await fixedBranding(admin, longName, photoURL);

    await reset();
    expect((await guest.request.get(photoURL)).status()).toBe(404);
    await guest.goto('/login');
    await fixedBranding(guest, longName, null);
  } finally {
    try {
      await reset();
      await saveName(originalName);
      await saveAccess('public', 'open');
    } finally {
      await admin.context().close();
    }
  }
});
