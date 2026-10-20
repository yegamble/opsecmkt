import { execFileSync } from 'node:child_process';
import { randomBytes } from 'node:crypto';
import { expect, test, type Page } from '@playwright/test';
import { uniqueHandle } from './db-fixtures';
import { seedPGPAccess, enrollTOTP, expectNotice, OTHER_FINGERPRINT, pgpFixture, RECIPIENT_FINGERPRINT, setRole, signedIn, submitStatus, totp } from './db-helpers';

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

// The words /messages shows for an unknown handle and for one the viewer may not write to (messageNotPermitted).
const NOT_PERMITTED = 'No account you can message has this handle. Contact an available vendor, your existing vendor or customer, or listed market staff for support.';

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
  await expect(page.locator('#pgp-visibility')).toHaveText('Shown to market staff, to every vendor or buyer you trade with and anyone you have messaged, to people who share an authorized support ticket with you, to every signed-in account if you sell, appear on the public staff list or are named as the staff contact, to both parties of every disputed or resolved order if you are a moderator or administrator, and publicly on your vendor page if you sell — with its user IDs and creation date. Use a key made only for this market.');
  const withHeaders = 'Leading note\n' + pgpFixture('recipient.pub.asc').replace('-----BEGIN PGP PUBLIC KEY BLOCK-----\n', '-----BEGIN PGP PUBLIC KEY BLOCK-----\nComment: e2e header\n');
  expect(await saveKey(page, withHeaders, password)).toBe(303);
  await expect(page).toHaveURL(/\/account$/);
  await expectNotice(page, 'Profile saved.');
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
  await expect(challenge).toHaveValue(new RegExp(`^OPSECMKT key ownership proof for ${handle}: [0-9a-f]{32}$`));
  const challengeText = await challenge.inputValue();
  const signature = page.getByLabel('Clearsigned message or armored detached signature');

  // A valid signature by the saved key over a different (stale) challenge is not proof.
  await signature.fill(pgpFixture('stale-challenge.signed.asc'));
  expect(await submitStatus(page, () => page.getByRole('button', { name: 'Verify signature' }).click())).toBe(400);
  await expect(page.getByRole('alert')).toHaveText('Not verified: the signed text is not the current challenge.');

  // A signature by another key is rejected, and so is text that is not a signature.
  await page.goto('/pgp');
  await signature.fill(pgpFixture('canary.other-key.asc'));
  expect(await submitStatus(page, () => page.getByRole('button', { name: 'Verify signature' }).click())).toBe(400);
  await expect(page.getByRole('alert')).toHaveText('Not verified: the clearsigned message does not verify with your saved key.');
  await page.goto('/pgp');
  await signature.fill('I promise this is my key.');
  expect(await submitStatus(page, () => page.getByRole('button', { name: 'Verify signature' }).click())).toBe(400);
  await expect(page.getByRole('alert')).toHaveText('Not verified: paste a clearsigned message or an armored detached signature.');

  // Nothing was verified; the open challenge is unchanged after reload.
  await page.goto('/pgp');
  await expect(page.locator('.page-head .badge')).toHaveText('Not verified');
  await expect(page.getByLabel('Text to sign')).toHaveValue(challengeText);

  // The decryption challenge shows a real OpenPGP message; a wrong answer is refused.
  await page.getByRole('radio', { name: 'Decrypt a message encrypted to your key' }).check();
  await page.getByRole('button', { name: 'Start challenge' }).click();
  await expect(page.getByRole('heading', { name: 'Decrypt this message' })).toBeVisible();
  await expect(page.getByLabel('Encrypted challenge')).toHaveValue(/-----BEGIN PGP MESSAGE-----/);
  await expect(page.getByLabel('Text to sign')).toHaveCount(0);
  await page.getByLabel('Decrypted text').fill('not-the-nonce');
  expect(await submitStatus(page, () => page.getByRole('button', { name: 'Verify decryption' }).click())).toBe(400);
  await expect(page.getByRole('alert')).toHaveText('Not verified: the decrypted text does not match the challenge.');

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
  const stranger = uniqueHandle('msgstranger');
  test.setTimeout(90_000); // five registrations and three saved keys before the message legs
  const s = await signedIn(browser, baseURL, sender, 'browser-msg-sender-123', true);
  await seedPGPAccess(s);
  const r = await signedIn(browser, baseURL, withKey, 'browser-msg-recipient-123', true);
  const o = await signedIn(browser, baseURL, otherKey, 'browser-msg-other-123', true);
  const n = await signedIn(browser, baseURL, noKey, 'browser-msg-nokey-123', true);
  const x = await signedIn(browser, baseURL, stranger, 'browser-msg-stranger-123', true);
  // A buyer starts a message only to an available vendor or eligible staff:
  // the three recipients sell; the stranger stays a buyer the sender may not write to.
  const database = process.env.E2E_DATABASE_URL!;
  execFileSync('psql', [database, '-v', 'ON_ERROR_STOP=1', '-qtAc', `UPDATE users SET role='vendor' WHERE handle IN ('${withKey}','${otherKey}','${noKey}')`]);
  // A legacy vendor missing its key is not a public message destination. The sender's existing order permits
  // contacting it, so the recipient-encryption check below can still prove that no unreadable message is stored.
  const legacyProduct = randomBytes(32).toString('hex');
  const legacyOrder = randomBytes(32).toString('hex');
  execFileSync('psql', [database, '-v', 'ON_ERROR_STOP=1', '-qtAc',
    `INSERT INTO products(id,vendor_id,title,description,category,region,kind,xmr,stock)
      SELECT '${legacyProduct}',id,'Legacy keyless recipient','Existing customer fixture','Digital','Worldwide','digital',500000000000,1
      FROM users WHERE handle='${noKey}';
      INSERT INTO orders(id,buyer_id,product_id,currency,amount,state)
      SELECT '${legacyOrder}',id,'${legacyProduct}','XMR',500000000000,'draft' FROM users WHERE handle='${sender}';`]);
  expect(await saveKey(r, pgpFixture('recipient.pub.asc'), 'browser-msg-recipient-123')).toBe(303);
  expect(await saveKey(o, pgpFixture('other.pub.asc'), 'browser-msg-other-123')).toBe(303);
  expect(await saveKey(x, pgpFixture('other.pub.asc'), 'browser-msg-stranger-123')).toBe(303);
  expect(await saveKey(s, pgpFixture('other.pub.asc'), 'browser-msg-sender-123')).toBe(303);
  await seedPGPAccess(s, pgpFixture('other.pub.asc'));

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
  await expect(s.locator('input[name="recipient"]')).toHaveValue(withKey);
  await expect(s.locator('.message-to')).toHaveText(`To: ${withKey}`);
  await expect(s.getByLabel('Recipient handle')).toHaveCount(0);
  await expect(s.locator('.contact-key .pgp-fingerprint')).toHaveText(RECIPIENT_FINGERPRINT);
  const body = s.getByLabel('PGP encrypted message');
  const send = () => s.getByRole('button', { name: 'Send encrypted text ↗' }).click();

  await body.fill('Hello, this is plaintext and must never be stored by the server.');
  expect(await submitStatus(s, send)).toBe(400);
  await expect(s.getByRole('alert')).toHaveText('Encrypt the message locally and paste the complete armored PGP message (up to 20 KB).');

  await s.goto(`/messages?to=${withKey}`);
  await body.fill('-----BEGIN PGP MESSAGE-----\n\nVGhpcyBpcyBub3QgYW4gT3BlblBHUCBwYWNrZXQgYXQgYWxs\n-----END PGP MESSAGE-----');
  expect(await submitStatus(s, send)).toBe(400);
  await expect(s.locator('body')).toContainText('This is not an OpenPGP-encrypted message');

  // An unknown handle and a buyer the sender may not write to look alike: the same words, no key, no compose form.
  for (const to of ['nobody_with_this_handle', stranger]) {
    await s.goto(`/messages?to=${to}`);
    await expect(s.getByRole('status')).toHaveText(NOT_PERMITTED);
    await expect(s.locator('.contact-key')).toHaveCount(0);
    await expect(body).toHaveCount(0);
  }

  // Nothing was stored for the refused attempts.
  await s.goto('/messages');
  await expect(s.getByRole('heading', { name: 'No messages yet' })).toBeVisible();

  // A-569: the compose page says a recipient without a usable key cannot be sent to, and a send is refused (409).
  await s.goto(`/messages?to=${noKey}`);
  await expect(s.getByRole('status').filter({ hasText: 'cannot be sent' })).toHaveText(`Messages to ${noKey} cannot be sent until they save a PGP public key that can receive encrypted messages. The server refuses a message to an account without one and stores nothing.`);
  await expect(s.locator('.contact-key')).toContainText(`${noKey} has not added a PGP public key.`);
  await body.fill(pgpFixture('message-to-recipient.asc'));
  expect(await submitStatus(s, send)).toBe(409);
  await expect(s.getByRole('alert')).toHaveText(`Message not sent: ${noKey} has not saved a PGP public key, so nothing can be encrypted to them. Nothing was stored. Ask ${noKey} to add a public key that can receive encrypted messages, then encrypt to it.`);

  // Ciphertext the recipient could not read is refused with what to do instead: the real ciphertext encrypted to the
  // recipient fixture key, sent to the account holding the other key, and a passphrase-only message.
  await s.goto(`/messages?to=${otherKey}`);
  await body.fill(pgpFixture('message-to-recipient.asc'));
  expect(await submitStatus(s, send)).toBe(400);
  await expect(s.getByRole('alert')).toContainText(`it names only other keys, so ${otherKey} could not read it. Nothing was stored. Encrypt it to ${otherKey}’s key ${OTHER_FINGERPRINT} and paste the result.`);
  await s.goto(`/messages?to=${withKey}`);
  await body.fill(pgpFixture('message-passphrase-only.asc'));
  expect(await submitStatus(s, send)).toBe(400);
  await expect(s.getByRole('alert')).toContainText(`Message not sent: it is encrypted with a passphrase only and names no recipient key, so ${withKey} could not read it without that passphrase. Nothing was stored. Encrypt it to ${withKey}’s key ${RECIPIENT_FINGERPRINT} and paste the result.`);
  await s.goto('/messages');
  await expect(s.getByRole('heading', { name: 'No messages yet' })).toBeVisible();

  // The same ciphertext sent to the account holding the recipient key is stored and badged as such.
  await s.goto(`/messages?to=${withKey}`);
  await body.fill(pgpFixture('message-to-recipient.asc'));
  expect(await submitStatus(s, send)).toBe(303);
  await expectNotice(s, 'Message sent.');
  await s.waitForLoadState();
  await s.reload();
  await expect(s.locator('article.message')).toHaveCount(1);
  await expect(s.locator('article.message', { hasText: `${sender} → ${withKey}` }).locator('.pgp-message-status')).toHaveText('Encrypted (to recipient’s key)');
  await expect(s.locator('article.message').first().locator('textarea[readonly]')).toHaveValue(/-----BEGIN PGP MESSAGE-----/);

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

  for (const p of [s, r, o, n, x]) await p.context().close();
});

test('staff find any account by handle; a buyer replies with encrypted evidence without scripts', async ({ browser, baseURL }) => {
  const { ADMIN } = await import('./db-fixtures');
  const handle = uniqueHandle('staffkey');
  const buyerHandle = uniqueHandle('evidence');
  const staff = await signedIn(browser, baseURL, handle, 'browser-staff-password-123', true);
  const buyer = await signedIn(browser, baseURL, buyerHandle, 'browser-evidence-password-123', true);
  const admin = await signedIn(browser, baseURL, ADMIN.handle, ADMIN.password, false);
  expect(await saveKey(staff, pgpFixture('recipient.pub.asc'), 'browser-staff-password-123')).toBe(303);
  // Messages go only to an account with a usable key (A-569): the buyer saves the same fixture key.
  expect(await saveKey(buyer, pgpFixture('recipient.pub.asc'), 'browser-evidence-password-123')).toBe(303);
  await setRole(admin, handle, 'moderator');
  await seedPGPAccess(staff);
  await seedPGPAccess(buyer);

  // A buyer has no handle search, and an unlisted moderator is not someone it may start a message to (A-543).
  await buyer.goto('/messages');
  await expect(buyer.getByLabel('Find a recipient')).toHaveCount(0);
  await expect(buyer.locator('#who-can-be-messaged')).toContainText('Private buyers cannot be found or contacted by handle.');
  await buyer.goto(`/messages?to=${handle}`);
  await expect(buyer.getByRole('status')).toHaveText(NOT_PERMITTED);
  await expect(buyer.locator('.contact-key')).toHaveCount(0);

  // Staff keep the handle search and may write to any account.
  await staff.goto('/messages');
  await staff.getByLabel('Find a recipient', { exact: true }).fill(buyerHandle);
  await staff.getByRole('button', { name: 'Load recipient’s public key' }).click();
  await expect(staff.getByLabel('Recipient handle', { exact: true })).toHaveValue(buyerHandle);
  await staff.getByLabel('PGP encrypted message').fill(pgpFixture('message-to-recipient.asc'));
  expect(await submitStatus(staff, () => staff.getByRole('button', { name: 'Send encrypted text ↗' }).click())).toBe(303);

  // The buyer replies from the received message: the moderator's key loads and the evidence is sent.
  await buyer.goto('/messages');
  await buyer.getByRole('link', { name: `Reply to ${handle}` }).click();
  await expect(buyer).toHaveURL(new RegExp(`/messages\\?to=${handle}$`));
  await expect(buyer.locator('input[name="recipient"]')).toHaveValue(handle);
  await expect(buyer.locator('.contact-key .pgp-fingerprint')).toHaveText(RECIPIENT_FINGERPRINT);
  await expect(buyer.locator('.contact-key')).toContainText('Ownership not verified');
  await buyer.getByLabel('PGP encrypted message').fill(pgpFixture('message-to-recipient.asc'));
  expect(await submitStatus(buyer, () => buyer.getByRole('button', { name: 'Send encrypted text ↗' }).click())).toBe(303);
  await staff.goto('/messages');
  const evidence = staff.locator('article.message', { hasText: `${buyerHandle} → ${handle}` });
  await expect(evidence.locator('textarea[readonly]')).toHaveValue(pgpFixture('message-to-recipient.asc').trim());
  await expect(evidence.locator('.pgp-message-status')).toHaveText('Encrypted (to recipient’s key)');
  await buyer.goto('/messages?to=no_such_staff_handle');
  await expect(buyer.getByRole('status')).toHaveText(NOT_PERMITTED);
  for (const page of [staff, buyer, admin]) await page.context().close();
});

test('hidden recipient IDs are sent only after the sender confirms, the ciphertext kept without scripts', async ({ browser, baseURL }) => {
  const sender = uniqueHandle('hidfrom');
  const recipient = uniqueHandle('hidto');
  const s = await signedIn(browser, baseURL, sender, 'browser-hidden-sender-123', true);
  await seedPGPAccess(s);
  const r = await signedIn(browser, baseURL, recipient, 'browser-hidden-recipient-123', true);
  expect(await saveKey(s, pgpFixture('other.pub.asc'), 'browser-hidden-sender-123')).toBe(303);
  await seedPGPAccess(s, pgpFixture('other.pub.asc'));
  // A buyer may start a message to a vendor (A-543); the vendor holds the recipient fixture key.
  execFileSync('psql', [process.env.E2E_DATABASE_URL!, '-v', 'ON_ERROR_STOP=1', '-qtAc', `UPDATE users SET role='vendor' WHERE handle='${recipient}'`]);
  expect(await saveKey(r, pgpFixture('recipient.pub.asc'), 'browser-hidden-recipient-123')).toBe(303);
  const body = s.getByLabel('PGP encrypted message');
  const send = () => s.getByRole('button', { name: 'Send encrypted text ↗' }).click();

  // The ordinary compose form asks for no confirmation.
  await s.goto(`/messages?to=${recipient}`);
  await expect(s.locator('input[name="hidden_recipients"]')).toHaveCount(0);

  // `gpg --throw-keyids` output names no key, so the server cannot check it: the compose form comes back with the
  // explanation, the recipient and its key, the ciphertext kept, and an unticked confirmation the browser requires.
  await body.fill(pgpFixture('message-to-recipient-hidden.asc'));
  expect(await submitStatus(s, send)).toBe(400);
  await expect(s.getByRole('alert')).toHaveText(`Message not sent yet. The recipient key IDs are hidden (as gpg --throw-keyids writes them), so the server cannot check that it is encrypted to ${recipient}’s key with fingerprint ${RECIPIENT_FINGERPRINT}. Nothing was stored. If you encrypted it to that key, tick the confirmation below and send again; it will be marked “recipient unknown”.`);
  await expect(s.locator('.message-to')).toHaveText(`To: ${recipient}`);
  await expect(s.locator('input[name="recipient"]')).toHaveValue(recipient);
  await expect(s.locator('.contact-key .pgp-fingerprint')).toHaveText(RECIPIENT_FINGERPRINT);
  const kept = await body.inputValue();
  expect(kept).toMatch(/^-----BEGIN PGP MESSAGE-----\n\nhE4DAAAAAAAAAAAS/);
  expect(kept.replace(/\s+/g, '')).toBe(pgpFixture('message-to-recipient-hidden.asc').replace(/\s+/g, ''));
  const confirm = s.getByRole('checkbox', { name: `I encrypted this message to ${recipient}’s key shown above, with its recipient key IDs hidden. The server cannot check that ${recipient} can read it; it will be marked “recipient unknown”.` });
  await expect(confirm).not.toBeChecked();
  await expect(confirm).toHaveAttribute('required', '');
  // Nothing was stored or notified yet.
  await r.goto('/messages');
  await expect(r.getByRole('heading', { name: 'No messages yet' })).toBeVisible();
  await r.goto('/notifications');
  await expect(r.locator('article.message', { hasText: `New message from ${sender}` })).toHaveCount(0);

  // Ticked on the returned form, the kept ciphertext is sent and marked "recipient unknown".
  await confirm.check();
  expect(await submitStatus(s, send)).toBe(303);
  await expectNotice(s, 'Message sent.');

  // Ciphertext naming the recipient key and another visible key needs no confirmation and warns about the other key.
  await s.goto(`/messages?to=${recipient}`);
  await body.fill(pgpFixture('message-to-recipient-and-other.asc'));
  expect(await submitStatus(s, send)).toBe(303);
  await expectNotice(s, 'Message sent.');
  await s.waitForLoadState();
  await s.reload();
  const sent = s.locator('article.message', { hasText: `${sender} → ${recipient}` });
  await expect(sent).toHaveCount(2);
  const unknown = sent.filter({ hasText: 'Encrypted (recipient unknown)' });
  await expect(unknown.locator('.pgp-message-status')).toHaveText('Encrypted (recipient unknown)');
  await expect(unknown.locator('textarea[readonly]')).toHaveValue(kept);
  await expect(sent.filter({ hasText: '+ 1 other visible key(s)' }).locator('.pgp-message-status .badge')).toHaveText(['Encrypted (to recipient’s key)', '+ 1 other visible key(s)']);

  // The recipient sees both, with the same statuses, and was notified of each.
  await r.goto('/messages');
  await expect(r.locator('article.message', { hasText: `${sender} → ${recipient}` })).toHaveCount(2);
  await expect(r.locator('.pgp-message-status', { hasText: 'Encrypted (recipient unknown)' })).toHaveCount(1);
  await r.goto('/notifications');
  await expect(r.locator('article.message', { hasText: `New message from ${sender}` })).toHaveCount(2);
  for (const p of [s, r]) await p.context().close();
});
