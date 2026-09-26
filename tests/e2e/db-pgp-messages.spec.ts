import { expect, test, type Page } from '@playwright/test';
import { uniqueHandle } from './db-fixtures';
import { enrollTOTP, pgpFixture, RECIPIENT_FINGERPRINT, setRole, signedIn, submitStatus, totp } from './db-helpers';

// PGP identity, encrypted messages and message notifications against a real database, JavaScript disabled.
// Each test registers its own accounts; keys and messages come from committed fixtures (see db-helpers.ts).

// Saves a key on /account, confirming with password when given (adding, changing or removing a key needs it: A-152).
async function saveKey(page: Page, armored: string, password?: string): Promise<number> {
  await page.goto('/account');
  const form = page.locator('form', { has: page.getByRole('button', { name: 'Save profile' }) });
  await form.getByLabel('PGP public key').fill(armored);
  if (password !== undefined) await form.getByLabel('Current password').fill(password);
  return submitStatus(page, () => form.getByRole('button', { name: 'Save profile' }).click());
}

const pgpRow = (page: Page) => page.locator('.key-values div', { hasText: 'PGP sign-in verification' }).locator('dd');

test('save a PGP key, see its fingerprint unverified, and fail ownership proofs that do not match', async ({ browser, baseURL }) => {
  const handle = uniqueHandle('pgpkey');
  const password = 'browser-pgp-password-123';
  const page = await signedIn(browser, baseURL, handle, password, true);

  // A session alone cannot add the first key (A-152): buyers would encrypt addresses to it.
  expect(await saveKey(page, pgpFixture('recipient.pub.asc'))).toBe(400);
  await expect(page.locator('body')).toContainText('Enter your current password');

  // A malformed key is refused with the reason and nothing is stored.
  expect(await saveKey(page, '-----BEGIN PGP PUBLIC KEY BLOCK-----\n\nbm90IGEga2V5\n-----END PGP PUBLIC KEY BLOCK-----', password)).toBe(400);
  await expect(page.locator('body')).toContainText('PGP key rejected:');
  await page.goto('/account');
  await expect(pgpRow(page)).toHaveText('No key saved');
  await expect(page.locator('.pgp-summary')).toHaveCount(0);

  // The note above the key field says who can see it. Text around the block and armor headers are not kept.
  await expect(page.locator('#pgp-visibility')).toHaveText('Shown to every signed-in account that looks up your handle, to every vendor or buyer you trade with, and publicly on your vendor page if you sell — with its user IDs and creation date. Use a key made only for this market.');
  const withHeaders = 'Leading note\n' + pgpFixture('recipient.pub.asc').replace('-----BEGIN PGP PUBLIC KEY BLOCK-----\n', '-----BEGIN PGP PUBLIC KEY BLOCK-----\nComment: e2e header\n');
  expect(await saveKey(page, withHeaders, password)).toBe(303);
  await expect(page).toHaveURL(/\/account\?saved=1$/);
  await expect(page.getByRole('status')).toHaveText('Changes saved.');
  await page.waitForLoadState();
  await page.reload();
  await expect(page.getByLabel('PGP public key')).toHaveValue(/^-----BEGIN PGP PUBLIC KEY BLOCK-----\n\n/);
  await expect(page.getByLabel('PGP public key')).not.toHaveValue(/Leading note|Comment|e2e header/);
  await expect(page.locator('.pgp-summary .pgp-fingerprint')).toHaveText(RECIPIENT_FINGERPRINT);
  await expect(page.locator('.pgp-summary .pgp-user-ids li')).toHaveText(['E2E Fixture Recipient (browser tests only) <recipient@e2e.invalid>']);
  // Saving the form again submits the same key: it is not recorded as a key change.
  expect(await submitStatus(page, () => page.getByRole('button', { name: 'Save profile' }).click())).toBe(303);
  await expect(page.getByRole('region', { name: 'Recent activity' }).getByText(/Updated PGP key/)).toHaveCount(1);
  await expect(page.locator('.pgp-summary .badge')).toHaveText('Not verified');
  await expect(pgpRow(page)).toHaveText('Requires a verified key');
  await expect(page.getByRole('region', { name: 'Recent activity' })).toContainText(`Updated PGP key (fingerprint ${RECIPIENT_FINGERPRINT.replace(/ /g, '')}; ownership unverified) (confirmed with password)`);
  // The owner is told of the key change once, without its fingerprint (A-152).
  await page.goto('/notifications');
  await expect(page.locator('main').getByText('A PGP public key was added to your account')).toHaveCount(1);
  await expect(page.locator('main')).not.toContainText(RECIPIENT_FINGERPRINT.replace(/ /g, ''));
  await page.goto('/account');

  await page.getByRole('link', { name: 'Prove key ownership' }).click();
  await expect(page).toHaveURL(/\/pgp$/);
  await expect(page.locator('.page-head .badge')).toHaveText('Not verified');
  await expect(page.locator('.pgp-fingerprint')).toHaveText(RECIPIENT_FINGERPRINT);
  await expect(page.getByText('Unavailable until ownership of this key is verified.')).toBeVisible();
  await expect(page.getByRole('button', { name: 'Turn on PGP sign-in verification' })).toHaveCount(0);

  // The signing challenge renders the exact text to sign for this account.
  await expect(page.getByRole('radio', { name: 'Sign a text with your private key' })).toBeChecked();
  await page.getByRole('button', { name: 'Start challenge' }).click();
  const challenge = page.getByLabel('Text to sign');
  await expect(challenge).toHaveText(new RegExp(`^OPSECMKT key ownership proof for ${handle}: [0-9a-f]{32}$`));
  const challengeText = await challenge.innerText();
  const signature = page.getByLabel('Clearsigned message or armored detached signature');

  // A valid signature by the saved key over a different (stale) challenge is not proof.
  await signature.fill(pgpFixture('stale-challenge.signed.asc'));
  expect(await submitStatus(page, () => page.getByRole('button', { name: 'Verify signature' }).click())).toBe(400);
  await expect(page.locator('body')).toHaveText('Not verified: the signed text is not the current challenge.');

  // A signature by another key is rejected, and so is text that is not a signature.
  await page.goto('/pgp');
  await signature.fill(pgpFixture('canary.other-key.asc'));
  expect(await submitStatus(page, () => page.getByRole('button', { name: 'Verify signature' }).click())).toBe(400);
  await expect(page.locator('body')).toHaveText('Not verified: the clearsigned message does not verify with your saved key.');
  await page.goto('/pgp');
  await signature.fill('I promise this is my key.');
  expect(await submitStatus(page, () => page.getByRole('button', { name: 'Verify signature' }).click())).toBe(400);
  await expect(page.locator('body')).toHaveText('Not verified: paste a clearsigned message or an armored detached signature.');

  // Nothing was verified; the open challenge is unchanged after reload.
  await page.goto('/pgp');
  await expect(page.locator('.page-head .badge')).toHaveText('Not verified');
  await expect(page.getByLabel('Text to sign')).toHaveText(challengeText);

  // The decryption challenge shows a real OpenPGP message; a wrong answer is refused.
  await page.getByRole('radio', { name: 'Decrypt a message encrypted to your key' }).check();
  await page.getByRole('button', { name: 'Start challenge' }).click();
  await expect(page.getByRole('heading', { name: 'Decrypt this message' })).toBeVisible();
  await expect(page.getByLabel('Encrypted challenge')).toContainText('-----BEGIN PGP MESSAGE-----');
  await expect(page.getByLabel('Text to sign')).toHaveCount(0);
  await page.getByLabel('Decrypted text').fill('not-the-nonce');
  expect(await submitStatus(page, () => page.getByRole('button', { name: 'Verify decryption' }).click())).toBe(400);
  await expect(page.locator('body')).toHaveText('Not verified: the decrypted text does not match the challenge.');

  await page.goto('/account');
  await expect(page.locator('.pgp-summary .badge')).toHaveText('Not verified');
  await expect(page.getByRole('region', { name: 'Recent activity' })).not.toContainText('Verified PGP key ownership');
  await page.context().close();
});

test('with TOTP on, saving a PGP key needs the password and an authenticator code', async ({ browser, baseURL }) => {
  const handle = uniqueHandle('pgptotp');
  const password = 'browser-pgp-totp-password-123';
  const page = await signedIn(browser, baseURL, handle, password, true);
  const secret = await enrollTOTP(page, password);
  const step = Math.floor(Date.now() / 30_000);

  // A session alone cannot swap in a key that could later become a second sign-in factor.
  expect(await saveKey(page, pgpFixture('recipient.pub.asc'))).toBe(400);
  await expect(page.locator('body')).toContainText('Enter your current password');
  await page.goto('/account');
  await expect(pgpRow(page)).toHaveText('No key saved');

  const form = page.locator('form', { has: page.getByRole('button', { name: 'Save profile' }) });
  await form.getByLabel('PGP public key').fill(pgpFixture('recipient.pub.asc'));
  await form.getByLabel('Current password').fill(password);
  await form.getByLabel('Authenticator code').fill(totp(secret, step + 1));
  expect(await submitStatus(page, () => form.getByRole('button', { name: 'Save profile' }).click())).toBe(303);
  await expect(page.getByRole('region', { name: 'Recent activity' })).toContainText('ownership unverified) (confirmed with password and authenticator code)');
  await page.context().close();
});

test('only OpenPGP-encrypted messages are accepted; status badges and the unread count follow', async ({ browser, baseURL }) => {
  const sender = uniqueHandle('msgfrom');
  const withKey = uniqueHandle('msgkey');
  const otherKey = uniqueHandle('msgother');
  const noKey = uniqueHandle('msgnokey');
  const s = await signedIn(browser, baseURL, sender, 'browser-msg-sender-123', true);
  const r = await signedIn(browser, baseURL, withKey, 'browser-msg-recipient-123', true);
  const o = await signedIn(browser, baseURL, otherKey, 'browser-msg-other-123', true);
  const n = await signedIn(browser, baseURL, noKey, 'browser-msg-nokey-123', true);
  expect(await saveKey(r, pgpFixture('recipient.pub.asc'), 'browser-msg-recipient-123')).toBe(303);
  expect(await saveKey(o, pgpFixture('other.pub.asc'), 'browser-msg-other-123')).toBe(303);

  // Adding the key notified the recipient (A-152); once that is read the navigation shows no unread count.
  const rNav = r.getByRole('navigation', { name: 'Main navigation' });
  await r.goto('/notifications');
  const keyNote = r.locator('article.message', { hasText: 'A PGP public key was added to your account' });
  await expect(keyNote.locator('.message-meta span')).toHaveText('Unread');
  await keyNote.getByRole('button', { name: 'Mark as read' }).click();
  await expect(r).toHaveURL(/\/notifications$/);
  await expect(rNav.getByRole('link', { name: 'Notifications', exact: true })).toBeVisible();

  // The compose form names the recipient and shows the key it must be encrypted to.
  await s.goto(`/messages?to=${withKey}`);
  await expect(s.getByLabel('Recipient handle')).toHaveValue(withKey);
  await expect(s.locator('.contact-key .pgp-fingerprint')).toHaveText(RECIPIENT_FINGERPRINT);
  const body = s.getByLabel('PGP encrypted message');
  const send = () => s.getByRole('button', { name: 'Send encrypted text ↗' }).click();

  await body.fill('Hello, this is plaintext and must never be stored by the server.');
  expect(await submitStatus(s, send)).toBe(400);
  await expect(s.locator('body')).toHaveText('Encrypt the message locally and paste the complete armored PGP message (up to 20 KB).');

  await s.goto(`/messages?to=${withKey}`);
  await body.fill('-----BEGIN PGP MESSAGE-----\n\nVGhpcyBpcyBub3QgYW4gT3BlblBHUCBwYWNrZXQgYXQgYWxs\n-----END PGP MESSAGE-----');
  expect(await submitStatus(s, send)).toBe(400);
  await expect(s.locator('body')).toContainText('This is not an OpenPGP-encrypted message');

  await s.goto('/messages?to=nobody_with_this_handle');
  await body.fill(pgpFixture('message-to-recipient.asc'));
  expect(await submitStatus(s, send)).toBe(400);
  await expect(s.locator('body')).toHaveText('Recipient not found');

  // Nothing was stored for the refused attempts.
  await s.goto('/messages');
  await expect(s.getByRole('heading', { name: 'No messages yet' })).toBeVisible();

  // The same real ciphertext (encrypted to the recipient fixture key) sent to three recipients.
  for (const to of [withKey, otherKey, noKey]) {
    await s.goto(`/messages?to=${to}`);
    await body.fill(pgpFixture('message-to-recipient.asc'));
    expect(await submitStatus(s, send)).toBe(303);
    await expect(s).toHaveURL(/\/messages\?saved=1$/);
  }
  await s.waitForLoadState();
  await s.reload();
  const sent = (to: string) => s.locator('article.message', { hasText: `${sender} → ${to}` }).locator('.pgp-message-status .badge');
  await expect(sent(withKey)).toHaveText('Encrypted (to recipient’s key)');
  await expect(sent(otherKey)).toHaveText('Encrypted (NOT to recipient’s key)');
  await expect(sent(noKey)).toHaveText('Encrypted (recipient unknown)');
  await expect(s.locator('article.message').first().locator('pre')).toContainText('-----BEGIN PGP MESSAGE-----');

  // The recipient sees the message with the same status and one unread notification in the navigation.
  await r.goto('/messages');
  const received = r.locator('article.message', { hasText: `${sender} → ${withKey}` });
  await expect(received.locator('.badge')).toHaveText('Encrypted (to recipient’s key)');
  const unread = rNav.getByRole('link', { name: 'Notifications (1 unread)' });
  await expect(unread).toBeVisible();
  await unread.click();
  const note = r.locator('article.message', { hasText: `New message from ${sender}` });
  await expect(note.locator('.message-meta span')).toHaveText('Unread');
  await note.getByRole('button', { name: 'Mark as read' }).click();
  await expect(r).toHaveURL(/\/notifications$/);
  await expect(rNav.getByRole('link', { name: 'Notifications', exact: true })).toBeVisible();
  await r.waitForLoadState();
  await r.reload();
  await expect(note.locator('.message-meta span')).toHaveText('Read');
  await expect(note.getByRole('button', { name: 'Mark as read' })).toHaveCount(0);
  await expect(rNav.locator('.nav-count')).toHaveCount(0);

  for (const p of [s, r, o, n]) await p.context().close();
});

test('find a moderator key and exchange encrypted dispute evidence without scripts', async ({ browser, baseURL }) => {
  const { ADMIN } = await import('./db-fixtures');
  const handle = uniqueHandle('staffkey');
  const staff = await signedIn(browser, baseURL, handle, 'browser-staff-password-123', true);
  const buyer = await signedIn(browser, baseURL, uniqueHandle('evidence'), 'browser-evidence-password-123', true);
  const admin = await signedIn(browser, baseURL, ADMIN.handle, ADMIN.password, false);
  expect(await saveKey(staff, pgpFixture('recipient.pub.asc'), 'browser-staff-password-123')).toBe(303);
  await setRole(admin, handle, 'moderator');
  await buyer.goto('/messages');
  await buyer.getByLabel('Find a recipient', { exact: true }).fill(handle);
  await buyer.getByRole('button', { name: 'Load recipient’s public key' }).click();
  await expect(buyer.getByLabel('Recipient handle', { exact: true })).toHaveValue(handle);
  await expect(buyer.locator('.contact-key .pgp-fingerprint')).toHaveText(RECIPIENT_FINGERPRINT);
  await expect(buyer.locator('.contact-key')).toContainText('Ownership not verified');
  await buyer.getByLabel('PGP encrypted message').fill(pgpFixture('message-to-recipient.asc'));
  expect(await submitStatus(buyer, () => buyer.getByRole('button', { name: 'Send encrypted text ↗' }).click())).toBe(303);
  await staff.goto('/messages');
  await expect(staff.locator('article.message pre')).toHaveText(pgpFixture('message-to-recipient.asc').trim());
  await expect(staff.locator('.pgp-message-status')).toHaveText('Encrypted (to recipient’s key)');
  await buyer.goto('/messages');
  await buyer.getByLabel('Find a recipient', { exact: true }).fill('no_such_staff_handle');
  await buyer.getByRole('button', { name: 'Load recipient’s public key' }).click();
  await expect(buyer.getByRole('status')).toContainText('No other account found');
  for (const page of [staff, buyer, admin]) await page.context().close();
});
