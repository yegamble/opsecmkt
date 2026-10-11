import { inflateSync } from 'node:zlib';
import { expect, test } from '@playwright/test';

// P4 Inventory preview surfaces: the listing editor and archived listings on the vendor desk.
for (const path of ['/listing-edit?id=encrypted-drive', '/vendor-dashboard']) {
  test(`inventory preview ${path} is HTML-only and fits the viewport`, async ({ page }) => {
    const scripts: string[] = [];
    page.on('request', request => { if (request.resourceType() === 'script') scripts.push(request.url()); });
    const response = await page.goto(path);
    expect(response?.status()).toBe(200);
    await expect(page.locator('script')).toHaveCount(0);
    expect(scripts).toEqual([]);
    expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(page.viewportSize()!.width);
    await expect(page.locator('main')).toContainText('Automatic delivery requires a payment provider');
    await expect(page.locator('main')).toContainText('Stored unencrypted on the server');
  });
}

test('listing editor is prefilled and states its limits', async ({ page }) => {
  await page.goto('/listing-edit?id=encrypted-drive');
  await expect(page.getByRole('heading', { level: 1 })).toHaveText('Edit listing.');
  await expect(page.getByLabel('Listing title')).toHaveValue('Encrypted USB Drive — 256GB');
  await expect(page.getByLabel('Fulfillment type')).toHaveValue('physical');
  await expect(page.getByLabel(/Price \(BTC, (?:test network|main network|not configured|not yet known)\)/)).toHaveValue('0.00412');
  await expect(page.locator('main')).toContainText('1 open order(s) depend on this listing');
  await expect(page.getByRole('button', { name: 'Archive listing' })).toBeVisible();
});

test('vendor desk shows archived listings with a badge and restore control', async ({ page }) => {
  await page.goto('/vendor-dashboard');
  const archived = page.locator('.product-card.is-archived');
  await expect(archived).toHaveCount(1);
  await expect(archived.locator('.badge')).toHaveText('Archived');
  await expect(archived.getByRole('button', { name: /^Restore / })).toBeVisible();
  await expect(page.locator('.stats')).toContainText('1 archived');
  await expect(page.getByLabel('Fulfillment type')).not.toContainText('Service');
});

// A-188: the listing photo form in the read-only preview. The preview has no scanner: /listing-edit shows a sample
// of the Ready state, ?scanner=off the state without CLAMD_ADDRESS and ?scanner=paused an unresponsive scanner.
// Proves markup and layout only; uploads themselves are db-listing-photos.spec.ts and the Go tests.
const editPath = '/listing-edit?id=encrypted-drive';

test('photo form is separate, multipart, csrf first', async ({ page }) => {
  await page.goto(editPath);
  const panel = page.getByRole('region', { name: 'Photos' });
  const form = panel.locator('form[action="/listings/photo"]');
  await expect(form).toHaveAttribute('method', 'post');
  await expect(form).toHaveAttribute('enctype', 'multipart/form-data');
  // The only multipart form; the listing details form stays urlencoded and takes no file.
  await expect(page.locator('form[enctype="multipart/form-data"]')).toHaveCount(1);
  const details = page.locator('form[action="/listings/update"]');
  expect(await details.getAttribute('enctype')).toBeNull();
  await expect(details.locator('input[type="file"]')).toHaveCount(0);
  await expect(form.locator('input').first()).toHaveAttribute('name', 'csrf');
  await expect(form.locator('input').first()).toHaveAttribute('type', 'hidden');
  // The server reads the parts in this order and refuses any other.
  expect(await form.locator('input').evaluateAll(inputs => inputs.map(i => i.getAttribute('name')))).toEqual(['csrf', 'id', 'description', 'photo']);
  const file = form.getByLabel('Photo', { exact: true });
  await expect(file).toHaveAttribute('type', 'file');
  await expect(file).toHaveAttribute('accept', 'image/jpeg,image/png');
  await expect(file).toHaveAttribute('required', '');
  expect(await file.getAttribute('capture')).toBeNull();
  await expect(form.getByLabel('Photo description (optional)')).toHaveAttribute('maxlength', '250');
  await expect(panel).toContainText('0 of 4 photos · 0 MB of 50 MB');
  await expect(page.locator('script')).toHaveCount(0);
});

test('limits and warning precede the input', async ({ page }) => {
  await page.goto(editPath);
  const form = page.locator('form[action="/listings/photo"]');
  const file = form.getByLabel('Photo', { exact: true });
  await expect(file).toHaveAttribute('aria-describedby', 'photo-help photo-privacy');
  const help = page.locator('#photo-help');
  await expect(help).toContainText('JPEG or PNG, at most 4 MB, 4096 pixels a side and 4096×3072 in all (about 12.6 megapixels).');
  await expect(help).toContainText('the original file is not kept');
  await expect(help).toContainText('At most 4 photos per listing.');
  await expect(help).toContainText('Uploading can take up to 90 seconds over Tor; press Upload once and wait for the page to change.');
  await expect(help).toContainText('Each file is checked by the ClamAV malware scanner; a match is refused and nothing is stored.');
  await expect(help).toContainText('ClamAV checks uploads for known malware; it does not review picture content.');
  const privacy = page.locator('#photo-privacy');
  await expect(privacy).toHaveClass(/\bnotice\b/);
  await expect(privacy).toContainText('public to anyone, including visitors who are not signed in');
  await expect(privacy).toContainText('location, camera and date (EXIF, XMP, IPTC), colour profiles, embedded previews and the file name');
  await expect(privacy).toContainText('faces, hands, tattoos, reflections, windows, the room, packaging, handwriting, screens, logos');
  await expect(privacy).toContainText('sensor noise');
  await expect(privacy).toContainText('Screenshots can show your clock and timezone, language, username, file paths, other tabs and notifications. Crop them before uploading.');
  await expect(privacy).toContainText('Never reuse a photo');
  await expect(file).toHaveAccessibleDescription(/at most 4 MB.*Screenshots can show your clock/);
  // Both come before the file input in reading and tab order.
  for (const id of ['photo-help', 'photo-privacy']) {
    expect(await file.evaluate((input, before) => !!(document.getElementById(before)!.compareDocumentPosition(input) & Node.DOCUMENT_POSITION_FOLLOWING), id)).toBe(true);
  }
});

test('photo form off when scanner not configured', async ({ page }) => {
  await page.goto(`${editPath}&scanner=off`);
  const panel = page.getByRole('region', { name: 'Photos' });
  await expect(panel).toContainText('Photo uploads are off on this market: they require the ClamAV malware scanner, which the operator has not set up.');
  await expect(page.locator('input[type="file"]')).toHaveCount(0);
  await expect(page.locator('form[enctype="multipart/form-data"]')).toHaveCount(0);
  await expect(page.getByRole('button', { name: 'Save changes' })).toBeEnabled();
  // Paused (scanner not responding): the form stays in place, disabled, with the reason.
  await page.goto(`${editPath}&scanner=paused`);
  await expect(panel).toContainText('Photo uploads are paused: the malware scanner is not responding. Listing details can still be saved.');
  await expect(panel.locator('fieldset')).toHaveAttribute('disabled', '');
  await expect(panel.getByLabel('Photo', { exact: true })).toBeDisabled();
  await expect(panel.getByRole('button', { name: 'Upload photo' })).toBeDisabled();
  await expect(page.getByRole('button', { name: 'Save changes' })).toBeEnabled();
});

test('fits 320 px at 200%', async ({ page, browserName }) => {
  await page.setViewportSize({ width: 320, height: 900 });
  for (const path of [editPath, '/account']) {
    await page.goto(path);
    const file = page.locator('input[type="file"]');
    expect((await file.boundingBox())!.height).toBeGreaterThanOrEqual(44);
    expect(parseFloat(await file.evaluate(input => getComputedStyle(input).fontSize))).toBeGreaterThanOrEqual(14);
    const button = await file.evaluate(input => { const s = getComputedStyle(input, '::file-selector-button'); return { minHeight: s.minHeight, fontSize: s.fontSize }; });
    if (button.minHeight) expect(parseFloat(button.minHeight)).toBeGreaterThanOrEqual(44);
    if (button.fontSize) expect(parseFloat(button.fontSize)).toBeGreaterThanOrEqual(14);
    // Keyboard focus shows the amber ring.
    await file.focus();
    await expect(file).toBeFocused();
    await expect(file).toHaveCSS('outline-style', 'solid');
    await expect(file).toHaveCSS('outline-width', '3px');
    await expect(file).toHaveCSS('outline-color', 'rgb(255, 181, 46)');
    for (const scale of ['100%', '200%']) {
      await page.locator('html').evaluate((element, size) => { element.style.fontSize = size; }, scale);
      expect(await page.evaluate(() => document.documentElement.scrollWidth), `${path} at ${scale} text`).toBeLessThanOrEqual(320);
      const box = (await file.boundingBox())!;
      expect(box.x + box.width, `${path} file input at ${scale} text (${browserName})`).toBeLessThanOrEqual(320);
    }
    // PR #24 review R24-2: with a file chosen, its name stays readable at 320 px and 200% text, where the privacy
    // warning asks the uploader to check the file. The name is drawn inside the control, out of the DOM's reach,
    // so it is measured in pixels: the columns that change when only the name's colour is made transparent.
    await file.setInputFiles({ name: 'holiday-kitchen-photo.png', mimeType: 'image/png', buffer: Buffer.from('not read in the preview') });
    await page.locator('html').evaluate(element => { element.style.fontSize = '200%'; });
    await page.mouse.move(0, 0);
    await file.evaluate(input => (input as HTMLInputElement).blur());
    const shown = await file.screenshot({ animations: 'disabled', caret: 'hide' });
    await file.evaluate(input => { (input as HTMLInputElement).style.color = 'transparent'; });
    const hidden = await file.screenshot({ animations: 'disabled', caret: 'hide' });
    await file.evaluate(input => { (input as HTMLInputElement).style.color = ''; });
    // At 28 px text a character is about 12–17 px wide: at least five characters of the name are drawn.
    expect(changedColumns(shown, hidden), `${path}: chosen file name width in px (${browserName})`).toBeGreaterThanOrEqual(60);
  }
});

// changedColumns decodes two same-sized 8-bit, non-interlaced PNG screenshots (Playwright's) and counts the pixel
// columns in which any pixel differs.
function changedColumns(a: Buffer, b: Buffer): number {
  const pa = decodePNG(a), pb = decodePNG(b);
  expect([pa.width, pa.height]).toEqual([pb.width, pb.height]);
  let columns = 0;
  for (let x = 0; x < pa.width; x++) {
    for (let y = 0; y < pa.height; y++) {
      const i = (y * pa.width + x) * pa.channels;
      if (pa.pixels.subarray(i, i + 3).compare(pb.pixels.subarray(i, i + 3)) !== 0) {
        columns++;
        break;
      }
    }
  }
  return columns;
}

function decodePNG(png: Buffer): { width: number; height: number; channels: number; pixels: Buffer } {
  let offset = 8, width = 0, height = 0, channels = 0;
  const data: Buffer[] = [];
  while (offset < png.length) {
    const length = png.readUInt32BE(offset), type = png.toString('latin1', offset + 4, offset + 8);
    const body = png.subarray(offset + 8, offset + 8 + length);
    if (type === 'IHDR') {
      width = body.readUInt32BE(0);
      height = body.readUInt32BE(4);
      expect([body[8], body[12]], 'PNG bit depth and interlace').toEqual([8, 0]);
      channels = ({ 0: 1, 2: 3, 4: 2, 6: 4 } as Record<number, number>)[body[9]];
    } else if (type === 'IDAT') {
      data.push(body);
    }
    offset += 12 + length;
  }
  const raw = inflateSync(Buffer.concat(data)), stride = width * channels, pixels = Buffer.alloc(stride * height);
  for (let y = 0; y < height; y++) {
    const filter = raw[y * (stride + 1)], line = raw.subarray(y * (stride + 1) + 1, (y + 1) * (stride + 1));
    for (let x = 0; x < stride; x++) {
      const left = x >= channels ? pixels[y * stride + x - channels] : 0;
      const up = y > 0 ? pixels[(y - 1) * stride + x] : 0;
      const upLeft = x >= channels && y > 0 ? pixels[(y - 1) * stride + x - channels] : 0;
      const p = left + up - upLeft, pl = Math.abs(p - left), pu = Math.abs(p - up), pul = Math.abs(p - upLeft);
      const predictor = [0, left, up, (left + up) >> 1, pl <= pu && pl <= pul ? left : pu <= pul ? up : upLeft][filter];
      pixels[y * stride + x] = (line[x] + predictor) & 0xff;
    }
  }
  return { width, height, channels, pixels };
}
