import { execFileSync } from 'node:child_process';
import { defineConfig } from '@playwright/test';

// The Monero escrow no-JavaScript journey (WR-escrow Phase 3, A-248, slice X-g2; tests/e2e/escrow-xmr-journey.spec.ts):
// the real server and PostgreSQL against the SIMULATED Monero escrow wallet-rpc and fakechain daemon
// (tests/e2e/fixtures/xmr_escrow_rpc.py, served by escrow-simulators.py --xmr-wallet-port --xmr-daemon-port). Chromium only,
// 320 px, JavaScript disabled. A second server on the same database runs with ESCROW_XMR off, for the off state.
// Simulated RPC: not chain evidence.
const databaseURL = process.env.E2E_ESCROW_XMR_DATABASE_URL;
if (!databaseURL) throw new Error('E2E_ESCROW_XMR_DATABASE_URL must point to a fresh dedicated test database');
const app = 18093, appOff = 18094, xmrWallet = 18096, xmrDaemon = 18097;
process.env.E2E_XMR_SIM = `http://127.0.0.1:${xmrWallet}`;
process.env.E2E_XMR_OFF_URL = `http://127.0.0.1:${appOff}`;
// The market fee address: a standard address of a wallet outside the simulator (real keys, so it passes A-417).
const feeAddress = execFileSync('python3', ['tests/e2e/fixtures/xmr_escrow_rpc.py', '--print-address', 'market fee'], { encoding: 'utf8' }).trim();
process.env.E2E_XMR_FEE_ADDRESS = feeAddress;
const market = {
  DATABASE_URL: databaseURL, COOKIE_SECURE: 'false', APP_MODE: 'clearnet',
  SETUP_TOKEN: 'e2e-escrow-xmr-local-setup-token-at-least-32-characters',
  BITCOIN_RPC_URL: '', BITCOIN_CHAIN: '', BITCOIN_WALLET: '',
  ESCROW_BTC: '', MONERO_RPC_URL: '', MONERO_WALLET_RPC_URL: '', MONERO_NETWORK: 'stagenet',
  PAYMENT_CONFIRMATIONS_XMR: '3', PAYMENT_POLL_INTERVAL: '1s', PAYMENT_EXPIRY: '24h', AUDIT_SIGNING_KEY: '',
};
export default defineConfig({
  testDir: './tests/e2e', testMatch: /escrow-xmr-journey\.spec\.ts$/, workers: 1, retries: 0,
  forbidOnly: !!process.env.CI,
  outputDir: 'test-results-escrow-xmr',
  reporter: [['list'], ['html', { outputFolder: 'playwright-escrow-xmr-report', open: 'never' }]],
  timeout: 300_000, expect: { timeout: 15_000 },
  use: { browserName: 'chromium', baseURL: `http://127.0.0.1:${app}`, javaScriptEnabled: false,
    viewport: { width: 320, height: 800 }, trace: 'retain-on-failure', screenshot: 'only-on-failure' },
  webServer: [
    { command: `python3 tests/e2e/fixtures/escrow-simulators.py --xmr-wallet-port ${xmrWallet} --xmr-daemon-port ${xmrDaemon}`,
      url: `http://127.0.0.1:${xmrWallet}/health`, reuseExistingServer: false },
    { command: 'go run ./cmd/server', url: `http://127.0.0.1:${app}/healthz`, reuseExistingServer: false, timeout: 120_000,
      env: { ...market, ADDR: `127.0.0.1:${app}`, ESCROW_XMR: 'on', ESCROW_XMR_WALLET_RPC_URL: `http://127.0.0.1:${xmrWallet}`,
        ESCROW_XMR_DAEMON_URL: `http://127.0.0.1:${xmrDaemon}`, MONERO_WALLET_SECRET: 'e2e-escrow-xmr-wallet-secret-local-only-not-for-production',
        ESCROW_XMR_SEED_KEY: 'tests/e2e/fixtures/pgp/other.pub.asc', ESCROW_XMR_SLOTS: '5', ESCROW_XMR_FEE_BPS: '250',
        ESCROW_XMR_FEE_ADDRESS: feeAddress, ESCROW_XMR_MAX_FEE: '' } },
    { command: 'go run ./cmd/server', url: `http://127.0.0.1:${appOff}/healthz`, reuseExistingServer: false, timeout: 120_000,
      env: { ...market, ADDR: `127.0.0.1:${appOff}`, ESCROW_XMR: 'off' } },
  ],
});
