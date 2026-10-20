import { expect, test } from '@playwright/test';
import { waitForStyles } from './styles';

test('catalog taxonomy, type and sorting stay selected through search without scripts', async ({ page }) => {
  await page.goto('/?category=Digital&subcategory=Guides+%2F+Courses&kind=digital&region=Europe&sort=price-high');
  await expect(page.locator('.product-card')).toHaveCount(1);
  await expect(page.locator('.product-badges')).toContainText('Guides / Courses');
  await expect(page.locator('.product-badges')).toContainText('digital');
  const search = page.locator('form[role="search"]:visible');
  await search.getByRole('textbox').fill('Privacy');
  await search.getByRole('button').click();
  await waitForStyles(page);
  const query = new URL(page.url()).searchParams;
  for (const [name, value] of [['category', 'Digital'], ['subcategory', 'Guides / Courses'], ['kind', 'digital'], ['region', 'Europe'], ['sort', 'price-high']]) {
    expect(query.get(name)).toBe(value);
  }
  await page.getByRole('combobox', { name: 'Sort listings' }).selectOption('price-low');
  await page.getByRole('button', { name: 'Sort', exact: true }).click();
  await expect(page.locator('.product-card')).toHaveCount(1);
  expect(new URL(page.url()).searchParams.get('subcategory')).toBe('Guides / Courses');
  await page.locator('.product-reputation .vendor-line a').click();
  await expect(page).toHaveURL(/\/vendor\?id=cipher$/);
  await expect(page.locator('script')).toHaveCount(0);
});

test('notification bell, sign out and marketplace comparisons fit at layout boundaries and doubled text', async ({ page }) => {
  test.setTimeout(90_000);
  await page.goto('/?category=Hardware');
  for (const width of [320, 390, 420, 640, 641, 768, 900, 901, 1150, 1151, 1280, 1920]) {
    await page.setViewportSize({ width, height: 900 });
    for (const size of ['100%', '200%']) {
      await page.locator('html').evaluate((element, value) => { element.style.fontSize = value; }, size);
      await waitForStyles(page);
      const bell = page.locator('.notification-bell');
      await expect(bell).toBeVisible();
      await expect(bell).toHaveAccessibleName('Notifications, 1 unread');
      await expect(page.getByRole('button', { name: 'Sign out', exact: true })).toBeVisible();
      expect(await page.evaluate(() => document.documentElement.scrollWidth), `${width}px, ${size} text`).toBeLessThanOrEqual(width);
      for (const wordmark of await page.locator('.text-brand').all()) {
        expect(await wordmark.evaluate(element => element.scrollWidth <= element.clientWidth), `wordmark clipping at ${width}px/${size}`).toBe(true);
      }
      for (const card of await page.locator('.product-card').all()) {
        expect(await card.evaluate(element => element.scrollWidth <= element.clientWidth), `card clipping at ${width}px/${size}`).toBe(true);
        await expect(card.locator('.product-reputation .vendor-line a')).toBeVisible();
        await expect(card.locator('.product-rating')).toBeVisible();
      }
    }
  }
});
