import { expect, test, type Page } from '@playwright/test';
import { waitForStyles } from './styles';

// A-542: /admin is an overview and each administration section is its own page. Every page carries the same
// no-JavaScript section navigation with the current section marked; it wraps on a narrow screen, so neither the
// page nor the navigation scrolls sideways. The read-only preview proves layout and semantics only.
const sections: [path: string, name: string][] = [
  ['/admin', 'Overview'], ['/admin/users', 'Users'], ['/admin/vendors', 'Vendors'], ['/admin/payments', 'Payments'],
  ['/admin/security', 'Security & access'], ['/admin/site', 'Site & branding'], ['/admin/transparency', 'Transparency'],
  ['/admin/system', 'System & audit'],
];

const sectionNav = (page: Page) => page.getByRole('navigation', { name: 'Administration sections' });

for (const width of [320, 1280]) {
  test(`every admin section at ${width}px: navigation, current section and no sideways scroll`, async ({ page }) => {
    await page.setViewportSize({ width, height: 900 });
    for (const [path, name] of sections) {
      expect((await page.goto(path))?.status(), path).toBe(200);
      await waitForStyles(page);
      const nav = sectionNav(page);
      await expect(nav.getByRole('link')).toHaveText(sections.map(([, label]) => label));
      await expect(nav.locator('[aria-current]')).toHaveCount(1);
      await expect(nav.getByRole('link', { name, exact: true })).toHaveAttribute('aria-current', 'page');
      // The main navigation's Admin link marks the area (true) on a section, the page itself on the overview.
      await expect(page.getByRole('navigation', { name: 'Main navigation' }).getByRole('link', { name: 'Admin', exact: true }))
        .toHaveAttribute('aria-current', path === '/admin' ? 'page' : 'true');
      expect(await page.evaluate(() => document.documentElement.scrollWidth), `${path} at ${width}px`).toBeLessThanOrEqual(width);
      expect(await nav.evaluate(element => element.scrollWidth <= element.clientWidth), `${path} navigation at ${width}px`).toBe(true);
      for (const link of await nav.getByRole('link').all()) {
        const box = (await link.boundingBox())!;
        expect(box.x, `${path}: ${await link.innerText()}`).toBeGreaterThanOrEqual(0);
        expect(box.x + box.width, `${path}: ${await link.innerText()}`).toBeLessThanOrEqual(width);
        expect(box.height).toBeGreaterThanOrEqual(43.9); // a 44px target, allowing sub-pixel rounding
      }
      await expect(page.locator('script')).toHaveCount(0);
    }
  });
}

test('the overview links every section and holds no form', async ({ page }) => {
  await page.goto('/admin');
  await expect(page.getByRole('heading', { name: 'Get your marketplace ready' })).toBeVisible();
  const list = page.getByRole('region', { name: 'Sections' });
  await expect(list.getByRole('link')).toHaveText(sections.slice(1).map(([, label]) => label));
  await expect(page.locator('main form[method="post"]')).toHaveCount(0);
  await expect(page.locator('main a[href="/admin/payments#payment-providers"]')).toHaveCount(1);
});

test('the keyboard reaches each section from the navigation', async ({ page, browserName }) => {
  const tab = browserName === 'webkit' && process.platform === 'darwin' ? 'Alt+Tab' : 'Tab';
  await page.goto('/admin');
  const links = sectionNav(page).getByRole('link');
  // A real keyboard traversal from the top of the page to the first section link, then through each in order.
  for (let i = 0; i < 40 && !(await links.first().evaluate(element => element === document.activeElement)); i++) await page.keyboard.press(tab);
  await expect(links.first()).toBeFocused();
  await expect(links.first()).not.toHaveCSS('outline-style', 'none');
  for (let i = 1; i < sections.length; i++) {
    await page.keyboard.press(tab);
    await expect(links.nth(i)).toBeFocused();
  }
  await links.nth(1).focus();
  await page.keyboard.press('Enter');
  await expect(page).toHaveURL(/\/admin\/users$/);
  await expect(page.getByRole('heading', { level: 1 })).toHaveText('Users.');
  await expect(sectionNav(page).getByRole('link', { name: 'Users', exact: true })).toHaveAttribute('aria-current', 'page');
});
