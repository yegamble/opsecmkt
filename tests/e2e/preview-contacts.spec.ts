import { expect, test, type Page } from '@playwright/test';

// Contacts in the read-only preview (sample data, JavaScript disabled): notifications in the main
// navigation at every width, counterparty PGP keys, message recipient prefill, moderator order links.

async function fits(page: Page) {
  await expect(page.locator('script')).toHaveCount(0);
  expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(page.viewportSize()!.width);
}

test('notifications are in the main navigation with a text unread count', async ({ page }) => {
  await page.goto('/orders');
  const nav = page.getByRole('navigation', { name: 'Main navigation' });
  const link = nav.getByRole('link', { name: 'Notifications (1 unread)' });
  await expect(link).toBeVisible();
  await expect(link).toBeInViewport();
  await expect(link).toContainText('(1');
  await fits(page);
  await link.click();
  await expect(page.getByRole('heading', { level: 1 })).toHaveText('Notifications.');
  await expect(nav.getByRole('link', { name: 'Notifications (1 unread)' })).toHaveAttribute('aria-current', 'page');
  await expect(page.getByRole('button', { name: 'Mark as read' })).toBeVisible();
  await fits(page);
});

test('vendor page shows the vendor key and links to a prefilled message', async ({ page }) => {
  await page.goto('/vendor?id=ghost');
  const key = page.getByRole('region', { name: 'PGP public key' });
  await expect(key.locator('.pgp-fingerprint')).toHaveText('0000 0000 0000 0000 0000 0000 0000 0000 0000 0000');
  await expect(key).toContainText('Ownership not verified');
  await expect(key.getByRole('textbox', { name: /ghost_circuit’s public key/ })).toHaveValue(/BEGIN PGP PUBLIC KEY BLOCK/);
  await fits(page);
  const message = page.getByRole('link', { name: 'Send encrypted message' });
  await expect(message).toHaveAttribute('href', '/messages?to=ghost_circuit');
  await message.click();
  await expect(page.getByLabel('Recipient handle')).toHaveValue('ghost_circuit');
  await expect(page.locator('main .pgp-fingerprint')).toBeVisible();
  await fits(page);
});

test('order page names the counterparty key state and message link', async ({ page }) => {
  await page.goto('/order?id=sample-draft');
  const keys = page.getByRole('region', { name: 'Vendor’s PGP key' });
  await expect(keys).toContainText('ghost_circuit has not added a PGP public key. You cannot send them an encrypted message until they add one.');
  await expect(page.getByRole('link', { name: 'Message vendor' })).toHaveAttribute('href', '/messages?to=ghost_circuit');
  await expect(page.getByRole('link', { name: 'Message buyer' })).toHaveCount(0);
  await fits(page);
});

test('product page contact link prefills the vendor', async ({ page }) => {
  await page.goto('/product?id=encrypted-drive');
  await expect(page.getByRole('link', { name: 'Contact vendor' })).toHaveAttribute('href', '/messages?to=ghost_circuit');
});

test('moderator desk links each dispute to its order page', async ({ page }) => {
  await page.goto('/moderator');
  await expect(page.getByRole('link', { name: 'Order sample-disputed' })).toHaveAttribute('href', '/order?id=sample-disputed');
  await fits(page);
});
