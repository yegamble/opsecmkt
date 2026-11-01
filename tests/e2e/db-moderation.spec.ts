import { execFileSync } from 'node:child_process';
import { createHash, randomBytes } from 'node:crypto';
import { expect, test } from '@playwright/test';
import { ADMIN, uniqueHandle } from './db-fixtures';
import { expectNotice, signedIn, submitAndLoad } from './db-helpers';
import { waitForStyles } from './styles';

// A-723: the private moderation queue on a real ADMIN_SOCKET. The bridge (fixtures/private-admin-server.py) stands in
// for the client-authorized Tor service: it copies bytes to the socket and adds nothing, so every request reaches the
// real private handler. PUBLIC is the same process's public listener.
const PRIVATE = 'http://127.0.0.1:18087';
const PUBLIC = 'http://127.0.0.1:18088';

// Legacy-format rows (capture version 0, the form an upgraded market still holds) let one fixture stand for each kind
// and disposition without a model or worker; the proposals are data only, and no marketplace row refers to them.
function seedProposal(kind: string, disposition: string, expires = "now()+interval '1 day'"): string {
  const databaseURL = process.env.E2E_DATABASE_URL;
  if (!databaseURL) throw new Error('The moderation fixture requires an isolated browser test database');
  const digest = () => createHash('sha256').update(randomBytes(32)).digest('hex');
  const id = digest();
  const content = digest();
  const target = randomBytes(16).toString('hex');
  const decision = JSON.stringify({ proposal: { summary: 'Fixture proposal for the browser queue.', reason_codes: ['NO_FINDING'] }, reason_codes: ['NO_FINDING'] });
  execFileSync('psql', [databaseURL, '-v', 'ON_ERROR_STOP=1', '-qtAc',
    `WITH j AS (INSERT INTO moderation_jobs(kind,target_id,actor_id,content_hash,snapshot,capture_version,completed_at)
       VALUES('${kind}','${target}','','${content}','{}'::jsonb,0,now()) RETURNING id)
     INSERT INTO moderation_suggestions(id,job_id,kind,target_id,actor_id,content_hash,policy_hash,input,decision,disposition,risk_score,confidence,reason_codes,input_hash,output_hash,prompt_version,model_id,weights_hash,expires_at,failure_class,storage_version)
     SELECT '${id}',j.id,'${kind}','${target}','','${content}','${digest()}','{}'::jsonb,'${decision}'::jsonb,'${disposition}',10,0.5,'["NO_FINDING"]'::jsonb,'${digest()}','${digest()}','moderation-browser','','',${expires},'',0 FROM j`],
  { encoding: 'utf8' });
  return id;
}

test('the private queue: overview counts, a kept filter, a confirmed review and readable items at 320 px', async ({ browser }) => {
  // Unique to this run's rows: the database is shared, so the spec filters to a kind only it writes.
  const reviewed = seedProposal('payment_flag', 'ESCALATE_ADMIN');
  const other = seedProposal('payment_flag', 'ESCALATE_ADMIN');
  const expired = seedProposal('upload', 'QUEUE_HUMAN', "now()-interval '1 hour'");
  const short = reviewed.slice(0, 8);
  const name = `Payment flag proposal ${short}`;

  const admin = await signedIn(browser, PRIVATE, ADMIN.handle, ADMIN.password, false, 320);

  // Counts only, above the checklist, with the section navigation's entry.
  await admin.goto('/admin');
  await waitForStyles(admin);
  const line = admin.locator('#moderation-queue-summary');
  await expect(line).toContainText(/\d+ unreviewed, \d+ escalations?, \d+ past deadline/);
  await expect(line).not.toContainText(short);
  const nav = admin.getByRole('navigation', { name: 'Administration sections' });
  await nav.getByRole('link', { name: 'Moderation review', exact: true }).click();
  await expect(admin).toHaveURL(/\/admin\/moderation$/);
  await expect(admin.getByRole('navigation', { name: 'Administration sections' }).getByRole('link', { name: 'Moderation review', exact: true })).toHaveAttribute('aria-current', 'page');

  // Within two tab stops of main: the skip link's target, then the queue's first link.
  await admin.goto('/admin/moderation');
  await waitForStyles(admin);
  await admin.locator('.skip-link').focus();
  await admin.keyboard.press('Enter');
  await admin.keyboard.press('Tab');
  await expect(admin.locator('#moderation-queue-section a:focus')).toHaveCount(1);
  await expect(admin.locator('details.moderation-panel').first()).not.toHaveAttribute('open', /.*/);
  await expect(admin.locator('#moderation-capacity-panel')).toBeAttached();
  await expect(admin.locator('#moderation-budget-panel')).toBeAttached();

  // Filter through the form with JavaScript off, then review: the filter comes back, confirmed, at the queue.
  await admin.getByRole('combobox', { name: 'Kind', exact: true }).selectOption({ label: 'Payment flag' });
  await admin.getByRole('combobox', { name: 'Proposed', exact: true }).selectOption({ label: 'Escalate to an administrator' });
  await submitAndLoad(admin, admin.getByRole('button', { name: 'Filter', exact: true }));
  await expect(admin).toHaveURL(/disposition=ESCALATE_ADMIN/);
  await expect(admin).toHaveURL(/kind=payment_flag/);
  const item = admin.locator('article.moderation-item', { hasText: reviewed });
  await expect(item).toHaveCount(1);
  await expect(admin.locator('article.moderation-item', { hasText: other })).toHaveCount(1);

  // Each control names its item; identically named controls would make these ambiguous.
  await expect(item.getByRole('heading', { name, exact: true })).toBeVisible();
  await expect(item.getByText('Escalate to an administrator', { exact: true })).toBeVisible();
  await expect(item.getByText('ESCALATE_ADMIN')).toHaveCount(0);
  await expect(admin.getByRole('button', { name: `Record review for ${name}`, exact: true })).toHaveCount(1);
  await expect(admin.getByRole('combobox', { name: `Review action for ${name}`, exact: true })).toHaveCount(1);
  await expect(admin.getByRole('textbox', { name: `Current password for ${name}`, exact: true })).toHaveCount(1);

  // Item boundaries are visible at 320 px, and the page does not scroll sideways.
  await expect(item).toHaveCSS('border-top-style', 'solid');
  const box = await item.boundingBox();
  expect(box).not.toBeNull();
  expect(box!.width).toBeLessThanOrEqual(320);
  expect(box!.x).toBeGreaterThanOrEqual(0);
  expect(box!.x + box!.width).toBeLessThanOrEqual(320);

  await admin.getByRole('combobox', { name: `Review action for ${name}`, exact: true }).selectOption('hold');
  await admin.getByRole('textbox', { name: `Current password for ${name}`, exact: true }).fill(ADMIN.password);
  await submitAndLoad(admin, admin.getByRole('button', { name: `Record review for ${name}`, exact: true }));
  await expect(admin).toHaveURL(/disposition=ESCALATE_ADMIN/);
  await expect(admin).toHaveURL(/kind=payment_flag/);
  await expect(admin).toHaveURL(/#moderation-queue$/);
  await expectNotice(admin, 'Review recorded; marketplace state unchanged.');
  await expect(admin.locator('#moderation-queue')).toBeInViewport();
  await expect(admin.locator('#moderation-queue-section').getByRole('status')).toHaveText('Review recorded; marketplace state unchanged.');
  await expect(admin.getByRole('combobox', { name: 'Kind', exact: true })).toHaveValue('payment_flag');
  await expect(admin.locator('article.moderation-item', { hasText: reviewed })).toHaveCount(0);
  await expect(admin.locator('article.moderation-item', { hasText: other })).toHaveCount(1);
  // One-shot: a reload confirms nothing.
  await admin.reload();
  await expect(admin.locator('#moderation-queue-section').getByRole('status')).toHaveCount(0);

  // The expired row reads as what it proposed, and that filter finds it.
  await admin.goto('/admin/moderation?state=expired&kind=upload&disposition=QUEUE_HUMAN');
  const past = admin.locator('article.moderation-item', { hasText: expired });
  await expect(past).toContainText('Human review; deadline passed, on hold');
  // AUTO_APPLY cannot match an unreviewed view, so it is not offered there.
  await expect(admin.getByRole('combobox', { name: 'Proposed', exact: true }).locator('option[value="AUTO_APPLY"]')).toHaveCount(0);

  // A filtered empty view names the filter and clears it. Nothing is ever proposed for a dispute on hold here.
  await admin.goto('/admin/moderation?state=expired&kind=dispute&disposition=HOLD');
  await expect(admin.locator('#moderation-queue-section')).toContainText('No proposals match Kind: Dispute, Proposed: Hold.');
  await admin.getByRole('link', { name: 'Clear the filter' }).click();
  await expect(admin).toHaveURL(/\/admin\/moderation\?state=expired$/);

  await admin.context().close();
});

test('the budget raise confirms in its opened panel and keeps the view', async ({ browser }) => {
  const admin = await signedIn(browser, PRIVATE, ADMIN.handle, ADMIN.password, false, 320);
  await admin.goto('/admin/moderation?state=reviewed');
  const panel = admin.locator('#moderation-budget-panel');
  await panel.locator('summary').click();
  const records = panel.getByLabel('Maximum lifetime allocations');
  const next = Number(await records.inputValue()) + 1;
  await records.fill(String(next));
  await panel.getByLabel('Current password', { exact: true }).fill(ADMIN.password);
  await submitAndLoad(admin, panel.getByRole('button', { name: 'Raise the moderation budget' }));
  await expect(admin).toHaveURL(/state=reviewed/);
  await expect(admin).toHaveURL(/#moderation-budget-panel$/);
  const raised = admin.locator('#moderation-budget-panel');
  await expect(raised.getByRole('status')).toContainText('Moderation budget raised.');
  await expect(raised.locator('details')).toHaveAttribute('open', /.*/);
  await expect(raised).toBeInViewport();
  await expect(raised.getByLabel('Maximum lifetime allocations')).toHaveValue(String(next));
  await admin.context().close();
});

test('the public listener and a buyer get nothing from the private queue', async ({ browser, request }) => {
  // Caller-supplied Host and forwarding headers select nothing: the public listener refuses even for a signed-in
  // administrator's cookie, shown here unauthenticated and then with one.
  for (const headers of [{}, { Host: 'private-admin.invalid', 'X-Forwarded-Host': 'private-admin.invalid', 'X-Forwarded-For': '127.0.0.1' }]) {
    for (const path of ['/admin', '/admin/moderation']) {
      expect((await request.get(`${PUBLIC}${path}`, { headers, maxRedirects: 0 })).status(), `${path} ${JSON.stringify(headers)}`).toBe(404);
    }
  }
  const admin = await signedIn(browser, PRIVATE, ADMIN.handle, ADMIN.password, false);
  expect((await admin.goto(`${PUBLIC}/admin/moderation`))?.status()).toBe(404);
  expect((await admin.goto(`${PUBLIC}/admin`))?.status()).toBe(404);
  await admin.context().close();

  // A buyer on the private connection: refused, with no count or link (TestOverviewCountsUnreviewedProposals covers a moderator).
  const buyer = await signedIn(browser, PRIVATE, uniqueHandle('modqueue'), 'browser-modqueue-password-123', true);
  expect((await buyer.goto('/admin/moderation'))?.status()).toBe(403);
  await expect(buyer.locator('body')).not.toContainText('unreviewed');
  expect((await buyer.goto('/admin'))?.status()).toBe(403);
  await expect(buyer.locator('body')).not.toContainText('unreviewed');
  await buyer.context().close();
});
