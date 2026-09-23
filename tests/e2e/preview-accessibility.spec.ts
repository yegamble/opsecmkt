import { expect, test, type Page, type Locator } from '@playwright/test';

async function filters(page: Page) {
  const mobile = page.locator('.filter-details');
  if (await mobile.isVisible()) {
    if (!(await mobile.evaluate(element => (element as HTMLDetailsElement).open))) await mobile.locator('summary').click();
    return mobile;
  }
  return page.locator('.desktop-filters');
}

async function fits(page: Page) {
  expect(await page.evaluate(() => document.documentElement.scrollWidth), `page reflow: ${page.url()}`).toBeLessThanOrEqual(page.viewportSize()!.width);
}

test('seeded categories expose one current link and matching listings after every navigation', async ({ page }) => {
  await page.goto('/');
  for (const [category, count] of [['All listings', 6], ['Hardware', 2], ['Digital', 3], ['Services', 1], ['All listings', 6]] as const) {
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
    if (category !== 'All listings') {
      await expect(page.locator('.product-art .art-label')).toHaveText(Array(count).fill(`${category} / ILLUSTRATION`));
      await expect(panel.getByRole('combobox', { name: 'Category', exact: true })).toHaveValue(category);
    }
    await fits(page);
  }
});

test('search has a readable keyboard focus indicator, usable controls and a decorative magnifier', async ({ page, browserName }) => {
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
  const icon = page.locator('.header-search button svg.ui-icon');
  await expect(icon).toHaveAttribute('aria-hidden', 'true');
  await expect(icon).toHaveAttribute('width', '20');
  await expect(icon).toHaveAttribute('height', '20');
  await expect(page.locator('.header-search button')).toHaveAttribute('aria-label', 'Search marketplace');
  if (await icon.isVisible()) {
    await expect(page.locator('.header-search button')).toHaveAccessibleName('Search marketplace');
    expect((await icon.boundingBox())!.width).toBeGreaterThanOrEqual(20);
    expect((await icon.boundingBox())!.height).toBeGreaterThanOrEqual(20);
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
  for (const path of ['/product?id=encrypted-drive', '/checkout?id=encrypted-drive', '/order?id=sample-draft', '/messages', '/account', '/vendor-dashboard', '/moderator', '/admin']) {
    await page.goto(path);
    await page.locator('html').evaluate(element => { element.style.fontSize = '200%'; });
    await expect(page.locator('main')).toBeVisible();
    await fits(page);
  }
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
