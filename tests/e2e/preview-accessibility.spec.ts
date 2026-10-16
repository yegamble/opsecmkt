import { expect, test, type Page, type Locator } from '@playwright/test';
import { previewRoutes } from './preview-routes';
import { waitForStyles } from './styles';

async function filters(page: Page) {
  const mobile = page.locator('.filter-details');
  await waitForStyles(page);
  if (await mobile.isVisible()) {
    if (!(await mobile.evaluate(element => (element as HTMLDetailsElement).open))) await mobile.locator('summary').click();
    return mobile;
  }
  return page.locator('.desktop-filters');
}

async function fits(page: Page) {
  await waitForStyles(page);
  expect(await page.evaluate(() => document.documentElement.scrollWidth), `page reflow: ${page.url()}`).toBeLessThanOrEqual(page.viewportSize()!.width);
}

test('seeded categories expose one current link and matching listings after every navigation', async ({ page }) => {
  await page.goto('/');
  // A-189: the encrypted drive and security research samples have preview photos; the other cards are illustrations.
  for (const [category, count, photos] of [['All listings', 6, 2], ['Hardware', 2, 1], ['Digital', 3, 1], ['Services', 1, 0], ['All listings', 6, 2]] as const) {
    let panel = await filters(page);
    // Wait for the new document; otherwise filters() can inspect the page mid-navigation and pick the hidden panel.
    await Promise.all([page.waitForEvent('load'), panel.getByRole('link', { name: category, exact: true }).click()]);
    panel = await filters(page);
    const current = panel.locator('.category-links [aria-current="page"]');
    await expect(current).toHaveCount(1);
    await expect(current).toHaveAccessibleName(category);
    await expect(current).toHaveClass('active');
    await expect(panel.locator('.category-links .active')).toHaveCount(1);
    await expect(page.locator('.listings-grid .product-card')).toHaveCount(count);
    await expect(page.locator('.product-art.has-photo')).toHaveCount(photos);
    if (category !== 'All listings') {
      await expect(page.locator('.product-art .art-label')).toHaveText(Array(count - photos).fill(`${category} / ILLUSTRATION`));
      await expect(panel.getByRole('combobox', { name: 'Category', exact: true })).toHaveValue(category);
    }
    await fits(page);
  }
});

// A-189: a photo card shows the listing's first photo as a decorative 320×240 thumbnail (the card link is named by
// the title) with no illustration label or scanline over it, and its kind badge on a solid chip; cards without
// photos keep the illustration. The product page shows photo 1 at full size, named, and links the others' full
// size by number, type and size. The preview's sample photos are generated and served by the real image route.
test('photo cards: decorative thumbnail, no overlaid label; product photo named', async ({ page, request }) => {
  await page.goto('/');
  const photoArt = page.locator('.product-art.has-photo');
  await expect(photoArt).toHaveCount(2);
  for (const art of await photoArt.all()) {
    const img = art.locator('img');
    await expect(img).toHaveCount(1);
    await expect(img).toHaveAttribute('alt', '');
    await expect(img).toHaveAttribute('src', /^\/img\/[0-9a-f]{32}-t$/);
    await expect(img).toHaveAttribute('width', '320');
    await expect(img).toHaveAttribute('height', '240');
    expect(await img.evaluate(element => (element as HTMLImageElement).complete && (element as HTMLImageElement).naturalWidth)).toBe(320);
    expect(await img.evaluate(element => getComputedStyle(element).objectFit)).toBe('contain');
    await expect(art).toHaveAccessibleName(/^View [^/]+$/);
    await expect(art.locator('.art-label')).toHaveCount(0);
    await expect(art.locator('.device')).toHaveCount(0);
    expect(await art.evaluate(element => getComputedStyle(element, '::after').content)).toBe('none');
    const badge = art.locator('.art-badge');
    await expect(badge).toBeVisible();
    expect(await badge.evaluate(element => getComputedStyle(element).backgroundColor)).toBe('rgb(17, 17, 17)');
    expect(await contrast(badge)).toBeGreaterThanOrEqual(4.5);
  }
  const illustrated = page.locator('.product-art:not(.has-photo)');
  await expect(illustrated).toHaveCount(4);
  await expect(illustrated.locator('img')).toHaveCount(0);
  await expect(illustrated.locator('.art-label')).toHaveText(Array(4).fill(/ \/ ILLUSTRATION$/));
  await expect(page.locator('img[src$="-f"]')).toHaveCount(0);
  await fits(page);

  await page.goto('/product?id=encrypted-drive');
  const photo = page.locator('.product-photo img');
  await expect(photo).toHaveAccessibleName('Photo 1 of 2: Encrypted USB Drive — 256GB');
  await expect(photo).toHaveAttribute('src', /^\/img\/[0-9a-f]{32}-f$/);
  expect(await photo.evaluate(element => getComputedStyle(element).objectFit)).toBe('contain');
  expect(await photo.evaluate(element => (element as HTMLImageElement).complete && (element as HTMLImageElement).naturalWidth)).toBe(1000);
  const more = page.getByRole('link', { name: /^Photo 2 of 2, full size \(JPEG, \d+ KB\)$/ });
  await expect(more).toHaveAttribute('href', /^\/img\/[0-9a-f]{32}-f$/);
  await expect(more.locator('img')).toHaveAttribute('alt', '');
  expect((await more.boundingBox())!.height).toBeGreaterThanOrEqual(44);
  await more.focus();
  await expect(more).toBeFocused();
  await expect(more).not.toHaveCSS('outline-style', 'none');
  await expect(page.getByText('Photos are supplied by the vendor; the market does not verify them.')).toBeVisible();
  await expect(page.locator('.art-label, .device')).toHaveCount(0);
  await fits(page);
  const full = await request.get((await more.getAttribute('href'))!);
  expect(full.status()).toBe(200);
  expect(full.headers()['content-type']).toBe('image/jpeg');
  expect(full.headers()['cache-control']).toBe('private, max-age=86400, immutable');
  expect(full.headers()['set-cookie']).toBeUndefined();

  // A described photo is named by its description; a listing without photos keeps the illustration notice.
  await page.goto('/product?id=security-research');
  await expect(page.locator('.product-photo img')).toHaveAccessibleName(/^Preview sample picture: /);
  await expect(page.locator('.photo-thumbs')).toHaveCount(0);
  await page.goto('/product?id=faraday-bag');
  await expect(page.locator('img')).toHaveCount(0);
  await expect(page.locator('.detail-art .art-label')).toHaveText('Hardware / ILLUSTRATION');
  await expect(page.getByText('Product illustrations are decorative.')).toBeVisible();
});

// A-189 review: a thumbnail can fail to load (a 404 after a takedown race, or a busy 503 with 25 eager loads). The
// card keeps its size and the page does not overflow; Firefox (and Tor Browser) then shows the card's art
// background, while Chromium and WebKit draw their broken-image icon (no fix without JavaScript is known). The
// check runs in every project; the art background claim is Firefox's.
test('a thumbnail that fails to load leaves the card intact', async ({ page }) => {
  await page.goto('/');
  const art = page.locator('.product-art.has-photo').first();
  const before = (await art.boundingBox())!;
  const img = art.locator('img');
  // Point the card at an unknown id, as a takedown between page and image request would.
  await img.evaluate(element => { (element as HTMLImageElement).src = `/img/${'0'.repeat(32)}-t`; });
  await expect.poll(() => img.evaluate(element => (element as HTMLImageElement).complete)).toBe(true);
  expect(await img.evaluate(element => (element as HTMLImageElement).naturalWidth)).toBe(0);
  const after = (await art.boundingBox())!;
  expect(Math.abs(after.width - before.width)).toBeLessThanOrEqual(1);
  expect(Math.abs(after.height - before.height)).toBeLessThanOrEqual(1);
  await expect(art.locator('.art-badge')).toBeVisible();
  await expect(art).toHaveAccessibleName(/^View /);
  expect(await art.evaluate(element => getComputedStyle(element).backgroundImage)).toContain('gradient');
  await fits(page);
});

test('search has a readable keyboard focus indicator and usable text controls', async ({ page, browserName }) => {
  await page.goto('/');
  const search = page.locator('form[role="search"]:visible');
  const input = search.getByRole('textbox', { name: 'Search listings', exact: true });
  const button = search.getByRole('button');
  await input.click();
  const allControls = browserName === 'webkit' && process.platform === 'darwin';
  await page.keyboard.press(allControls ? 'Alt+Tab' : 'Tab');
  await expect(button).toBeFocused();
  await expect(button).toHaveCSS('outline-style', 'solid');
  await expect(button).toHaveCSS('outline-width', '3px');
  await expect(button).toHaveCSS('outline-offset', '-3px');
  await page.keyboard.press(allControls ? 'Alt+Shift+Tab' : 'Shift+Tab');
  await expect(input).toBeFocused();
  await expect(input).toHaveCSS('outline-style', 'solid');
  await expect(input).toHaveCSS('outline-offset', '-3px');
  expect(await contrast(input, 'outline-color')).toBeGreaterThanOrEqual(3);
  expect(await contrast(input, 'border-top-color')).toBeGreaterThanOrEqual(3);
  for (const control of [input, button]) {
    expect((await control.boundingBox())!.height).toBeGreaterThanOrEqual(44);
    expect(parseFloat(await control.evaluate(element => getComputedStyle(element).fontSize))).toBeGreaterThanOrEqual(control === input ? 16 : 14);
  }
  const searchButton = page.locator('.header-search button');
  await expect(searchButton).toHaveAttribute('aria-label', 'Search marketplace');
  await expect(searchButton).toContainText('Search');
  if (await searchButton.isVisible()) {
    await expect(searchButton).toHaveAccessibleName('Search marketplace');
  }
  await input.fill('encrypted');
  await page.keyboard.press('Enter');
  await expect(page.locator('.product-card')).toHaveCount(1);
  await expect(page.locator('.product-card h2')).toHaveText('Encrypted USB Drive — 256GB');
  await fits(page);
});

test('seeded catalog text remains readable and reflows at doubled text size', async ({ page }) => {
  await page.goto('/');
  const panel = await filters(page);
  expect(parseFloat(await page.locator('body').evaluate(element => getComputedStyle(element).fontSize))).toBeGreaterThanOrEqual(16);
  for (const selector of ['.product-description', '.vendor-line', '.stock', '.sidebar-note']) {
    const elements = page.locator(`${selector}:visible`);
    for (const element of await elements.all()) {
      expect(parseFloat(await element.evaluate(node => getComputedStyle(node).fontSize)), selector).toBeGreaterThanOrEqual(14);
    }
  }
  await expect(panel.locator('.category-links svg.ui-icon')).toHaveAttribute('aria-hidden', 'true');
  expect((await panel.locator('.category-links svg.ui-icon').boundingBox())!.height).toBeGreaterThanOrEqual(20);
  await fits(page);
  for (const selector of ['body', '.product-description', '.vendor-line', '.stock', '.catalog-tools .accent']) {
    expect(await contrast(page.locator(selector).first()), `${selector} text contrast`).toBeGreaterThanOrEqual(4.5);
  }
  // Simulate 200% text resizing while preserving the viewport to catch fixed-size clipping.
  await page.locator('html').evaluate(element => { element.style.fontSize = '200%'; });
  await expect(page.locator('.product-card')).toHaveCount(6);
  await expect(page.locator('.product-card h2').first()).toBeVisible();
  await fits(page);
  await expect(page.locator('script')).toHaveCount(0);
  // A-115: every preview route, including real-shaped 64-hex order IDs, the sample deposit row and monospace h1s.
  for (const [path, heading] of previewRoutes) {
    await page.goto(path);
    await page.locator('html').evaluate(element => { element.style.fontSize = '200%'; });
    await expect(page.getByRole('heading', { level: 1 })).toHaveText(heading);
    await fits(page);
  }
});

test('named text and surface token pairs meet the 7:1 contrast target', async ({ page }) => {
  await page.goto('/vendor-dashboard');
  const pairs = await page.evaluate(() => {
    const root = getComputedStyle(document.documentElement);
    const tokenPairs = [
      ['text', '--text', '--bg'], ['text', '--text', '--panel'], ['text', '--text', '--surface'],
      ['muted', '--muted', '--bg'], ['muted', '--muted', '--panel'], ['muted', '--muted', '--surface'],
      ['amber', '--amber', '--bg'], ['amber', '--amber', '--panel'], ['amber', '--amber', '--surface'],
      ['green', '--green', '--bg'], ['green', '--green', '--panel'], ['green', '--green', '--surface'],
    ] as const;
    const computedRGB = (token: string) => {
      const probe = document.createElement('span');
      probe.style.color = root.getPropertyValue(token).trim();
      document.body.append(probe);
      const channels = getComputedStyle(probe).color.match(/[\d.]+/g)?.map(Number);
      probe.remove();
      if (!channels || channels.length < 3) throw new Error(`Cannot resolve color token ${token}`);
      return channels.slice(0, 3);
    };
    const luminance = (channels: number[]) => channels.map(channel => {
      const value = channel / 255;
      return value <= 0.04045 ? value / 12.92 : ((value + 0.055) / 1.055) ** 2.4;
    }).reduce((sum, value, index) => sum + value * [0.2126, 0.7152, 0.0722][index], 0);
    return tokenPairs.map(([name, foreground, background]) => {
      const fg = luminance(computedRGB(foreground));
      const bg = luminance(computedRGB(background));
      return { name, foreground, background, ratio: (Math.max(fg, bg) + 0.05) / (Math.min(fg, bg) + 0.05) };
    });
  });
  for (const pair of pairs) {
    expect(pair.ratio, `${pair.name} text (${pair.foreground}) on ${pair.background}`).toBeGreaterThanOrEqual(7);
  }

  // Recorded exception: archived-card illustration art is dimmed with opacity:.45 and uses a gradient
  // behind its label. It is intentionally outside this token-pair table; the withdrawn full-page node walk
  // is not implied by this focused evidence.
  const archivedArt = page.locator('.product-card.is-archived .product-art');
  await expect(archivedArt).toHaveCount(1);
  expect(await archivedArt.evaluate(element => getComputedStyle(element).opacity)).toBe('0.45');
});

test('reduced-motion emulation disables transitions and smooth scrolling', async ({ page }) => {
  await page.emulateMedia({ reducedMotion: 'no-preference' });
  await page.goto('/');
  const card = page.locator('.product-card').first();
  await expect(card).toHaveCSS('transition-property', 'border-color');
  await page.emulateMedia({ reducedMotion: 'reduce' });
  expect(await page.evaluate(() => matchMedia('(prefers-reduced-motion: reduce)').matches)).toBe(true);
  await expect(card).toHaveCSS('transition-property', 'none');
  await expect(page.locator('html')).toHaveCSS('scroll-behavior', 'auto');
});

// Sample effective foreground/background pairs, not a claim of a complete accessibility audit.
async function contrast(element: Locator, property = 'color'): Promise<number> {
  return element.evaluate((node, prop) => {
    const rgb = (value: string) => (value.match(/[\d.]+/g) || []).map(Number);
    const luminance = (channels: number[]) => channels.slice(0, 3).map(v => {
      v /= 255;
      return v <= 0.04045 ? v / 12.92 : ((v + 0.055) / 1.055) ** 2.4;
    }).reduce((sum, v, index) => sum + v * [0.2126, 0.7152, 0.0722][index], 0);
    let ancestor: Element | null = node;
    let background = [255, 255, 255];
    while (ancestor) {
      const color = rgb(getComputedStyle(ancestor).backgroundColor);
      if (color.length === 3 || color[3] === 1) { background = color; break; }
      ancestor = ancestor.parentElement;
    }
    const foreground = luminance(rgb(getComputedStyle(node).getPropertyValue(prop)));
    const back = luminance(background);
    return (Math.max(foreground, back) + 0.05) / (Math.min(foreground, back) + 0.05);
  }, property);
}

test('footer identity, navigation and payment status stay separate and usable at enlarged text', async ({ page }) => {
  await page.goto('/');
  const footer = page.getByRole('contentinfo');
  const navigation = footer.getByRole('navigation', { name: 'Footer', exact: true });
  await expect(navigation.getByRole('link', { name: 'Transparency', exact: true })).toHaveAttribute('href', '/canary');
  await expect(navigation.getByRole('link', { name: 'Account', exact: true })).toHaveAttribute('href', '/account');
  for (const scale of ['100%', '200%']) {
    await page.locator('html').evaluate((element, size) => { element.style.fontSize = size; }, scale);
    await footer.scrollIntoViewIfNeeded();
    const boxes = [];
    for (const selector of ['.footer-identity', '.footer-links', '.footer-status']) {
      const item = footer.locator(selector);
      await expect(item).toBeVisible();
      const box = (await item.boundingBox())!;
      expect(box.x).toBeGreaterThanOrEqual(0);
      expect(box.x + box.width).toBeLessThanOrEqual(page.viewportSize()!.width);
      boxes.push(box);
    }
    for (let a = 0; a < boxes.length; a++) for (let b = a + 1; b < boxes.length; b++) {
      const xOverlap = Math.min(boxes[a].x + boxes[a].width, boxes[b].x + boxes[b].width) - Math.max(boxes[a].x, boxes[b].x);
      const yOverlap = Math.min(boxes[a].y + boxes[a].height, boxes[b].y + boxes[b].height) - Math.max(boxes[a].y, boxes[b].y);
      expect(xOverlap > 1 && yOverlap > 1, `footer sections overlap at ${scale}`).toBe(false);
    }
    for (const link of await navigation.getByRole('link').all()) {
      expect((await link.boundingBox())!.height).toBeGreaterThanOrEqual(44);
    }
    await fits(page);
  }
});
