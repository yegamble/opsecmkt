// Every page the read-only preview serves, with its level-1 heading. Order IDs and txids have the real 64-hex
// shape (internal/market/orders_load.go) so the preview exercises the same layout as real orders; the paid
// sample adds one sample deposit row. Shared by preview.spec.ts and the 200% text loop in
// preview-accessibility.spec.ts.
export const PREVIEW_DRAFT_ID = '171abe22fd1a69af6d19320aa8548d251d0d8c4e9bb7cebfa4619778e6df0b54';
export const PREVIEW_PAID_ID = '8cf1aaf1bfa545a92504cdfe264306ee5f9d9ac801736734b173d8631a891cd9';
export const PREVIEW_DISPUTED_ID = '8c4ed43bdc4d7d6925e39713751066e19e4b37833545946bbeaa14d5dced7df2';
export const PREVIEW_DEPOSIT_TXID = '926bb57bc9bcbc21ddd00aa2e6f70f9d445378bcbaf4e55b6bfa53010ba85ddb';

export const previewRoutes: [path: string, heading: string][] = [
  ['/', 'The marketplace.'],
  ['/product?id=encrypted-drive', 'Encrypted USB Drive — 256GB'],
  ['/vendor?id=ghost', 'ghost_circuit'],
  ['/checkout?id=encrypted-drive', 'Create an order draft.'],
  ['/orders', 'Orders.'],
  [`/order?id=${PREVIEW_DRAFT_ID}`, 'Encrypted USB Drive — 256GB'],
  [`/order?id=${PREVIEW_PAID_ID}`, 'Encrypted USB Drive — 256GB'],
  ['/messages', 'Messages.'], ['/notifications', 'Notifications.'], ['/disputes', 'Disputes.'],
  ['/account', 'Your account.'], ['/vendor-dashboard', 'Vendor desk.'],
  ['/listing-edit?id=encrypted-drive', 'Edit listing.'],
  ['/moderator', 'Moderation desk.'], ['/admin', 'Control room.'],
  ['/pgp', 'PGP verification.'], ['/totp', 'Two-factor authentication.'],
  ['/canary', 'Trust is verifiable.'], ['/setup', 'Initialize your market.'],
  ['/login', 'Welcome back.'], ['/register', 'Create an account.'],
  ['/challenge', 'Verify your sign-in.'],
];
