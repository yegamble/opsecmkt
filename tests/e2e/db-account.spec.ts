import { expect, test } from '@playwright/test';
import { FIXTURE_LISTING, uniqueHandle } from './db-fixtures';
import { waitForStyles } from './styles';
import { expectNotice, saveEncryptionKey, signedIn, submitAndLoad } from './db-helpers';

// Uses the listing published by db-setup.spec.ts and registers its own buyer.
test('register, draft, private XMPP opt-in and session boundaries', async ({ page, browser, baseURL }) => {
  const handle = uniqueHandle('buyer');
  const password = 'browser-buyer-password-123';
  await page.goto('/register');
  await page.getByLabel('Handle', { exact: true }).fill(handle);
  await page.getByLabel('Password', { exact: true }).fill(password);
  await page.getByRole('button', { name: 'Create account' }).click();
  await expect(page.locator('.account-name')).toHaveText(handle);
  await saveEncryptionKey(page, password);
  // The catalog shows 25 listings a page (A-181) and other specs publish more: search for the setup fixture.
  await page.goto(`/?q=${encodeURIComponent(FIXTURE_LISTING)}`);
  await page.getByRole('link', { name: FIXTURE_LISTING, exact: true }).click();
  await page.getByRole('link', { name: 'Review order draft' }).click();
  await page.getByLabel('Pay with').selectOption('XMR');
  await page.getByRole('button', { name: 'Create unfunded draft' }).click();
  await expect(page).toHaveURL(/\/order\?id=/);
  await expect(page.getByRole('heading', { name: 'Unfunded draft' })).toBeVisible();
  await expect(page.locator('.total')).toContainText('XMR');
  await expect(page.locator('.page-head .badge')).toHaveText('Draft — unfunded');
  expect((await page.goto('/admin'))?.status()).toBe(403);
  await page.goto('/account');
  const xmpp = page.locator('form[action="/account/xmpp"]');
  await expect(xmpp.getByLabel('Private XMPP address')).toHaveValue('');
  await expect(xmpp.getByLabel('Receive XMPP notifications')).not.toBeChecked();
  const destination = `${handle}@example.test`;
  await xmpp.getByLabel('Private XMPP address').fill(destination);
  await xmpp.getByLabel('Receive XMPP notifications').check();
  await xmpp.getByLabel('Current password').fill(password);
  await submitAndLoad(page, xmpp.getByRole('button', { name: 'Save XMPP preferences' }));
  await expect(page).toHaveURL(/\/account#xmpp-notifications$/);
  const signOutForm = page.getByRole('main').locator('form[action="/logout"]'); // the page's own; the header has one too (A-548)
  await expect(signOutForm).toBeVisible();
  await expect(signOutForm.locator('input[name="csrf"]')).toHaveValue(/.+/);
  expect(await signOutForm.evaluate(form => {
    const sessions = document.querySelector('#sessions');
    return !!sessions && !!(form.compareDocumentPosition(sessions) & Node.DOCUMENT_POSITION_FOLLOWING);
  })).toBeTruthy();
  await submitAndLoad(page, page.getByRole('main').getByRole('button', { name: 'Sign out', exact: true }));
  await page.goto('/account');
  await expect(page).toHaveURL(/\/login\?next=%2Faccount$/);
  await page.getByLabel('Handle', { exact: true }).fill(handle);
  await page.getByLabel('Password', { exact: true }).fill('wrong-password-123');
  await page.getByRole('button', { name: 'Sign in' }).click();
  await expect(page.getByRole('alert')).toHaveText('Invalid handle or password');
  // A-514: the sign-in form comes back with the handle kept, never the password, and still returns to /account.
  await expect(page.getByLabel('Handle', { exact: true })).toHaveValue(handle);
  await expect(page.getByLabel('Password', { exact: true })).toHaveValue('');
  await page.getByLabel('Password', { exact: true }).fill(password);
  await submitAndLoad(page, page.getByRole('button', { name: 'Sign in' }));
  await expect(page).toHaveURL(/\/account$/);
  await expect(page.getByLabel('Private XMPP address')).toHaveValue(destination);
  await expect(page.getByLabel('Receive XMPP notifications')).toBeChecked();
  const activity = page.getByRole('region', { name: 'Recent activity' });
  await expect(activity).toContainText('Signed in');
  await expect(activity).not.toContainText('Updated profile');
  await expect(activity).toContainText('Account created: buyer');

  // "Sign out everywhere" ends this session and one opened in a separate browser context.
  const other = await browser.newContext({ baseURL, javaScriptEnabled: false });
  const otherPage = await other.newPage();
  await otherPage.goto('/login');
  await otherPage.getByLabel('Handle', { exact: true }).fill(handle);
  await otherPage.getByLabel('Password', { exact: true }).fill(password);
  await submitAndLoad(otherPage, otherPage.getByRole('button', { name: 'Sign in' }));
  await otherPage.goto('/account');
  await expect(otherPage).toHaveURL(/\/account$/);
  // A-157: both sessions are listed with their start time; each browser sees its own marked.
  await page.reload();
  const sessions = page.getByRole('region', { name: 'Sessions' });
  await expect(sessions.getByRole('button', { name: 'Sign out everywhere' })).toBeVisible();
  await expect(sessions.locator('.session-count')).toHaveText('2 active sessions');
  await expect(sessions.locator('.session-row')).toHaveCount(2);
  await expect(sessions.locator('.session-row', { hasText: 'This session' })).toHaveCount(1);
  for (const row of await sessions.locator('.session-started').allTextContents()) {
    expect(row).toMatch(/^\d{4}-\d{2}-\d{2} \d{2}:\d{2} UTC$/);
  }
  await expect(sessions).toContainText('Only changing your password stops them.');
  await expect(otherPage.getByRole('region', { name: 'Sessions' }).locator('.session-row', { hasText: 'This session' })).toHaveCount(1);
  await page.getByRole('button', { name: 'Sign out everywhere' }).click();
  await expect(page).toHaveURL(/\/login$/);
  await page.goto('/account');
  await expect(page).toHaveURL(/\/login\?next=%2Faccount$/);
  await otherPage.goto('/account');
  await expect(otherPage).toHaveURL(/\/login\?next=%2Faccount$/);
  await other.close();
});

// A-548: Sign out in the site header, clicked on a 320 px phone and pressed with the keyboard on a 1280 px desktop
// (JavaScript off), ends the session from a page other than /account and lands on the sign-in page's notice. With the
// longest handle (32 characters) at 100% and 200% text, the header never scrolls sideways and Sign out stays inside
// the viewport from 320 to 1280 px, including 641-740 and 1100-1200 px where the search shares the header (UIA-1).
test('header sign out ends the session from any page', async ({ browser, baseURL }) => {
  test.setTimeout(90_000);
  const handle = uniqueHandle('signout').padEnd(32, '_long'); // the longest handle; a long prefix would cut the unique part
  expect(handle).toHaveLength(32);
  const password = 'browser-signout-password-123';
  const widths = { 320: [320, 390, 641, 700, 740, 768], 1280: [1100, 1150, 1200, 1280] };
  for (const [width, register] of [[320, true], [1280, false]] as const) {
    const page = await signedIn(browser, baseURL, handle, password, register, width);
    // Measured on /account: the empty /orders page's "Explore the marketplace" button is wider than 320 px at 200%
    // text whatever the header does.
    await page.goto('/account');
    await waitForStyles(page);
    const header = page.getByRole('banner');
    await expect(header.getByRole('link', { name: handle, exact: true })).toHaveAttribute('href', '/account');
    let signOut = header.getByRole('button', { name: 'Sign out', exact: true });
    for (const scale of ['100%', '200%']) {
      await page.locator('html').evaluate((element, size) => { element.style.fontSize = size; }, scale);
      for (const viewport of widths[width]) {
        await page.setViewportSize({ width: viewport, height: 900 });
        const box = (await signOut.boundingBox())!;
        expect(box.height, `${viewport} px, ${scale}`).toBeGreaterThanOrEqual(44);
        expect(box.x, `${viewport} px, ${scale}`).toBeGreaterThanOrEqual(0);
        expect(box.x + box.width, `Sign out inside ${viewport} px at ${scale}`).toBeLessThanOrEqual(viewport);
        const wide = await page.evaluate(() => [...document.querySelectorAll('body *')].filter(e => e.getBoundingClientRect().right > document.documentElement.clientWidth + 0.5).map(e => `${e.tagName.toLowerCase()}.${e.className}`).slice(0, 4).join(', '));
        expect(await page.evaluate(() => document.documentElement.scrollWidth), `page width at ${viewport} px, ${scale} (${wide})`).toBeLessThanOrEqual(viewport);
      }
    }
    await page.setViewportSize({ width, height: 900 });
    await page.goto('/orders');
    signOut = page.getByRole('banner').getByRole('button', { name: 'Sign out', exact: true });
    if (width === 320) {
      await submitAndLoad(page, signOut);
    } else {
      await signOut.focus();
      await Promise.all([page.waitForEvent('load'), page.keyboard.press('Enter')]);
    }
    await expect(page).toHaveURL(/\/login$/);
    await expectNotice(page, 'Signed out.');
    await expect(page.locator('form[action="/logout"]')).toHaveCount(0);
    await expect(page.locator('.account-name')).toHaveCount(0);
    await page.goto('/orders');
    await expect(page).toHaveURL(/\/login\?next=%2Forders$/);
    await page.context().close();
  }
});
