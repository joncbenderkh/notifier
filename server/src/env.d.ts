// SPDX-License-Identifier: GPL-3.0-or-later
// Secrets are not in wrangler.jsonc, so `wrangler types` cannot see them.
interface Secrets {
  ADMIN_TOKEN: string;
  FCM_SERVICE_ACCOUNT?: string;
}

declare global {
  // eslint-disable-next-line @typescript-eslint/no-empty-object-type
  interface Env extends Secrets {}
  namespace Cloudflare {
    // eslint-disable-next-line @typescript-eslint/no-empty-object-type
    interface Env extends Secrets {}
  }
}

export {};
