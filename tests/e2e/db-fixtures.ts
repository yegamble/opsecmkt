// Shared constants for database projects. db-setup.spec.ts runs once first; every other
// db-*.spec.ts must create its own uniquely named users and listings (see uniqueHandle).
export const SETUP_TOKEN = 'e2e-local-only-setup-token-at-least-32-characters';
export const ADMIN = { handle: 'browser_admin', password: 'browser-admin-password-123' };
export const FIXTURE_LISTING = 'Browser regression test hardware';

export function uniqueHandle(prefix: string): string {
  return `${prefix}_${Date.now().toString(36)}${Math.floor(Math.random() * 1e6).toString(36)}`.slice(0, 32);
}
