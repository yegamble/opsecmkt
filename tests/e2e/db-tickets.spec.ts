import { execFileSync } from 'node:child_process';
import { expect, test, type Page } from '@playwright/test';
import { ADMIN, uniqueHandle } from './db-fixtures';
import { pgpFixture, saveEncryptionKey, setRole, signedIn, submitAndLoad } from './db-helpers';
import { waitForStyles } from './styles';

test.use({ actionTimeout: 15_000 });

// Ticket bodies are encrypted offline to BOTH participants. The committed
// two-recipient ciphertext names the opener's "other" key and staff's key.
test('encrypted support tickets keep lifecycle notices private after administrator escalation', async ({ browser, baseURL }) => {
  test.setTimeout(180_000);
  const sql = (query: string) => execFileSync('psql', [process.env.E2E_DATABASE_URL!, '-v', 'ON_ERROR_STOP=1', '-qtAc', query], { encoding: 'utf8' }).trim();
  const openerHandle = uniqueHandle('ticket_opener');
  const modHandle = uniqueHandle('ticket_mod');
  const password = 'browser-encrypted-ticket-password-123';
  const ciphertext = pgpFixture('message-to-recipient-and-other.asc');
  const pages: Page[] = [];
  const oldPublic = sql("SELECT value FROM settings WHERE key='staff_list_public'");
  try {
    const admin = await signedIn(browser, baseURL, ADMIN.handle, ADMIN.password, false, 320);
    const opener = await signedIn(browser, baseURL, openerHandle, password, true, 320);
    const moderator = await signedIn(browser, baseURL, modHandle, password, true, 320);
    pages.push(admin, opener, moderator);
    await setRole(admin, modHandle, 'moderator');
    await saveEncryptionKey(moderator, password, pgpFixture('recipient.pub.asc'));
    await opener.goto('/tickets');
    await expect(opener.getByRole('region', { name: 'Open an encrypted ticket' })).toContainText('Add a usable PGP encryption public key');
    await saveEncryptionKey(opener, password, pgpFixture('other.pub.asc'));
    const modID = sql(`SELECT id FROM users WHERE handle='${modHandle}'`);
    const adminID = sql(`SELECT id FROM users WHERE handle='${ADMIN.handle}'`);
    // Fixture visibility is explicit; the workflow still selects the visible
    // staff recipient and submits all ticket forms through the actual browser.
    sql("INSERT INTO settings(key,value) VALUES('staff_list_public','true') ON CONFLICT(key) DO UPDATE SET value='true'");
    sql(`UPDATE users SET staff_hidden=false WHERE id='${modID}'`);
    await opener.goto('/tickets');
    const choose = opener.getByRole('form', { name: 'Choose support recipient', exact: true });
    await choose.getByLabel('Support recipient').selectOption(modID);
    await submitAndLoad(opener, choose.getByRole('button', { name: 'Load support keys' }));
    await expect(opener).toHaveURL(new RegExp(`/tickets\\?staff=${modID}$`));
    const create = opener.getByRole('region', { name: 'Open an encrypted ticket' });
    await expect(create).toContainText('Encrypt to both ticket participants');
    await expect(create.locator('.contact-key')).toHaveCount(2);
    await create.getByLabel('PGP encrypted ticket', { exact: true }).fill(ciphertext);
    await create.getByRole('button', { name: 'Open encrypted ticket' }).focus();
    await Promise.all([opener.waitForEvent('load'), opener.keyboard.press('Enter')]);
    await expect(opener).toHaveURL(/\/ticket\?id=[0-9a-f]+$/);
    const ticketID = new URL(opener.url()).searchParams.get('id')!;
    const ticketURL = `/ticket?id=${ticketID}`;
    await expect(opener.locator('.page-head .badge')).toHaveText('Open');
    const conversation = (page: Page) => page.getByRole('region', { name: 'Encrypted conversation', exact: true });
    await expect(conversation(opener).getByRole('textbox')).toHaveCount(1);
    await expect(conversation(opener).getByRole('textbox')).toHaveValue(/^-----BEGIN PGP MESSAGE-----/);
    await expect(opener.getByRole('region', { name: 'Staff ticket controls' })).toHaveCount(0);
    await waitForStyles(opener);
    expect(await opener.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(320);
    await expect(opener.locator('script')).toHaveCount(0);

    await moderator.goto('/notifications');
    await expect(moderator.locator(`a[href="${ticketURL}"]`)).toHaveCount(1);
    await moderator.locator(`a[href="${ticketURL}"]`).click();
    const reply = moderator.getByRole('region', { name: 'Reply with encrypted text' });
    await reply.getByLabel('PGP encrypted reply').fill(ciphertext);
    await submitAndLoad(moderator, reply.getByRole('button', { name: 'Send encrypted ticket reply' }));
    await expect(conversation(moderator).getByRole('textbox')).toHaveCount(2);
    const staleReply = Object.fromEntries(await moderator.locator('form[action="/tickets/reply"] input[type="hidden"]').evaluateAll(inputs =>
      inputs.map(input => [(input as HTMLInputElement).name, (input as HTMLInputElement).value])));
    const assign = moderator.getByRole('form', { name: 'Assign ticket', exact: true });
    await assign.getByLabel('New assignee').selectOption(adminID);
    await submitAndLoad(moderator, assign.getByRole('button', { name: 'Assign ticket', exact: true }));
    const assignment = moderator.getByRole('region', { name: 'Ticket details' }).locator('.key-values > div', { has: moderator.getByText('Assigned to', { exact: true }) });
    await expect(assignment).toContainText(ADMIN.handle);

    await admin.goto(ticketURL);
    const escalate = admin.getByRole('form', { name: 'Escalate ticket to administrators', exact: true });
    await escalate.getByLabel('Administrator assignee').selectOption(adminID);
    await submitAndLoad(admin, escalate.getByRole('button', { name: 'Escalate to administrators' }));
    await expect(admin.getByRole('region', { name: 'Ticket details' })).toContainText('Administrators only');
    await expect(admin.getByRole('region', { name: 'Ticket history' })).toContainText(/escalat/i);
    await opener.goto('/notifications');
    const escalationID = sql(`SELECT id FROM notifications WHERE user_id=(SELECT id FROM users WHERE handle='${openerHandle}') AND body ILIKE '%escalat%' ORDER BY created DESC,id DESC LIMIT 1`);
    expect(escalationID).not.toBe('');
    const escalationNote = opener.locator('article.message').filter({ has: opener.locator(`input[name="id"][value="${escalationID}"]`) });
    await expect(escalationNote.locator(`a[href="${ticketURL}"]`)).toHaveCount(1);
    await expect(escalationNote).not.toContainText('-----BEGIN PGP MESSAGE-----');

    // A former moderator participant loses the escalated thread and cannot
    // replay a real form obtained while they still had access.
    expect([403, 404]).toContain((await moderator.goto(ticketURL))!.status());
    const denied = await moderator.request.post('/tickets/reply', { form: { ...staleReply, body: ciphertext }, maxRedirects: 0 });
    expect([403, 404]).toContain(denied.status());
    expect(sql(`SELECT count(*) FROM ticket_messages WHERE ticket_id='${ticketID}'`)).toBe('2');
    await moderator.goto('/tickets');
    await expect(moderator.locator(`a[href="${ticketURL}"]`)).toHaveCount(0);

    await opener.goto(ticketURL);
    await expect(conversation(opener).getByRole('textbox')).toHaveCount(2);
    const close = opener.getByRole('region', { name: 'Close this ticket', exact: true });
    await submitAndLoad(opener, close.getByRole('button', { name: 'Close ticket', exact: true }));
    await expect(opener.locator('.page-head .badge')).toHaveText('Closed');
    await expect(opener.getByRole('region', { name: 'Reply with encrypted text' })).toContainText('Replies are unavailable');
    expect(sql(`SELECT bool_and(encrypted AND body LIKE '-----BEGIN PGP MESSAGE-----%') FROM ticket_messages WHERE ticket_id='${ticketID}'`)).toBe('t');
    expect(sql(`SELECT count(*) FROM ticket_participants WHERE ticket_id='${ticketID}' AND user_id='${modID}' AND ever_assigned AND ever_messaged`)).toBe('1');
    for (const page of [opener, moderator, admin]) {
      const handle = page === opener ? openerHandle : page === moderator ? modHandle : ADMIN.handle;
      const noticeID = sql(`SELECT id FROM notifications WHERE user_id=(SELECT id FROM users WHERE handle='${handle}') AND body ILIKE '%closed%' ORDER BY created DESC,id DESC LIMIT 1`);
      expect(noticeID, `${handle} receives a closure notice`).not.toBe('');
      await page.goto('/notifications');
      const notice = page.locator('article.message').filter({ has: page.locator(`input[name="id"][value="${noticeID}"]`) });
      await expect(notice).toContainText(/closed/i);
      await expect(notice).not.toContainText('-----BEGIN PGP MESSAGE-----');
      await expect(notice.locator(`a[href="${ticketURL}"]`)).toHaveCount(page === moderator ? 0 : 1);
    }
    expect([403, 404]).toContain((await moderator.goto(ticketURL))!.status());
  } finally {
    if (oldPublic === '') sql("DELETE FROM settings WHERE key='staff_list_public'");
    else sql(`UPDATE settings SET value='${oldPublic}' WHERE key='staff_list_public'`);
    for (const page of pages) await page.context().close();
  }
});
