import { execFileSync } from 'node:child_process';
import { expect, test, type Page } from '@playwright/test';
import { ADMIN, uniqueHandle } from './db-fixtures';
import { pgpFixture, saveEncryptionKey, setRole, signedIn, submitAndLoad } from './db-helpers';
import { waitForStyles } from './styles';

// Real server/database forms with JavaScript disabled. Keep outgoing delivery off:
// the fake sender credentials are never used to connect to an external service.
test('XMPP sender configuration and private opt-in respect role boundaries', async ({ browser, baseURL }) => {
  test.setTimeout(120_000);
  const sql = (query: string) => execFileSync('psql', [process.env.E2E_DATABASE_URL!, '-v', 'ON_ERROR_STOP=1', '-qtAc', query], { encoding: 'utf8' }).trim();
  const sender = 'browser-market@example.test';
  const senderPassword = 'xmpp-browser-sender-password-123';
  const accountPassword = 'xmpp-browser-account-password-123';
  const buyerHandle = uniqueHandle('xmpp_buyer');
  const vendorHandle = uniqueHandle('xmpp_vendor');
  const moderatorHandle = uniqueHandle('xmpp_mod');
  const pages: Page[] = [];
  try {
    const admin = await signedIn(browser, baseURL, ADMIN.handle, ADMIN.password, false, 320);
    pages.push(admin);
    await admin.goto('/admin/site');
    const outgoing = admin.getByRole('region', { name: 'Outgoing XMPP', exact: true });
    await expect(outgoing.getByLabel('Enable outgoing XMPP delivery')).not.toBeChecked();
    await outgoing.getByLabel('Sender XMPP address').fill(sender);
    await outgoing.getByLabel('Server hostname').fill('127.0.0.1');
    await outgoing.getByLabel('Server port').fill('5223');
    await outgoing.getByLabel('Connection security').selectOption('direct');
    await outgoing.getByLabel('Sender account password').fill(senderPassword);
    await outgoing.getByLabel('Current password', { exact: true }).fill(ADMIN.password);
    await outgoing.getByRole('button', { name: 'Save outgoing XMPP settings' }).focus();
    await Promise.all([admin.waitForEvent('load'), admin.keyboard.press('Enter')]);
    await expect(admin).toHaveURL(/\/admin\/site#xmpp-outgoing$/);
    await admin.reload();
    await expect(outgoing.getByLabel('Sender XMPP address')).toHaveValue(sender);
    await expect(outgoing.getByLabel('Server hostname')).toHaveValue('127.0.0.1');
    await expect(outgoing.getByLabel('Server port')).toHaveValue('5223');
    await expect(outgoing.getByLabel('Connection security')).toHaveValue('direct');
    await expect(outgoing.getByLabel('Enable outgoing XMPP delivery')).not.toBeChecked();
    await expect(outgoing.getByLabel('Sender account password')).toHaveValue('');
    await expect(outgoing.getByLabel('Current password', { exact: true })).toHaveValue('');
    await expect(outgoing).toContainText('A sender password is saved.');
    expect(await admin.content()).not.toContain(senderPassword);
    const config = JSON.parse(sql("SELECT value FROM settings WHERE key='xmpp_config'"));
    expect(config.enabled).toBe(false);
    expect(config.sender).toBe(sender);
    expect(config.password_sealed).toBeTruthy();
    expect(JSON.stringify(config)).not.toContain(senderPassword);

    // A blank credential field retains the encrypted sender password after reload.
    await outgoing.getByLabel('Current password', { exact: true }).fill(ADMIN.password);
    await submitAndLoad(admin, outgoing.getByRole('button', { name: 'Save outgoing XMPP settings' }));
    expect(JSON.parse(sql("SELECT value FROM settings WHERE key='xmpp_config'")).password_sealed).toBe(config.password_sealed);

    const buyer = await signedIn(browser, baseURL, buyerHandle, accountPassword, true, 320);
    pages.push(buyer);
    await buyer.goto('/account');
    const preferences = buyer.getByRole('region', { name: 'XMPP notifications', exact: true });
    const destination = `${buyerHandle}@example.test`;
    await expect(preferences.getByLabel('Private XMPP address')).toHaveValue('');
    await expect(preferences.getByLabel('Receive XMPP notifications')).not.toBeChecked();
    await expect(preferences).toContainText('Notification summaries are not end-to-end encrypted.');
    // Follow the real 320 px form's keyboard order from address through submit.
    await preferences.getByLabel('Private XMPP address').focus();
    await buyer.keyboard.type(destination);
    await buyer.keyboard.press('Tab');
    await expect(preferences.getByLabel('Receive XMPP notifications')).toBeFocused();
    await buyer.keyboard.press('Space');
    await buyer.keyboard.press('Tab');
    await expect(preferences.getByLabel('Current password', { exact: true })).toBeFocused();
    await buyer.keyboard.type(accountPassword);
    await buyer.keyboard.press('Tab');
    await expect(preferences.getByRole('button', { name: 'Save XMPP preferences' })).toBeFocused();
    await Promise.all([buyer.waitForEvent('load'), buyer.keyboard.press('Enter')]);
    await expect(buyer).toHaveURL(/\/account#xmpp-notifications$/);
    await buyer.reload();
    await expect(preferences.getByLabel('Private XMPP address')).toHaveValue(destination);
    await expect(preferences.getByLabel('Receive XMPP notifications')).toBeChecked();
    await expect(preferences.getByLabel('Current password', { exact: true })).toHaveValue('');
    await waitForStyles(buyer);
    expect(await buyer.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(320);
    for (const field of await preferences.locator('input:not([type="hidden"]), button').all()) {
      const box = (await field.boundingBox())!;
      expect(box.x).toBeGreaterThanOrEqual(0);
      expect(box.x + box.width).toBeLessThanOrEqual(320);
    }
    await preferences.getByLabel('Receive XMPP notifications').uncheck();
    await preferences.getByLabel('Current password', { exact: true }).fill(accountPassword);
    await submitAndLoad(buyer, preferences.getByRole('button', { name: 'Save XMPP preferences' }));
    await buyer.reload();
    await expect(preferences.getByLabel('Receive XMPP notifications')).not.toBeChecked();
    await expect(preferences.getByLabel('Private XMPP address')).toHaveValue(destination);

    const vendor = await signedIn(browser, baseURL, vendorHandle, accountPassword, true, 320);
    const moderator = await signedIn(browser, baseURL, moderatorHandle, accountPassword, true, 320);
    pages.push(vendor, moderator);
    await setRole(admin, vendorHandle, 'vendor');
    await setRole(admin, moderatorHandle, 'moderator');
    for (const [role, page] of [['buyer', buyer], ['vendor', vendor], ['moderator', moderator]] as const) {
      await page.goto('/account');
      await expect(page.locator('.page-head .badge')).toHaveText(role);
      const own = page.getByRole('region', { name: 'XMPP notifications', exact: true });
      await expect(own).toBeVisible();
      if (role !== 'buyer') await expect(own.getByLabel('Private XMPP address')).toHaveValue('');
      expect(await page.content()).not.toContain(sender);
      expect(await page.content()).not.toContain(senderPassword);
      const csrf = await own.locator('input[name="csrf"]').inputValue();
      // An authenticated nonadministrator cannot save the outgoing account even
      // with a valid session CSRF token and their own correct current password.
      const denied = await page.request.post('/admin/xmpp', {
        form: { csrf, password: accountPassword, xmpp_sender: 'forged@example.test', xmpp_host: '127.0.0.1', xmpp_port: '5222', xmpp_tls_mode: 'starttls' },
        maxRedirects: 0,
      });
      expect(denied.status(), `${role} outgoing settings POST`).toBe(403);
      expect((await page.goto('/admin/site'))?.status(), `${role} outgoing settings page`).toBe(403);
      expect(await page.content()).not.toContain(senderPassword);
    }
    expect(sql("SELECT value FROM settings WHERE key='xmpp_config'")).toBe(JSON.stringify(config));
    expect(sql('SELECT count(*) FROM xmpp_outbox')).toBe('0');
  } finally {
    for (const page of pages) await page.context().close();
  }
});

test('encrypted XMPP contents need administrator permission, recipient consent and a usable key', async ({ browser, baseURL }) => {
  test.setTimeout(180_000);
  const sql = (query: string) => execFileSync('psql', [process.env.E2E_DATABASE_URL!, '-v', 'ON_ERROR_STOP=1', '-qtAc', query], { encoding: 'utf8' }).trim();
  const password = 'xmpp-content-browser-password-123';
  const handles = ['xmpp_content_buyer', 'xmpp_content_vendor', 'xmpp_content_mod'].map(uniqueHandle);
  const pages: Page[] = [];
  try {
    const admin = await signedIn(browser, baseURL, ADMIN.handle, ADMIN.password, false, 320);
    const buyer = await signedIn(browser, baseURL, handles[0], password, true, 320);
    const vendor = await signedIn(browser, baseURL, handles[1], password, true, 320);
    const moderator = await signedIn(browser, baseURL, handles[2], password, true, 320);
    pages.push(admin, buyer, vendor, moderator);
    await setRole(admin, handles[1], 'vendor');
    await setRole(admin, handles[2], 'moderator');
    await saveEncryptionKey(vendor, password);
    await saveEncryptionKey(moderator, password);
    await admin.goto('/admin/site');
    const outgoing = admin.getByRole('region', { name: 'Outgoing XMPP', exact: true });
    await expect(outgoing.getByLabel('Allow PGP-encrypted message contents in XMPP')).not.toBeChecked();
    const priorPolicy = Number(sql("SELECT COALESCE(value::jsonb->>'content_policy_version','0') FROM settings WHERE key='xmpp_config'") || '0');

    const postContents = async () => {
      const own = buyer.locator('form[action="/account/xmpp"]');
      const hidden = Object.fromEntries(await own.locator('input[type="hidden"]').evaluateAll(inputs =>
        inputs.map(input => [(input as HTMLInputElement).name, (input as HTMLInputElement).value])));
      return buyer.request.post('/account/xmpp', {
        form: { ...hidden, xmpp: `${handles[0]}@example.test`, xmpp_enabled: '1', xmpp_include_contents: '1', password }, maxRedirects: 0,
      });
    };
    await buyer.goto('/account');
    const preferences = buyer.getByRole('region', { name: 'XMPP notifications', exact: true });
    await expect(preferences.getByLabel('Include PGP-encrypted message contents')).toBeDisabled();
    const notAllowed = await postContents();
    expect(notAllowed.status()).toBe(403);
    expect(await notAllowed.text()).toContain('The administrator has not allowed message contents in XMPP.');

    // Receiving summaries remains available without a PGP key.
    await preferences.getByLabel('Private XMPP address').fill(`${handles[0]}@example.test`);
    await preferences.getByLabel('Receive XMPP notifications').check();
    await preferences.getByLabel('Current password', { exact: true }).fill(password);
    await submitAndLoad(buyer, preferences.getByRole('button', { name: 'Save XMPP preferences' }));
    await buyer.reload();
    await expect(preferences.getByLabel('Receive XMPP notifications')).toBeChecked();
    await expect(preferences.getByLabel('Include PGP-encrypted message contents')).not.toBeChecked();
    await expect(buyer.getByLabel('PGP public key', { exact: true })).toHaveValue('');

    // Buyer, vendor and moderator cannot grant the global contents permission.
    const beforeDenied = sql("SELECT value FROM settings WHERE key='xmpp_config'");
    for (const [role, page] of [['buyer', buyer], ['vendor', vendor], ['moderator', moderator]] as const) {
      await page.goto('/account');
      const csrf = await page.locator('form[action="/account/xmpp"] input[name="csrf"]').inputValue();
      const denied = await page.request.post('/admin/xmpp', {
        form: { csrf, password, xmpp_allow_contents: '1', xmpp_sender: 'forged@example.test', xmpp_host: '127.0.0.1', xmpp_port: '5222', xmpp_tls_mode: 'starttls' }, maxRedirects: 0,
      });
      expect(denied.status(), `${role} global content permission`).toBe(403);
      expect((await page.goto('/admin/site'))?.status()).toBe(403);
    }
    expect(sql("SELECT value FROM settings WHERE key='xmpp_config'")).toBe(beforeDenied);

    await outgoing.getByLabel('Sender XMPP address').fill('browser-content@example.test');
    await outgoing.getByLabel('Server hostname').fill('127.0.0.1');
    await outgoing.getByLabel('Server port').fill('5223');
    await outgoing.getByLabel('Connection security').selectOption('direct');
    await outgoing.getByLabel('Enable outgoing XMPP delivery').uncheck();
    await outgoing.getByLabel('Allow PGP-encrypted message contents in XMPP').check();
    await outgoing.getByLabel('Current password', { exact: true }).fill(ADMIN.password);
    await submitAndLoad(admin, outgoing.getByRole('button', { name: 'Save outgoing XMPP settings' }));
    await admin.reload();
    await expect(outgoing.getByLabel('Allow PGP-encrypted message contents in XMPP')).toBeChecked();
    await expect(outgoing.getByLabel('Enable outgoing XMPP delivery')).not.toBeChecked();
    const allowedPolicy = JSON.parse(sql("SELECT value FROM settings WHERE key='xmpp_config'"));
    expect(allowedPolicy.allow_contents).toBe(true);
    expect(allowedPolicy.content_policy_version).toBe(priorPolicy + 1);

    await buyer.goto('/account');
    await expect(preferences.getByLabel('Include PGP-encrypted message contents')).toBeDisabled();
    await expect(preferences).toContainText('Add a usable PGP encryption public key');
    const noKey = await postContents();
    expect(noKey.status()).toBe(400);
    expect(await noKey.text()).toContain('Add a usable PGP encryption public key before choosing message contents.');
    await buyer.goto(`/messages?to=${handles[1]}`);
    await expect(buyer.getByRole('button', { name: 'Send encrypted text ↗' })).toBeDisabled();

    await saveEncryptionKey(buyer, password);
    await expect(preferences.getByLabel('Include PGP-encrypted message contents')).toBeEnabled();
    await expect(preferences.getByLabel('Include PGP-encrypted message contents')).not.toBeChecked();
    await expect(buyer.locator('.key-values div', { hasText: 'PGP sign-in verification' }).locator('dd')).toHaveText('Requires a verified key');
    await preferences.getByLabel('Include PGP-encrypted message contents').check();
    await preferences.getByLabel('Current password', { exact: true }).fill(password);
    await submitAndLoad(buyer, preferences.getByRole('button', { name: 'Save XMPP preferences' }));
    await buyer.reload();
    await expect(preferences.getByLabel('Receive XMPP notifications')).toBeChecked();
    await expect(preferences.getByLabel('Include PGP-encrypted message contents')).toBeChecked();

    // Staff compose actual ciphertext addressed to this buyer. The frontend notice
    // remains a summary; the private message keeps the armored block.
    for (const staff of [admin, moderator]) {
      await staff.goto(`/messages?to=${handles[0]}`);
      await staff.getByLabel('PGP encrypted message').fill(pgpFixture('message-to-recipient.asc'));
      await submitAndLoad(staff, staff.getByRole('button', { name: 'Send encrypted text ↗' }));
    }
    await buyer.goto('/notifications');
    await expect(buyer.locator('main')).toContainText(`New message from ${ADMIN.handle}`);
    await expect(buyer.locator('main')).toContainText(`New message from ${handles[2]}`);
    await expect(buyer.locator('main')).not.toContainText('-----BEGIN PGP MESSAGE-----');
    await buyer.goto('/messages');
    await expect(buyer.locator('article.message')).toHaveCount(2);
    for (const message of await buyer.locator('article.message').all()) {
      await expect(message.locator('textarea[readonly]')).toHaveValue(/^-----BEGIN PGP MESSAGE-----/);
      await expect(message.locator('.pgp-message-status')).toHaveText('Encrypted (to recipient’s key)');
    }

    await admin.goto('/admin/site');
    await outgoing.getByLabel('Allow PGP-encrypted message contents in XMPP').uncheck();
    await outgoing.getByLabel('Current password', { exact: true }).fill(ADMIN.password);
    await submitAndLoad(admin, outgoing.getByRole('button', { name: 'Save outgoing XMPP settings' }));
    expect(JSON.parse(sql("SELECT value FROM settings WHERE key='xmpp_config'")).content_policy_version).toBe(priorPolicy + 2);
    await buyer.goto('/account');
    await expect(preferences.getByLabel('Include PGP-encrypted message contents')).toBeDisabled();
    await expect(preferences).toContainText('The administrator has not allowed message contents in XMPP.');
    expect(sql('SELECT count(*) FROM xmpp_outbox')).toBe('0');
  } finally {
    for (const page of pages) await page.context().close();
  }
});
