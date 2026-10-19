import { expect, test } from '@playwright/test';

// A-553: the System panel's own-build row, observed Monero versions and Health column, and the Payments panel's Monero
// escrow watcher health. ?status=sample is a labelled healthy sample and ?status=attention a failing watcher pass, an
// unreachable daemon, wallet creation stopped and no versions worker. Layout and copy only: the observations, the
// shared database row and its restart behaviour are proven against PostgreSQL and simulated Monero RPC by
// TestDependencyStatusObservesMonero and TestXMREscrowAdminShowsPassFailures, not here.
for (const width of [320, 1280]) {
  test(`status panels at ${width} px`, async ({ page }) => {
    await page.setViewportSize({ width, height: 900 });
    await page.goto('/admin/system?status=sample');
    const panel = page.locator('#dependency-status');
    const table = page.locator('#dependency-status table');
    await expect(table.getByRole('columnheader')).toHaveText(['Service', 'Installed', 'Latest upstream', 'Status', 'Health']);
    const row = (service: string) => table.getByRole('row').filter({ has: page.getByRole('rowheader', { name: service, exact: true }) });
    await expect(row('OPSEC Market (this app)')).toContainText('v0.0.0-preview');
    await expect(row('OPSEC Market (this app)')).toContainText('commit 0123456789ab');
    await expect(row('Monero daemon')).toContainText('Observed by this app (monerod get_info)');
    await expect(row('Monero daemon')).toContainText('Synced at height 3312480');
    await expect(row('Monero wallet RPC')).toContainText('wallet RPC interface 1.31');
    await expect(row('Tor')).toContainText('Not observed by this app');
    await expect(page.locator('#dependency-worker-step')).toHaveCount(0);
    await expect(panel).not.toContainText('http');
    expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(width);

    await page.goto('/admin/system?status=attention');
    await expect(page.locator('#dependency-worker-step')).toContainText('docs/install/service-topology.md#version-worker-host');
    await expect(row('Monero daemon').locator('strong')).toHaveText('Not reachable or failing: get_info timeout, 2026-01-17 09:01 UTC (1 minute ago)');
    await expect(row('PostgreSQL')).toContainText('Not compared: versions worker not configured');
    await expect(page.locator('#dependency-status')).not.toContainText('Release check unavailable');
    expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(width);

    await page.goto('/admin/payments?status=sample');
    const health = page.locator('.admin-payments');
    await expect(health.getByRole('heading', { name: 'Monero escrow watcher health', level: 3 })).toBeVisible();
    await expect(health).toContainText('Last pass: 2026-01-17 09:01 UTC (1 minute ago), completed without errors.');
    await expect(health).toContainText('Last good pass: 2026-01-17 09:01 UTC (1 minute ago).');
    await expect(page.locator('#xmr-escrow-creation-stopped')).toHaveCount(0);
    expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(width);

    await page.goto('/admin/payments?status=attention');
    await expect(page.locator('#xmr-escrow-creation-stopped')).toContainText('Monero escrow wallet creation stopped: ESCROW_XMR_SEED_KEY is not the seed key pinned in this database.');
    await expect(health.getByRole('status').filter({ hasText: 'Last pass failed' })).toContainText('monero escrow: daemon check failed: get_info timeout');
    await expect(health).toContainText('Last good pass: 2026-01-17 08:31 UTC (31 minutes ago).');
    await expect(page.locator('script')).toHaveCount(0);
    expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(width);

    await page.goto('/admin?status=attention');
    await expect(page.locator('main')).toContainText('Monero escrow needs attention.');
    expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(width);
  });
}
