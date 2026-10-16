import { expect, test, type Page } from '@playwright/test';
import { ADMIN, FIXTURE_LISTING, uniqueHandle } from './db-fixtures';
import { expectNotice, signedIn, submitStatus, withinViewport } from './db-helpers';

// The database project runs these shared-admin policy changes on one worker.
// db-setup supplies a fresh local market, paused payments and a published listing.
test('site access and registration are independent, including existing accounts and stale signup forms', async ({ page: guest, browser, baseURL }) => {
  test.setTimeout(90_000);
  await guest.setViewportSize({ width: 320, height: 900 });
  const admin = await signedIn(browser, baseURL, ADMIN.handle, ADMIN.password, false, 320);
  const owned = [admin.context()];
  const memberHandle = uniqueHandle('access_member');
  const memberPassword = 'browser-access-member-password-123';

  async function saveAccess(siteAccess: 'public' | 'members', registrations: 'open' | 'closed') {
    await admin.goto('/admin');
    const panel = admin.locator('#access');
    await panel.locator(`input[name="site_access"][value="${siteAccess}"]`).check();
    await panel.locator(`input[name="registrations"][value="${registrations}"]`).check();
    await panel.getByLabel('Current password', { exact: true }).fill(ADMIN.password);
    expect(await submitStatus(admin, () => panel.getByRole('button', { name: 'Save access settings' }).click())).toBe(303);
    await expectNotice(admin, 'Site access and registration settings saved.');
    await expect(panel.locator(`input[name="site_access"][value="${siteAccess}"]`)).toBeChecked();
    await expect(panel.locator(`input[name="registrations"][value="${registrations}"]`)).toBeChecked();
    await withinViewport(panel, 320);
  }

  async function seesListing(page: Page, path: string) {
    const response = await page.goto(path);
    expect(response?.status()).toBe(200);
    await expect(page.getByRole('heading', { name: FIXTURE_LISTING, exact: true })).toBeVisible();
  }

  async function canaryPolicy(page: Page, siteAccess: 'public' | 'members', registrations: 'open' | 'closed') {
    expect((await page.goto('/canary'))?.status()).toBe(200);
    await expect(page.locator('#site-access')).toContainText(siteAccess === 'members' ? 'Members only:' : 'Public browsing:');
    await expect(page.locator('#site-access')).toContainText(`New registrations are ${registrations}.`);
    const photos = page.locator('#records li', { has: page.locator('strong', { hasText: /^Photos and avatars\.$/ }) });
    if (siteAccess === 'members') {
      await expect(photos).toContainText('visible to signed-in accounts');
      await expect(photos).toContainText('tell browsers not to store them (no-store)');
      await expect(photos).not.toContainText('public to anyone');
      await expect(photos).not.toContainText('reuses them for a day');
    } else {
      await expect(photos).toContainText('public to anyone');
      await expect(photos).toContainText('reuses them for a day');
      await expect(photos).not.toContainText('no-store');
    }
  }

  try {
    await test.step('access controls work with the keyboard at 320 px', async () => {
      await admin.goto('/admin');
      const panel = admin.locator('#access');
      await panel.locator('input[name="site_access"][value="public"]').focus();
      await admin.keyboard.press('ArrowDown');
      await expect(panel.locator('input[name="site_access"][value="members"]')).toBeChecked();
      await admin.keyboard.press('Tab');
      await expect(admin.locator(':focus')).toHaveAttribute('name', 'registrations');
      await admin.keyboard.press('Tab');
      await expect(panel.getByLabel('Current password', { exact: true })).toBeFocused();
    });
    await saveAccess('public', 'open');
    const member = await signedIn(browser, baseURL, memberHandle, memberPassword, true, 320);
    owned.push(member.context());
    await member.context().close(); // Later sign-in must work with registration closed.

    const listingURL = await test.step('public/open: guests browse and may register', async () => {
      await guest.goto(`/?q=${encodeURIComponent(FIXTURE_LISTING)}`);
      await expect(guest.getByRole('link', { name: 'Join market', exact: true })).toBeVisible();
      const listing = guest.getByRole('link', { name: FIXTURE_LISTING, exact: true });
      const href = (await listing.getAttribute('href'))!;
      await seesListing(guest, href);
      await canaryPolicy(guest, 'public', 'open');
      await guest.goto('/register');
      await expect(guest.locator('form[action="/register"]')).toBeVisible();
      return href;
    });

    await test.step('members/open: guests sign in or register before viewing site content', async () => {
      await saveAccess('members', 'open');
      for (const path of [`/?q=${encodeURIComponent(FIXTURE_LISTING)}`, listingURL, '/canary']) {
        await guest.goto(path);
        await expect(guest).toHaveURL(/\/login(?:\?next=.*)?$/);
        await expect(guest.locator('body')).not.toContainText(FIXTURE_LISTING);
        await expect(guest.locator('.header-search, .sidebar, nav[aria-label="Main navigation"]')).toHaveCount(0);
        await expect(guest.getByRole('link', { name: 'Join market', exact: true })).toBeVisible();
      }
      await guest.goto('/register');
      await expect(guest.locator('form[action="/register"]')).toBeVisible();
      await expect(guest.locator('body')).toContainText('The full Transparency page');
      await expect(guest.locator('a[href^="/canary"]')).toHaveCount(0);

      const newcomer = await signedIn(browser, baseURL, uniqueHandle('access_new'), memberPassword, true, 320);
      owned.push(newcomer.context());
      await seesListing(newcomer, listingURL);
      await canaryPolicy(newcomer, 'members', 'open');
    });

    // Keep a real registration form open before the administrator closes intake.
    // Submitting it afterwards must be refused by the server, not only hidden in new pages.
    const staleContext = await browser.newContext({ baseURL, javaScriptEnabled: false, viewport: { width: 320, height: 900 } });
    owned.push(staleContext);
    const staleSignup = await staleContext.newPage();
    await staleSignup.goto('/register');
    await staleSignup.getByLabel('Handle', { exact: true }).fill(uniqueHandle('access_stale'));
    await staleSignup.getByLabel('Password', { exact: true }).fill(memberPassword);

    await test.step('members/closed: no signup, while an existing account can still sign in', async () => {
      await saveAccess('members', 'closed');
      await guest.goto('/login');
      await expect(guest.getByRole('link', { name: 'Join market', exact: true })).toHaveCount(0);
      await expect(guest.getByRole('link', { name: 'Create an account', exact: true })).toHaveCount(0);
      await expect(guest.locator('body')).toContainText('New registrations are closed');
      expect((await guest.goto('/register'))?.status()).toBe(403);
      await expect(guest.getByRole('heading', { name: 'Registration is closed' })).toBeVisible();
      await expect(guest.locator('form[action="/register"], img.captcha-image')).toHaveCount(0);
      expect(await submitStatus(staleSignup, () => staleSignup.getByRole('button', { name: 'Create account' }).click())).toBe(403);
      await expect(staleSignup.locator('body')).toContainText('New registrations are currently closed');
      await expect(staleSignup.locator('form[action="/register"]')).toHaveCount(0);

      const returning = await signedIn(browser, baseURL, memberHandle, memberPassword, false, 320);
      owned.push(returning.context());
      await seesListing(returning, listingURL);
      await canaryPolicy(returning, 'members', 'closed');
    });

    await test.step('public/closed: guests can browse while account creation stays closed', async () => {
      await saveAccess('public', 'closed');
      await guest.goto(`/?q=${encodeURIComponent(FIXTURE_LISTING)}`);
      await expect(guest.getByRole('link', { name: FIXTURE_LISTING, exact: true })).toBeVisible();
      await expect(guest.getByRole('link', { name: 'Join market', exact: true })).toHaveCount(0);
      await seesListing(guest, listingURL);
      await canaryPolicy(guest, 'public', 'closed');
      expect((await guest.goto('/register'))?.status()).toBe(403);
      await expect(guest.locator('form[action="/register"]')).toHaveCount(0);
    });
  } finally {
    // Do not leave a failing journey's global policy behind for other db specs.
    try {
      await saveAccess('public', 'open');
      expect((await guest.goto('/register'))?.status()).toBe(200);
      await expect(guest.locator('form[action="/register"]')).toBeVisible();
    } finally {
      for (const context of owned) await context.close();
    }
  }
});
