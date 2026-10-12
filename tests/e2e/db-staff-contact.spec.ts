import { execFileSync } from 'node:child_process';
import { expect, test, type Page } from '@playwright/test';
import { uniqueHandle } from './db-fixtures';
import { expectNotice, pgpFixture, signedIn, submitAndLoad, submitStatus } from './db-helpers';
import { waitForStyles } from './styles';

// A-119: an administrator saves the "Staff contact" on /admin with their password; /canary, the suspended sign-in
// refusal and a notification that tells its reader to contact the staff show it as text (a hostile value stays
// text), at 320 px without horizontal scrolling; a moderator's handle whose key can receive encrypted messages
// also links a signed-in reader to /messages. A fresh account is made administrator for this test only, so the
// shared administrator's password-confirmation budget is untouched; the contact is cleared again at the end.

async function noOverflow(page: Page) {
  await expect(page.locator('script')).toHaveCount(0);
  await waitForStyles(page);
  expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(page.viewportSize()!.width);
}

test('administrator publishes a staff contact that every promised page shows as text', async ({ browser, baseURL }) => {
  const database = process.env.E2E_DATABASE_URL!;
  const sql = (query: string) => execFileSync('psql', [database, '-v', 'ON_ERROR_STOP=1', '-qtAc', query], { encoding: 'utf8' }).trim();
  const password = 'browser-staff-contact-password-123';
  const adminHandle = uniqueHandle('contact_admin');
  const modHandle = uniqueHandle('contact_mod');
  const buyerHandle = uniqueHandle('contact_buyer');
  const suspectHandle = uniqueHandle('contact_suspect');
  // 200 characters (the limit) with no space to break at, and markup that must stay text.
  const hostile = '<b>staff</b>@' + 'q'.repeat(200 - '<b>staff</b>@'.length - '.onion'.length) + '.onion';
  expect(hostile.length).toBe(200);

  const admin = await signedIn(browser, baseURL, adminHandle, password, true, 320);
  const mod = await signedIn(browser, baseURL, modHandle, password, true);
  const buyer = await signedIn(browser, baseURL, buyerHandle, password, true, 320);
  const suspect = await signedIn(browser, baseURL, suspectHandle, password, true);
  const visitorContext = await browser.newContext({ baseURL, javaScriptEnabled: false, viewport: { width: 320, height: 900 } });
  const visitor = await visitorContext.newPage();
  const panel = admin.getByRole('region', { name: 'Staff contact' });
  const saveContact = async (value: string) => {
    await admin.goto('/admin');
    await panel.getByLabel('Staff contact').fill(value);
    await panel.getByLabel('Current password').fill(password);
    return submitStatus(admin, () => panel.getByRole('button', { name: 'Save staff contact' }).click());
  };
  try {
    sql(`UPDATE users SET role='admin' WHERE handle='${adminHandle}';
      UPDATE users SET role='moderator' WHERE handle='${modHandle}';
      UPDATE users SET suspended_at=now() WHERE handle='${suspectHandle}';
      INSERT INTO notifications(id,user_id,body) SELECT md5(random()::text)||md5(random()::text), id,
        'Your BTC payout address was changed. If you did not make this change, someone else may know your password: change it and contact the market staff.'
        FROM users WHERE handle='${buyerHandle}';`);

    // The moderator saves a key that can receive encrypted messages.
    await mod.goto('/account');
    const profile = mod.locator('form', { has: mod.getByRole('button', { name: 'Save profile' }) });
    await profile.getByLabel('PGP public key', { exact: true }).fill(pgpFixture('recipient.pub.asc'));
    await profile.getByLabel('Current password', { exact: true }).fill(password);
    await submitAndLoad(mod, profile.getByRole('button', { name: 'Save profile' }));
    await expect(mod).toHaveURL(/\/account$/);
    await expectNotice(mod, 'Profile saved.');

    // Unset: the canary points to the operator.
    await visitor.goto('/canary');
    const contact = visitor.getByRole('region', { name: 'Staff contact' }).locator('.staff-contact');
    await expect(contact).toHaveText('Ask the operator of this market.');

    // The administrator's form needs the password; the hostile value is saved and audited as text.
    await admin.goto('/admin');
    await expect(panel.getByLabel('Staff contact')).toHaveAttribute('maxlength', '200');
    await noOverflow(admin);
    expect(await saveContact(hostile)).toBe(303);
    await expect(admin).toHaveURL(/\/admin#staff-contact$/);
    await expect(panel.getByRole('status')).toHaveText('Staff contact saved.');
    await admin.waitForLoadState();
    await admin.reload();
    await expect(panel.getByLabel('Staff contact')).toHaveValue(hostile);
    await expect(admin.locator('table').last().locator('tbody tr').filter({ hasText: `Staff contact changed from (not set) to “${hostile}” (confirmed with password)` })).toHaveCount(1);
    await noOverflow(admin);

    await visitor.goto('/canary');
    await expect(contact).toHaveText(`Contact the market staff: ${hostile}.`);
    await expect(contact.locator('b, a')).toHaveCount(0);
    await noOverflow(visitor);
    await buyer.goto('/notifications');
    const notice = buyer.locator('article', { hasText: 'Your BTC payout address was changed' });
    await expect(notice.locator('.staff-contact')).toHaveText(`Contact the market staff: ${hostile}.`);
    await expect(notice.locator('.staff-contact b')).toHaveCount(0);
    await noOverflow(buyer);

    // The suspended account's refusal names the contact (escaped text in the sign-in form's alert).
    await suspect.goto('/login');
    await suspect.getByLabel('Handle', { exact: true }).fill(suspectHandle);
    await suspect.getByLabel('Password', { exact: true }).fill(password);
    expect(await submitStatus(suspect, () => suspect.getByRole('button', { name: 'Sign in' }).click())).toBe(403);
    await expect(suspect.getByRole('alert')).toHaveText(`This account is suspended. Contact the market staff: ${hostile}.`);

    // A moderator's handle with a usable key links a signed-in reader to Messages, prefilled; a visitor gets text.
    expect(await saveContact(modHandle)).toBe(303);
    await visitor.goto('/canary');
    await expect(contact).toHaveText(`Contact the market staff: ${modHandle}.`);
    await buyer.goto('/canary');
    const buyerContact = buyer.getByRole('region', { name: 'Staff contact' }).locator('.staff-contact');
    await expect(buyerContact).toHaveText(`Contact the market staff: ${modHandle}. Send ${modHandle} an encrypted message.`);
    await noOverflow(buyer);
    await submitAndLoad(buyer, buyerContact.getByRole('link', { name: `Send ${modHandle} an encrypted message` }));
    await expect(buyer).toHaveURL(new RegExp(`/messages\\?to=${modHandle}$`));
    await expect(buyer.getByLabel('Recipient handle')).toHaveValue(modHandle);
    await buyer.goto('/notifications');
    await expect(notice.locator('.staff-contact a')).toHaveAttribute('href', `/messages?to=${modHandle}`);
  } finally {
    // Cleared again so later database specs see the unset state (409: it was never set, after an earlier failure).
    expect([303, 409]).toContain(await saveContact(''));
    await visitor.goto('/canary');
    await expect(visitor.getByRole('region', { name: 'Staff contact' }).locator('.staff-contact')).toHaveText('Ask the operator of this market.');
    for (const page of [admin, mod, buyer, suspect, visitor]) {
      await page.context().close();
    }
  }
});
