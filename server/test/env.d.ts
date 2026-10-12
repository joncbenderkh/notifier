// SPDX-License-Identifier: GPL-3.0-or-later
// Extra bindings the test harness injects on top of wrangler.jsonc.
import type { D1Migration } from "@cloudflare/vitest-plugin";

declare global {
  namespace Cloudflare {
    interface Env {
      TEST_MIGRATIONS: D1Migration[];
    }
  }
}

export {};
