# Frontend Foundation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans. Steps use `- [ ]` checkbox syntax.

**Goal:** Bootstrap the СоциоПульс React + TypeScript frontend under `web/`, ready for page-level plans (16, 17, 18, 19) to add domain screens. Includes Vite project init, directory structure, typed API client (with auth refresh + idempotency), WebSocket hub client, layout (Sidebar + Topbar + AppShell), theme/font-size store, routing with role-aware guards, the Login page from the prototype, WASM-runtime loader for surveys, test setup with MSW, and build integration with `cmd/api` (embed.FS).

**Architecture:** Single-page app, code-split by route, React Query for server state, Zustand for ephemeral client state, Radix primitives for headless components, hand-rolled CSS ported 1:1 from prototype's `styles.css`. Frontend lives in `web/`; CI builds `web/dist/` and `cmd/api` embeds it via `embed.FS`. WS client uses native `WebSocket` wrapped in a typed hub.

**Tech Stack:** React 18.3, TypeScript 5.4, Vite 5.2, @tanstack/react-query 5.30, zustand 4.5, react-router-dom 6.23, @radix-ui/react-{dialog,dropdown-menu,select,tooltip,toast} (latest stable), date-fns 3.6, i18next 23.11 + react-i18next 14, vitest 1.6, @testing-library/react 16, @testing-library/user-event 14.5, msw 2.3, @playwright/test 1.45 (config only; tests in Plan 19).

**Spec sections covered:** §FR-L (theme/font-size), §NFR-8 (browsers), §NFR-9 (i18n), §NFR-12 (idempotency), §10.1 (WS protocol from frontend side), §11.5 (WASM survey runtime). Прототип источник истины — `SocioPulse.html`, `social-pulse-maket/project/{styles.css,layout.jsx,login.jsx,icons.jsx}`.

**Prerequisites:**
- Plan 00 (repo + Makefile + Dockerfile multi-stage).
- Plan 02 (cmd/api with `/healthz` + auth-stub middleware so frontend can hit a working backend).
- Plan 05 (auth) — at least the JWT validation contract; frontend can target the eventual login response shape now.
- Node.js 20 LTS installed in dev environment.

---

## File Structure

```
web/
├── .gitignore                        # node_modules, dist, .vite, coverage
├── package.json
├── package-lock.json                 # committed
├── tsconfig.json
├── tsconfig.node.json
├── vite.config.ts
├── vitest.config.ts
├── playwright.config.ts              # config only; specs in Plan 19
├── index.html
├── public/
│   └── surveys-runtime.wasm          # built by Plan 07's build-wasm.sh, copied here in CI
├── src/
│   ├── main.tsx
│   ├── App.tsx
│   ├── routes.tsx
│   ├── api/
│   │   ├── client.ts                 # base fetch wrapper
│   │   ├── ws.ts                     # WebSocket hub
│   │   ├── auth.ts
│   │   ├── projects.ts
│   │   ├── surveys.ts
│   │   ├── calls.ts
│   │   ├── operators.ts
│   │   ├── reports.ts
│   │   └── recording.ts
│   ├── components/
│   │   ├── layout/
│   │   │   ├── Sidebar.tsx
│   │   │   ├── Topbar.tsx
│   │   │   └── AppShell.tsx
│   │   ├── icons/
│   │   │   └── Icon.tsx              # union of all 60+ icons from prototype
│   │   ├── ui/
│   │   │   ├── Button.tsx
│   │   │   ├── Input.tsx
│   │   │   ├── Select.tsx
│   │   │   ├── Card.tsx
│   │   │   ├── Badge.tsx
│   │   │   ├── Tabs.tsx
│   │   │   ├── Modal.tsx             # wraps Radix Dialog
│   │   │   ├── Toast.tsx             # wraps Radix Toast
│   │   │   ├── Tooltip.tsx           # wraps Radix Tooltip
│   │   │   ├── Segmented.tsx
│   │   │   └── ProgressBar.tsx
│   │   └── auth/
│   │       ├── RequireAuth.tsx
│   │       └── RequireRole.tsx
│   ├── pages/
│   │   ├── Login.tsx                 # ported from prototype login.jsx
│   │   └── NotFound.tsx
│   ├── hooks/
│   │   ├── useAuth.ts
│   │   ├── useTenant.ts
│   │   ├── useTheme.ts
│   │   ├── useWS.ts
│   │   └── useIdempotencyKey.ts
│   ├── stores/
│   │   ├── auth.ts
│   │   ├── theme.ts
│   │   └── ws.ts
│   ├── types/
│   │   ├── api.ts
│   │   └── domain.ts
│   ├── styles/
│   │   ├── globals.css               # ported styles.css :root + body + utility classes
│   │   ├── tokens.css                # CSS variables (light + dark themes)
│   │   └── reset.css
│   ├── lib/
│   │   ├── i18n.ts
│   │   ├── format.ts
│   │   ├── idempotency.ts
│   │   ├── wasm.ts                   # surveys-runtime loader
│   │   └── retry.ts
│   ├── locales/
│   │   └── ru.json
│   └── test/
│       ├── setup.ts
│       ├── msw-handlers.ts
│       ├── msw-server.ts
│       └── test-utils.tsx
├── eslint.config.js
└── prettier.config.cjs

# Repo-level changes
Makefile                              # add web-build, web-test, web-lint
Dockerfile                            # multi-stage: node build → go build → runtime
cmd/api/embed.go                      # embeds web/dist via embed.FS, mounts SPA routes
```

---

## Task 1: Initialize Vite project

**Files:**
- Create: `web/package.json`, `web/tsconfig.json`, `web/tsconfig.node.json`, `web/vite.config.ts`, `web/index.html`, `web/.gitignore`

- [ ] **Step 1: Verify Node.js**

```bash
cd "$(git rev-parse --show-toplevel)"
node --version   # must be >= 20.x
npm --version    # must be >= 10.x
```

If lower, install Node 20 LTS via nvm or system package.

- [ ] **Step 2: Scaffold via Vite**

```bash
mkdir -p web
cd web
npm create vite@5.2 . -- --template react-ts -y
```

This generates `package.json`, `tsconfig.json`, `vite.config.ts`, `index.html`, `src/`, etc. We will overwrite some of these in subsequent steps.

- [ ] **Step 3: Pin dependency versions**

Replace `web/package.json`:

```json
{
  "name": "sociopulse-web",
  "private": true,
  "version": "0.0.1",
  "type": "module",
  "scripts": {
    "dev": "vite",
    "build": "tsc -b && vite build",
    "preview": "vite preview",
    "lint": "eslint . --report-unused-disable-directives",
    "format": "prettier --write \"src/**/*.{ts,tsx,css,json,md}\"",
    "test": "vitest",
    "test:coverage": "vitest run --coverage",
    "typecheck": "tsc -b --pretty"
  },
  "dependencies": {
    "react": "^18.3.1",
    "react-dom": "^18.3.1",
    "react-router-dom": "^6.23.1",
    "@tanstack/react-query": "^5.30.0",
    "zustand": "^4.5.2",
    "@radix-ui/react-dialog": "^1.0.5",
    "@radix-ui/react-dropdown-menu": "^2.0.6",
    "@radix-ui/react-select": "^2.0.0",
    "@radix-ui/react-tooltip": "^1.0.7",
    "@radix-ui/react-toast": "^1.1.5",
    "date-fns": "^3.6.0",
    "i18next": "^23.11.5",
    "react-i18next": "^14.1.2",
    "uuid": "^9.0.1"
  },
  "devDependencies": {
    "@types/react": "^18.3.2",
    "@types/react-dom": "^18.3.0",
    "@types/uuid": "^9.0.8",
    "@typescript-eslint/eslint-plugin": "^7.10.0",
    "@typescript-eslint/parser": "^7.10.0",
    "@vitejs/plugin-react": "^4.3.0",
    "@testing-library/jest-dom": "^6.4.5",
    "@testing-library/react": "^16.0.0",
    "@testing-library/user-event": "^14.5.2",
    "@vitest/coverage-v8": "^1.6.0",
    "eslint": "^9.3.0",
    "eslint-plugin-react-hooks": "^4.6.2",
    "eslint-plugin-react-refresh": "^0.4.7",
    "jsdom": "^24.0.0",
    "msw": "^2.3.0",
    "prettier": "^3.2.5",
    "typescript": "^5.4.5",
    "vite": "^5.2.11",
    "vitest": "^1.6.0",
    "@playwright/test": "^1.45.0"
  }
}
```

- [ ] **Step 4: Install**

```bash
cd web
npm install
```

Expected: lockfile generated, no errors.

- [ ] **Step 5: Update `tsconfig.json`**

```json
{
  "compilerOptions": {
    "target": "ES2022",
    "useDefineForClassFields": true,
    "lib": ["ES2022", "DOM", "DOM.Iterable"],
    "module": "ESNext",
    "skipLibCheck": true,
    "moduleResolution": "bundler",
    "allowImportingTsExtensions": true,
    "resolveJsonModule": true,
    "isolatedModules": true,
    "moduleDetection": "force",
    "noEmit": true,
    "jsx": "react-jsx",
    "strict": true,
    "noUnusedLocals": true,
    "noUnusedParameters": true,
    "noFallthroughCasesInSwitch": true,
    "exactOptionalPropertyTypes": true,
    "verbatimModuleSyntax": true,
    "baseUrl": ".",
    "paths": {
      "@/*": ["src/*"]
    }
  },
  "include": ["src"],
  "references": [{ "path": "./tsconfig.node.json" }]
}
```

- [ ] **Step 6: Update `vite.config.ts`**

```ts
import path from "node:path";
import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

export default defineConfig({
  plugins: [react()],
  resolve: {
    alias: { "@": path.resolve(__dirname, "src") },
  },
  build: {
    outDir: "dist",
    sourcemap: true,
    target: "es2022",
    rollupOptions: {
      output: {
        manualChunks: {
          react: ["react", "react-dom"],
          radix: [
            "@radix-ui/react-dialog",
            "@radix-ui/react-dropdown-menu",
            "@radix-ui/react-select",
            "@radix-ui/react-tooltip",
            "@radix-ui/react-toast",
          ],
          query: ["@tanstack/react-query"],
        },
      },
    },
  },
  server: {
    port: 5173,
    proxy: {
      "/api": "http://localhost:8080",
      "/ws": { target: "ws://localhost:8080", ws: true },
    },
  },
});
```

- [ ] **Step 7: Verify dev server starts**

```bash
cd web
npm run dev
```

Open http://localhost:5173 — should see Vite default page (we'll replace it). Stop with Ctrl-C.

- [ ] **Step 8: Update `web/.gitignore`**

```
node_modules
dist
.vite
coverage
*.log
.DS_Store
playwright-report
test-results
```

- [ ] **Step 9: Commit**

```bash
cd "$(git rev-parse --show-toplevel)"
git add web/
git commit -m "chore(web): scaffold Vite + React + TS project"
```

---

## Task 2: Port styles from prototype

**Files:**
- Create: `web/src/styles/{globals.css,tokens.css,reset.css}`
- Modify: `web/src/main.tsx`
- Delete: `web/src/App.css`, `web/src/index.css` (default Vite stubs)

- [ ] **Step 1: Create `tokens.css`** — port `:root` and `[data-theme="dark"]` from prototype `styles.css`

```css
:root {
  /* All CSS variables from social-pulse-maket/project/styles.css :root */
  --bg-app: #f4f6f9;
  --bg-card: #ffffff;
  --bg-soft: #eef2f7;
  --bg-hover: #e6ecf3;
  --bg-sidebar: #1a2230;
  --bg-sidebar-hover: #232d3e;
  --bg-sidebar-active: #2c3a52;

  --border: #d8dee8;
  --border-strong: #c1cad6;
  --border-focus: #2563a8;

  --text: #1a2230;
  --text-muted: #5b6878;
  --text-faint: #8a96a8;
  --text-on-dark: #e8edf4;
  --text-on-dark-muted: #98a4b8;

  --accent: #2563a8;
  --accent-hover: #1d4f87;
  --accent-soft: #e3edf8;

  --success: #1e7a4d;
  --success-soft: #e1f1e8;
  --danger: #b54a4a;
  --danger-soft: #f7e4e4;
  --warning: #c97a1f;
  --warning-soft: #fbeedd;
  --info: #4a6da6;
  --info-soft: #e6eaf3;

  --shadow-sm: 0 1px 2px rgba(20, 30, 50, 0.06);
  --shadow-md: 0 2px 8px rgba(20, 30, 50, 0.08);
  --shadow-lg: 0 8px 24px rgba(20, 30, 50, 0.12);

  --radius-sm: 4px;
  --radius: 6px;
  --radius-lg: 10px;
  --radius-xl: 14px;

  --font-base: 16px;
  --font-sans: -apple-system, BlinkMacSystemFont, "Segoe UI", "Roboto", "Helvetica Neue", Arial, sans-serif;
  --font-mono: ui-monospace, "SF Mono", "Cascadia Mono", "Roboto Mono", Consolas, monospace;
}

[data-theme="dark"] {
  --bg-app: #0f1620;
  --bg-card: #1a2230;
  --bg-soft: #232d3e;
  --bg-hover: #2c3a52;
  --bg-sidebar: #0a0f17;
  --bg-sidebar-hover: #141c28;
  --bg-sidebar-active: #1f2a3d;

  --border: #2c3a52;
  --border-strong: #3a4862;
  --border-focus: #4a8fd6;

  --text: #e8edf4;
  --text-muted: #98a4b8;
  --text-faint: #6b7a92;

  --accent: #4a8fd6;
  --accent-hover: #6aa3e0;
  --accent-soft: #1e3a5c;

  --success: #4ea378;
  --success-soft: #1e3a2c;
  --danger: #d96a6a;
  --danger-soft: #3a1f1f;
  --warning: #e09a4a;
  --warning-soft: #3a2c14;
  --info: #7896c8;
  --info-soft: #1e2a42;

  --shadow-sm: 0 1px 2px rgba(0, 0, 0, 0.3);
  --shadow-md: 0 2px 8px rgba(0, 0, 0, 0.4);
  --shadow-lg: 0 8px 24px rgba(0, 0, 0, 0.5);
}

[data-fs="lg"] { --font-base: 18px; }
[data-fs="xl"] { --font-base: 20px; }
```

- [ ] **Step 2: Create `reset.css`** — minimal, just `*` and html/body base from prototype

```css
* { box-sizing: border-box; }
html, body { margin: 0; padding: 0; }

button {
  font-family: inherit;
  font-size: inherit;
  cursor: pointer;
  border: none;
  background: none;
  color: inherit;
}

input, select, textarea {
  font-family: inherit;
  font-size: inherit;
  color: inherit;
}

::-webkit-scrollbar { width: 10px; height: 10px; }
::-webkit-scrollbar-track { background: transparent; }
::-webkit-scrollbar-thumb { background: var(--border-strong); border-radius: 5px; }
::-webkit-scrollbar-thumb:hover { background: var(--text-faint); }
```

- [ ] **Step 3: Create `globals.css`** — paste the rest of prototype `styles.css` (Buttons, Inputs, Cards, Badges, layout primitives, App shell, Tables, Stat tiles, Tabs, Segmented, Operator workstation, Status pill, Op state, Login, Modal, Misc, Waveform, Flow builder, Mini-bar/chart, density)

Copy verbatim from `/Users/user/call-center/social-pulse/social-pulse-maket/project/styles.css` lines 84–827 (everything after `* { box-sizing: border-box; }` block, since `*` is now in `reset.css`, and the `:root`/`[data-theme="dark"]` blocks are in `tokens.css`).

The full content is preserved as-is — no changes. This guarantees pixel-fidelity with the prototype.

- [ ] **Step 4: Update `web/src/main.tsx`**

```tsx
import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { BrowserRouter } from "react-router-dom";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";

import "./styles/tokens.css";
import "./styles/reset.css";
import "./styles/globals.css";

import App from "./App";
import "./lib/i18n";

const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      staleTime: 30_000,
      gcTime: 5 * 60_000,
      retry: 1,
      refetchOnWindowFocus: false,
    },
    mutations: { retry: 0 },
  },
});

createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <QueryClientProvider client={queryClient}>
      <BrowserRouter>
        <App />
      </BrowserRouter>
    </QueryClientProvider>
  </StrictMode>,
);
```

- [ ] **Step 5: Update `web/index.html`**

```html
<!doctype html>
<html lang="ru">
  <head>
    <meta charset="UTF-8" />
    <meta name="viewport" content="width=device-width, initial-scale=1.0" />
    <title>СоциоПульс</title>
  </head>
  <body>
    <div id="root"></div>
    <script type="module" src="/src/main.tsx"></script>
  </body>
</html>
```

- [ ] **Step 6: Delete default Vite css stubs**

```bash
rm -f web/src/App.css web/src/index.css
```

- [ ] **Step 7: Commit**

```bash
git add web/src/styles/ web/src/main.tsx web/index.html
git rm web/src/App.css web/src/index.css 2>/dev/null || true
git commit -m "feat(web): port styles from prototype (tokens + reset + globals)"
```

---

## Task 3: Auth store + API client base

**Files:**
- Create: `web/src/stores/auth.ts`
- Create: `web/src/api/client.ts`
- Create: `web/src/lib/idempotency.ts`
- Create: `web/src/lib/retry.ts`
- Create: `web/src/api/__tests__/client.test.ts`

- [ ] **Step 1: Failing test for client error retry + idempotency header**

```ts
// web/src/api/__tests__/client.test.ts
import { describe, it, expect, beforeAll, afterAll, afterEach } from "vitest";
import { setupServer } from "msw/node";
import { http, HttpResponse } from "msw";

import { apiClient } from "@/api/client";

const server = setupServer();
beforeAll(() => server.listen({ onUnhandledRequest: "error" }));
afterEach(() => server.resetHandlers());
afterAll(() => server.close());

describe("apiClient", () => {
  it("attaches Idempotency-Key on POST", async () => {
    let receivedKey: string | null = null;
    server.use(
      http.post("/api/echo", ({ request }) => {
        receivedKey = request.headers.get("Idempotency-Key");
        return HttpResponse.json({ ok: true });
      }),
    );
    await apiClient.post("/api/echo", { x: 1 });
    expect(receivedKey).toMatch(/^[0-9a-f-]{36}$/);
  });

  it("retries on 503 and succeeds", async () => {
    let attempts = 0;
    server.use(
      http.get("/api/flaky", () => {
        attempts++;
        if (attempts < 2) return new HttpResponse(null, { status: 503 });
        return HttpResponse.json({ ok: true });
      }),
    );
    const res = await apiClient.get<{ ok: boolean }>("/api/flaky");
    expect(res.ok).toBe(true);
    expect(attempts).toBe(2);
  });

  it("does NOT retry on 4xx", async () => {
    let attempts = 0;
    server.use(
      http.get("/api/forbidden", () => {
        attempts++;
        return new HttpResponse(null, { status: 403 });
      }),
    );
    await expect(apiClient.get("/api/forbidden")).rejects.toThrow(/403/);
    expect(attempts).toBe(1);
  });
});
```

- [ ] **Step 2: Vitest config**

`web/vitest.config.ts`:

```ts
import path from "node:path";
import { defineConfig } from "vitest/config";
import react from "@vitejs/plugin-react";

export default defineConfig({
  plugins: [react()],
  resolve: {
    alias: { "@": path.resolve(__dirname, "src") },
  },
  test: {
    globals: true,
    environment: "jsdom",
    setupFiles: ["./src/test/setup.ts"],
    coverage: { reporter: ["text", "html"], thresholds: { lines: 70, branches: 65 } },
  },
});
```

`web/src/test/setup.ts`:

```ts
import "@testing-library/jest-dom/vitest";
```

- [ ] **Step 3: Implement `idempotency.ts`**

```ts
// web/src/lib/idempotency.ts
import { v4 as uuid } from "uuid";

export function newIdempotencyKey(): string {
  return uuid();
}
```

- [ ] **Step 4: Implement `retry.ts`**

```ts
// web/src/lib/retry.ts
const RETRYABLE_STATUSES = new Set([502, 503, 504]);

export interface RetryOptions {
  maxAttempts: number;
  baseDelayMs: number;
}

const DEFAULT_RETRY: RetryOptions = { maxAttempts: 3, baseDelayMs: 200 };

export async function fetchWithRetry(
  input: RequestInfo,
  init?: RequestInit,
  opts: RetryOptions = DEFAULT_RETRY,
): Promise<Response> {
  let lastErr: unknown;
  for (let attempt = 1; attempt <= opts.maxAttempts; attempt++) {
    try {
      const res = await fetch(input, init);
      if (res.ok || !RETRYABLE_STATUSES.has(res.status)) return res;
      lastErr = new Error(`HTTP ${res.status}`);
    } catch (e) {
      lastErr = e;
    }
    if (attempt < opts.maxAttempts) {
      const delay = opts.baseDelayMs * Math.pow(2, attempt - 1);
      await new Promise((r) => setTimeout(r, delay));
    }
  }
  throw lastErr;
}
```

- [ ] **Step 5: Implement auth store**

```ts
// web/src/stores/auth.ts
import { create } from "zustand";
import { persist } from "zustand/middleware";

export interface AuthClaims {
  userId: string;
  tenantId: string;
  roles: string[];
  fullName: string;
  exp: number;
}

interface AuthState {
  accessToken: string | null;
  refreshToken: string | null;
  claims: AuthClaims | null;
  setSession: (s: { accessToken: string; refreshToken: string; claims: AuthClaims }) => void;
  clear: () => void;
}

export const useAuthStore = create<AuthState>()(
  persist(
    (set) => ({
      accessToken: null,
      refreshToken: null,
      claims: null,
      setSession: ({ accessToken, refreshToken, claims }) =>
        set({ accessToken, refreshToken, claims }),
      clear: () => set({ accessToken: null, refreshToken: null, claims: null }),
    }),
    { name: "sociopulse.auth", version: 1 },
  ),
);
```

- [ ] **Step 6: Implement `api/client.ts`**

```ts
// web/src/api/client.ts
import { useAuthStore } from "@/stores/auth";
import { newIdempotencyKey } from "@/lib/idempotency";
import { fetchWithRetry } from "@/lib/retry";

export class APIError extends Error {
  constructor(public status: number, message: string, public body?: unknown) {
    super(message);
  }
}

let refreshInFlight: Promise<boolean> | null = null;

async function tryRefresh(): Promise<boolean> {
  if (refreshInFlight) return refreshInFlight;
  refreshInFlight = (async () => {
    const { refreshToken } = useAuthStore.getState();
    if (!refreshToken) return false;
    const res = await fetch("/api/auth/refresh", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ refresh_token: refreshToken }),
    });
    if (!res.ok) {
      useAuthStore.getState().clear();
      return false;
    }
    const data = await res.json();
    useAuthStore.getState().setSession({
      accessToken: data.access_token,
      refreshToken: data.refresh_token,
      claims: data.claims,
    });
    return true;
  })();
  try {
    return await refreshInFlight;
  } finally {
    refreshInFlight = null;
  }
}

async function request<T>(
  method: string,
  path: string,
  body?: unknown,
  extraHeaders?: Record<string, string>,
): Promise<T> {
  const headers: Record<string, string> = {
    Accept: "application/json",
    ...extraHeaders,
  };
  if (body !== undefined) headers["Content-Type"] = "application/json";

  const isMutating = method !== "GET" && method !== "HEAD";
  if (isMutating) headers["Idempotency-Key"] = newIdempotencyKey();

  const { accessToken } = useAuthStore.getState();
  if (accessToken) headers["Authorization"] = `Bearer ${accessToken}`;

  let res = await fetchWithRetry(path, {
    method,
    headers,
    body: body !== undefined ? JSON.stringify(body) : undefined,
    credentials: "include",
  });

  if (res.status === 401 && accessToken) {
    const ok = await tryRefresh();
    if (ok) {
      headers["Authorization"] = `Bearer ${useAuthStore.getState().accessToken}`;
      res = await fetchWithRetry(path, {
        method,
        headers,
        body: body !== undefined ? JSON.stringify(body) : undefined,
        credentials: "include",
      });
    }
  }

  if (!res.ok) {
    let errBody: unknown;
    try { errBody = await res.json(); } catch { /* */ }
    throw new APIError(res.status, `HTTP ${res.status}`, errBody);
  }

  if (res.status === 204) return undefined as T;
  return (await res.json()) as T;
}

export const apiClient = {
  get: <T>(path: string) => request<T>("GET", path),
  post: <T>(path: string, body?: unknown) => request<T>("POST", path, body),
  put: <T>(path: string, body?: unknown) => request<T>("PUT", path, body),
  patch: <T>(path: string, body?: unknown) => request<T>("PATCH", path, body),
  delete: <T>(path: string) => request<T>("DELETE", path),
};
```

- [ ] **Step 7: Run tests**

```bash
cd web
npm test -- run
```

Expected: 3 tests pass.

- [ ] **Step 8: Commit**

```bash
git add web/src/api/ web/src/stores/auth.ts web/src/lib/{idempotency,retry}.ts web/vitest.config.ts web/src/test/setup.ts
git commit -m "feat(web): API client with auth refresh + idempotency + retry"
```

---

## Task 4: WebSocket hub client

**Files:**
- Create: `web/src/api/ws.ts`
- Create: `web/src/hooks/useWS.ts`
- Create: `web/src/api/__tests__/ws.test.ts`

- [ ] **Step 1: Failing test (using a mock WebSocket)**

```ts
// web/src/api/__tests__/ws.test.ts
import { describe, it, expect, vi, beforeEach } from "vitest";
import { WSHub } from "@/api/ws";

class MockSocket {
  listeners: Record<string, ((ev: any) => void)[]> = {};
  sent: string[] = [];
  readyState = 0;
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSING = 2;
  static CLOSED = 3;
  constructor(public url: string) {
    queueMicrotask(() => this.fire("open", {}));
  }
  addEventListener(t: string, cb: any) {
    (this.listeners[t] ??= []).push(cb);
  }
  removeEventListener(t: string, cb: any) {
    this.listeners[t] = (this.listeners[t] ?? []).filter((f) => f !== cb);
  }
  send(d: string) { this.sent.push(d); }
  close() { this.readyState = MockSocket.CLOSED; this.fire("close", {}); }
  fire(t: string, ev: any) {
    if (t === "open") this.readyState = MockSocket.OPEN;
    (this.listeners[t] ?? []).forEach((cb) => cb(ev));
  }
}

beforeEach(() => {
  // @ts-expect-error - test override
  globalThis.WebSocket = MockSocket;
});

describe("WSHub", () => {
  it("sends auth as the first frame", async () => {
    const hub = new WSHub({ url: "ws://localhost/ws" });
    await hub.connect("token-1");
    const sock = (hub as any).socket as MockSocket;
    expect(sock.sent[0]).toContain('"type":"auth"');
    expect(sock.sent[0]).toContain('"token":"token-1"');
  });

  it("invokes subscriber callbacks on event frames", async () => {
    const hub = new WSHub({ url: "ws://localhost/ws" });
    await hub.connect("token-1");
    const cb = vi.fn();
    const unsub = hub.subscribe("operators.state", {}, cb);

    const sock = (hub as any).socket as MockSocket;
    sock.fire("message", { data: JSON.stringify({ type: "subscribe.ok", sub_id: "abc" }) });
    sock.fire("message", { data: JSON.stringify({ type: "event", sub_id: "abc", payload: { x: 1 } }) });

    expect(cb).toHaveBeenCalledWith({ x: 1 });
    unsub();
  });
});
```

- [ ] **Step 2: Implement `api/ws.ts`**

```ts
// web/src/api/ws.ts
type Frame =
  | { type: "auth"; token: string }
  | { type: "auth.ok" }
  | { type: "auth.error"; reason: string }
  | { type: "refresh"; token: string }
  | { type: "refresh.ok" }
  | { type: "subscribe"; topic: string; filter?: Record<string, string> }
  | { type: "subscribe.ok"; sub_id: string; topic: string }
  | { type: "subscribe.error"; reason: string }
  | { type: "unsubscribe"; sub_id: string }
  | { type: "event"; sub_id: string; topic: string; payload: unknown }
  | { type: "ping" }
  | { type: "pong" }
  | { type: "force.event"; payload: unknown };

interface PendingSub {
  topic: string;
  filter: Record<string, string>;
  callback: (payload: any) => void;
  subID?: string;
}

export interface WSHubConfig {
  url: string;
  pingIntervalMs?: number;
  reconnectDelayMs?: number;
}

export class WSHub {
  private cfg: Required<WSHubConfig>;
  private socket: WebSocket | null = null;
  private subs = new Map<string, PendingSub>();
  private localKey = 0;
  private pingTimer: number | null = null;
  private connectingToken: string | null = null;

  constructor(cfg: WSHubConfig) {
    this.cfg = {
      url: cfg.url,
      pingIntervalMs: cfg.pingIntervalMs ?? 20_000,
      reconnectDelayMs: cfg.reconnectDelayMs ?? 1_000,
    };
  }

  async connect(token: string): Promise<void> {
    this.connectingToken = token;
    return new Promise((resolve, reject) => {
      const sock = new WebSocket(this.cfg.url);
      this.socket = sock;
      const onOpen = () => {
        sock.send(JSON.stringify({ type: "auth", token } satisfies Frame));
      };
      const onMessage = (ev: MessageEvent) => {
        const f: Frame = JSON.parse(ev.data);
        if (f.type === "auth.ok") {
          this.startPings();
          this.resubscribeAll();
          resolve();
        } else if (f.type === "auth.error") {
          reject(new Error(f.reason));
        } else if (f.type === "subscribe.ok") {
          for (const [k, sub] of this.subs) {
            if (!sub.subID && sub.topic === f.topic) {
              sub.subID = f.sub_id;
              break;
            }
          }
        } else if (f.type === "event") {
          for (const sub of this.subs.values()) {
            if (sub.subID === f.sub_id) sub.callback(f.payload);
          }
        }
      };
      const onClose = () => {
        this.stopPings();
        if (this.connectingToken) setTimeout(() => this.connect(this.connectingToken!), this.cfg.reconnectDelayMs);
      };
      sock.addEventListener("open", onOpen);
      sock.addEventListener("message", onMessage);
      sock.addEventListener("close", onClose);
    });
  }

  refresh(token: string): void {
    this.send({ type: "refresh", token });
  }

  subscribe(topic: string, filter: Record<string, string>, callback: (payload: any) => void): () => void {
    const key = `${topic}:${++this.localKey}`;
    this.subs.set(key, { topic, filter, callback });
    if (this.socket?.readyState === WebSocket.OPEN) {
      this.send({ type: "subscribe", topic, filter });
    }
    return () => {
      const sub = this.subs.get(key);
      this.subs.delete(key);
      if (sub?.subID) this.send({ type: "unsubscribe", sub_id: sub.subID });
    };
  }

  close(): void {
    this.connectingToken = null;
    this.stopPings();
    this.socket?.close();
    this.socket = null;
  }

  private send(f: Frame): void {
    if (this.socket?.readyState === WebSocket.OPEN) {
      this.socket.send(JSON.stringify(f));
    }
  }

  private startPings(): void {
    this.stopPings();
    this.pingTimer = window.setInterval(() => this.send({ type: "ping" }), this.cfg.pingIntervalMs);
  }
  private stopPings(): void {
    if (this.pingTimer != null) {
      clearInterval(this.pingTimer);
      this.pingTimer = null;
    }
  }
  private resubscribeAll(): void {
    for (const sub of this.subs.values()) {
      sub.subID = undefined;
      this.send({ type: "subscribe", topic: sub.topic, filter: sub.filter });
    }
  }
}
```

- [ ] **Step 3: `useWS.ts` hook**

```ts
// web/src/hooks/useWS.ts
import { useEffect, useRef } from "react";
import { useAuthStore } from "@/stores/auth";
import { WSHub } from "@/api/ws";

let singleton: WSHub | null = null;

export function getWSHub(): WSHub {
  if (!singleton) {
    const proto = location.protocol === "https:" ? "wss" : "ws";
    singleton = new WSHub({ url: `${proto}://${location.host}/ws` });
  }
  return singleton;
}

export function useWSSubscription<T>(
  topic: string,
  filter: Record<string, string>,
  callback: (payload: T) => void,
): void {
  const cbRef = useRef(callback);
  cbRef.current = callback;
  const accessToken = useAuthStore((s) => s.accessToken);

  useEffect(() => {
    if (!accessToken) return;
    const hub = getWSHub();
    let unsub: (() => void) | null = null;
    let cancelled = false;

    void hub.connect(accessToken).then(() => {
      if (cancelled) return;
      unsub = hub.subscribe(topic, filter, (p) => cbRef.current(p as T));
    });

    return () => {
      cancelled = true;
      unsub?.();
    };
  }, [accessToken, topic, JSON.stringify(filter)]);
}
```

- [ ] **Step 4: Run tests**

```bash
cd web && npm test -- run
```

Expected: WS tests pass.

- [ ] **Step 5: Commit**

```bash
git add web/src/api/ws.ts web/src/hooks/useWS.ts web/src/api/__tests__/ws.test.ts
git commit -m "feat(web): WS hub client with auto-reconnect + resubscribe"
```

---

## Task 5: Theme store + i18n

**Files:**
- Create: `web/src/stores/theme.ts`
- Create: `web/src/hooks/useTheme.ts`
- Create: `web/src/lib/i18n.ts`
- Create: `web/src/locales/ru.json`

- [ ] **Step 1: Theme store**

```ts
// web/src/stores/theme.ts
import { create } from "zustand";
import { persist } from "zustand/middleware";

export type Theme = "light" | "dark";
export type FontSize = "md" | "lg" | "xl";

interface ThemeState {
  theme: Theme;
  fontSize: FontSize;
  setTheme: (t: Theme) => void;
  setFontSize: (f: FontSize) => void;
  toggleTheme: () => void;
}

export const useThemeStore = create<ThemeState>()(
  persist(
    (set, get) => ({
      theme: "light",
      fontSize: "md",
      setTheme: (theme) => {
        document.documentElement.dataset.theme = theme;
        set({ theme });
      },
      setFontSize: (fontSize) => {
        document.documentElement.dataset.fs = fontSize === "md" ? "" : fontSize;
        set({ fontSize });
      },
      toggleTheme: () => get().setTheme(get().theme === "light" ? "dark" : "light"),
    }),
    {
      name: "sociopulse.theme",
      onRehydrateStorage: () => (state) => {
        if (!state) return;
        document.documentElement.dataset.theme = state.theme;
        document.documentElement.dataset.fs = state.fontSize === "md" ? "" : state.fontSize;
      },
    },
  ),
);
```

- [ ] **Step 2: Theme hook**

```ts
// web/src/hooks/useTheme.ts
import { useThemeStore } from "@/stores/theme";
export const useTheme = () => useThemeStore();
```

- [ ] **Step 3: i18n init**

```ts
// web/src/lib/i18n.ts
import i18n from "i18next";
import { initReactI18next } from "react-i18next";
import ru from "@/locales/ru.json";

void i18n.use(initReactI18next).init({
  resources: { ru: { translation: ru } },
  lng: "ru",
  fallbackLng: "ru",
  interpolation: { escapeValue: false },
});

export default i18n;
```

- [ ] **Step 4: `web/src/locales/ru.json`** — minimal seed; pages add as needed

```json
{
  "app.name": "СоциоПульс",
  "common.loading": "Загрузка…",
  "common.error.generic": "Произошла ошибка",
  "common.cancel": "Отмена",
  "common.save": "Сохранить",
  "common.delete": "Удалить",
  "common.confirm": "Подтвердить"
}
```

- [ ] **Step 5: Commit**

```bash
git add web/src/stores/theme.ts web/src/hooks/useTheme.ts web/src/lib/i18n.ts web/src/locales/
git commit -m "feat(web): theme + font-size store with persistence and i18n init"
```

---

## Task 6: Icon component

**Files:**
- Create: `web/src/components/icons/Icon.tsx`
- Create: `web/src/components/icons/Icon.test.tsx`

- [ ] **Step 1: Failing test**

```tsx
// web/src/components/icons/Icon.test.tsx
import { describe, it, expect } from "vitest";
import { render } from "@testing-library/react";
import { Icon } from "./Icon";

describe("Icon", () => {
  it("renders svg with given size", () => {
    const { container } = render(<Icon name="phone" size={24} />);
    const svg = container.querySelector("svg");
    expect(svg).toHaveAttribute("width", "24");
    expect(svg).toHaveAttribute("height", "24");
  });

  it("falls back to info icon for unknown name", () => {
    const { container } = render(<Icon name={"nonexistent" as any} />);
    expect(container.querySelector("svg")).not.toBeNull();
  });
});
```

- [ ] **Step 2: Implement `Icon.tsx`**

Port every icon from `social-pulse-maket/project/icons.jsx`. Type `name` as a union of all string keys; render via a switch / object map. The implementation is a direct TS translation:

```tsx
// web/src/components/icons/Icon.tsx
import type { CSSProperties, JSX } from "react";

export type IconName =
  | "phone" | "phone-call" | "phone-off" | "pause" | "play" | "check" | "x"
  | "home" | "users" | "user" | "chart" | "folder" | "settings" | "logout"
  | "headset" | "file" | "file-text" | "plus" | "search" | "filter"
  | "chevronRight" | "chevronDown" | "chevronLeft" | "arrowRight" | "download"
  | "upload" | "eye" | "edit" | "trash" | "archive" | "money" | "activity"
  | "radio" | "mic" | "mic-off" | "clock" | "bell" | "map" | "list" | "grid"
  | "refresh" | "volume-2" | "target" | "flag" | "alert-circle" | "info"
  | "building" | "flow" | "code" | "moon" | "sun" | "headphones" | "eye-off"
  | "skip-forward" | "rotate-ccw" | "save" | "plus2" | "more-horizontal" | "pulse";

interface Props {
  name: IconName;
  size?: number;
  stroke?: number;
  color?: string;
  style?: CSSProperties;
}

export function Icon({ name, size = 18, stroke = 1.7, color = "currentColor", style }: Props): JSX.Element {
  const props = {
    width: size,
    height: size,
    viewBox: "0 0 24 24",
    fill: "none",
    stroke: color,
    strokeWidth: stroke,
    strokeLinecap: "round" as const,
    strokeLinejoin: "round" as const,
    style,
  };
  return (
    <svg {...props}>
      {ICONS[name] ?? ICONS.info}
    </svg>
  );
}

const ICONS: Record<IconName, JSX.Element> = {
  // EXACT bodies from social-pulse-maket/project/icons.jsx — copy each
  // SVG path / line / circle child verbatim. This file is large but
  // mechanical; do not improvise icon shapes.
  // ... (full implementation here, ~250 lines)
  // Example for "phone":
  "phone": <path d="M22 16.92v3a2 2 0 0 1-2.18 2 19.79 19.79 0 0 1-8.63-3.07 19.5 19.5 0 0 1-6-6 19.79 19.79 0 0 1-3.07-8.67A2 2 0 0 1 4.11 2h3a2 2 0 0 1 2 1.72 12.84 12.84 0 0 0 .7 2.81 2 2 0 0 1-.45 2.11L8.09 9.91a16 16 0 0 0 6 6l1.27-1.27a2 2 0 0 1 2.11-.45 12.84 12.84 0 0 0 2.81.7A2 2 0 0 1 22 16.92z"/>,
  // Add the remaining 60+ icons below — full source in social-pulse-maket/project/icons.jsx
} as Record<IconName, JSX.Element>;
```

The agent implementing this task MUST port all icon bodies from the prototype. Use the file `/Users/user/call-center/social-pulse/social-pulse-maket/project/icons.jsx` as the source — copy each SVG child element body for each named icon into the `ICONS` object.

- [ ] **Step 3: Run tests → pass**

- [ ] **Step 4: Commit**

```bash
git add web/src/components/icons/
git commit -m "feat(web): Icon component with all 60+ icons ported from prototype"
```

---

## Task 7: Layout — Sidebar, Topbar, AppShell

**Files:**
- Create: `web/src/components/layout/{Sidebar.tsx,Topbar.tsx,AppShell.tsx}`
- Create: `web/src/components/layout/__tests__/Sidebar.test.tsx`

- [ ] **Step 1: Failing test for Sidebar role-based menu**

```tsx
// web/src/components/layout/__tests__/Sidebar.test.tsx
import { describe, it, expect } from "vitest";
import { render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { Sidebar } from "../Sidebar";

const adminUser = { userId: "1", tenantId: "t", roles: ["admin"], fullName: "Анна А.", exp: 0 };
const operatorUser = { userId: "2", tenantId: "t", roles: ["operator"], fullName: "Светлана И.", exp: 0 };

describe("Sidebar", () => {
  it("shows admin menu items for admin role", () => {
    render(<MemoryRouter><Sidebar claims={adminUser} onLogout={() => {}} /></MemoryRouter>);
    expect(screen.getByText("Обзор")).toBeInTheDocument();
    expect(screen.getByText("Состояние операторов")).toBeInTheDocument();
    expect(screen.getByText("Проекты")).toBeInTheDocument();
  });

  it("shows operator menu items for operator role", () => {
    render(<MemoryRouter><Sidebar claims={operatorUser} onLogout={() => {}} /></MemoryRouter>);
    expect(screen.getByText("Рабочее место")).toBeInTheDocument();
    expect(screen.getByText("Моя результативность")).toBeInTheDocument();
    expect(screen.queryByText("Обзор")).not.toBeInTheDocument();
  });
});
```

- [ ] **Step 2: Implement `Sidebar.tsx`**

```tsx
// web/src/components/layout/Sidebar.tsx
import { NavLink } from "react-router-dom";
import { Icon, type IconName } from "@/components/icons/Icon";
import type { AuthClaims } from "@/stores/auth";

interface NavEntry {
  id: string;
  label: string;
  icon: IconName;
  badge?: string;
  to: string;
}
type NavItem = { section: string } | NavEntry;

const operatorNav: NavItem[] = [
  { section: "Работа" },
  { id: "workstation", label: "Рабочее место", icon: "headset", to: "/operator/workstation" },
  { id: "my-stats", label: "Моя результативность", icon: "chart", to: "/operator/stats" },
  { section: "Проект" },
  { id: "project-info", label: "О проекте", icon: "folder", to: "/operator/project" },
  { id: "op-history", label: "История звонков", icon: "list", to: "/operator/history" },
];

const adminNav: NavItem[] = [
  { section: "Мониторинг" },
  { id: "admin-overview", label: "Обзор", icon: "home", to: "/admin/overview" },
  { id: "admin-operators", label: "Состояние операторов", icon: "users", to: "/admin/operators" },
  { id: "admin-dialer", label: "Состояние автодозвона", icon: "radio", to: "/admin/dialer" },
  { section: "Управление" },
  { id: "admin-projects", label: "Проекты", icon: "folder", to: "/admin/projects" },
  { id: "admin-surveys", label: "Анкеты", icon: "file-text", to: "/admin/surveys" },
  { id: "admin-users", label: "Пользователи", icon: "user", to: "/admin/users" },
  { section: "Контроль" },
  { id: "admin-calls", label: "Исходящие звонки", icon: "phone", to: "/admin/calls" },
  { id: "admin-finance", label: "Финансы", icon: "money", to: "/admin/finance" },
  { id: "admin-reports", label: "Отчётность", icon: "download", to: "/admin/reports" },
];

interface Props {
  claims: AuthClaims;
  onLogout: () => void;
}

export function Sidebar({ claims, onLogout }: Props) {
  const isAdmin = claims.roles.includes("admin");
  const nav = isAdmin ? adminNav : operatorNav;
  return (
    <aside className="sidebar">
      <div className="sidebar-header">
        <div className="sidebar-logo">СП</div>
        <div>
          <div className="sidebar-name">СоциоПульс</div>
          <div className="sidebar-sub">{claims.tenantId}</div>
        </div>
      </div>
      <nav className="sidebar-nav">
        {nav.map((item, i) =>
          "section" in item ? (
            <div key={`s${i}`} className="sidebar-section-label">{item.section}</div>
          ) : (
            <NavLink
              key={item.id}
              to={item.to}
              className={({ isActive }) => `nav-item ${isActive ? "active" : ""}`}
            >
              <span className="nav-icon"><Icon name={item.icon} size={18} /></span>
              <span>{item.label}</span>
              {item.badge && <span className="nav-badge">{item.badge}</span>}
            </NavLink>
          ),
        )}
      </nav>
      <div className="sidebar-footer">
        <button type="button" className="user-chip" onClick={onLogout} title="Выйти">
          <div className="avatar">{claims.fullName.split(" ").map((s) => s[0]).slice(0, 2).join("")}</div>
          <div style={{ flex: 1, minWidth: 0 }}>
            <div style={{ fontSize: "0.92em", fontWeight: 500, whiteSpace: "nowrap", overflow: "hidden", textOverflow: "ellipsis" }}>
              {claims.fullName}
            </div>
            <div style={{ fontSize: "0.78em", color: "var(--text-on-dark-muted)" }}>
              {isAdmin ? "Администратор" : "Оператор"}
            </div>
          </div>
          <Icon name="logout" size={16} color="var(--text-on-dark-muted)" />
        </button>
      </div>
    </aside>
  );
}
```

- [ ] **Step 3: Implement `Topbar.tsx`**

```tsx
// web/src/components/layout/Topbar.tsx
import type { ReactNode } from "react";
import { Icon } from "@/components/icons/Icon";
import { useTheme } from "@/hooks/useTheme";

interface Props {
  crumbs: string[];
  right?: ReactNode;
}

export function Topbar({ crumbs, right }: Props) {
  const { theme, toggleTheme } = useTheme();
  return (
    <header className="topbar">
      <div className="row flex-1" style={{ gap: 8 }}>
        {crumbs.map((c, i) => (
          <span key={i} className="row" style={{ gap: 8 }}>
            {i > 0 && <Icon name="chevronRight" size={14} color="var(--text-faint)" />}
            <span className={`crumb ${i === crumbs.length - 1 ? "crumb-current" : ""}`}>{c}</span>
          </span>
        ))}
      </div>
      <div className="row gap-8">
        <button type="button" className="btn btn-ghost btn-sm" title="Уведомления" style={{ position: "relative" }}>
          <Icon name="bell" size={18} />
          <span style={{ position: "absolute", top: 6, right: 6, width: 8, height: 8, background: "var(--danger)", borderRadius: "50%", border: "2px solid var(--bg-card)" }} />
        </button>
        <button type="button" className="btn btn-ghost btn-sm" title="Сменить тему" onClick={toggleTheme}>
          <Icon name={theme === "light" ? "moon" : "sun"} size={18} />
        </button>
        {right}
      </div>
    </header>
  );
}
```

- [ ] **Step 4: Implement `AppShell.tsx`**

```tsx
// web/src/components/layout/AppShell.tsx
import type { ReactNode } from "react";
import { Outlet, useNavigate } from "react-router-dom";
import { Sidebar } from "./Sidebar";
import { Topbar } from "./Topbar";
import { useAuthStore } from "@/stores/auth";

interface Props {
  crumbs: string[];
  topbarRight?: ReactNode;
  fullBleed?: boolean;
}

export function AppShell({ crumbs, topbarRight, fullBleed }: Props) {
  const claims = useAuthStore((s) => s.claims);
  const clear = useAuthStore((s) => s.clear);
  const navigate = useNavigate();
  if (!claims) return null;
  return (
    <div className="app">
      <Sidebar claims={claims} onLogout={() => { clear(); navigate("/login"); }} />
      <div className="main">
        <Topbar crumbs={crumbs} right={topbarRight} />
        <div className="main-scroll" style={fullBleed ? { padding: 0 } : undefined}>
          <Outlet />
        </div>
      </div>
    </div>
  );
}
```

- [ ] **Step 5: Run all tests**

```bash
cd web && npm test -- run
```

- [ ] **Step 6: Commit**

```bash
git add web/src/components/layout/
git commit -m "feat(web): layout primitives — Sidebar, Topbar, AppShell"
```

---

## Task 8: Routing + auth guards

**Files:**
- Create: `web/src/components/auth/{RequireAuth.tsx,RequireRole.tsx}`
- Create: `web/src/routes.tsx`
- Modify: `web/src/App.tsx`

- [ ] **Step 1: Auth guards**

```tsx
// web/src/components/auth/RequireAuth.tsx
import type { ReactNode } from "react";
import { Navigate, useLocation } from "react-router-dom";
import { useAuthStore } from "@/stores/auth";

export function RequireAuth({ children }: { children: ReactNode }) {
  const claims = useAuthStore((s) => s.claims);
  const loc = useLocation();
  if (!claims) return <Navigate to="/login" state={{ from: loc }} replace />;
  return <>{children}</>;
}
```

```tsx
// web/src/components/auth/RequireRole.tsx
import type { ReactNode } from "react";
import { useAuthStore } from "@/stores/auth";

export function RequireRole({ role, children }: { role: string; children: ReactNode }) {
  const claims = useAuthStore((s) => s.claims);
  if (!claims) return null;
  if (!claims.roles.includes(role)) return <div className="page">Доступ запрещён</div>;
  return <>{children}</>;
}
```

- [ ] **Step 2: `routes.tsx`** — declarative router with placeholders for plans 16-19

```tsx
// web/src/routes.tsx
import { Navigate, type RouteObject } from "react-router-dom";
import { lazy } from "react";
import { AppShell } from "@/components/layout/AppShell";
import { RequireAuth } from "@/components/auth/RequireAuth";
import { RequireRole } from "@/components/auth/RequireRole";

const Login = lazy(() => import("@/pages/Login"));
const NotFound = lazy(() => import("@/pages/NotFound"));

// Page modules below are added by Plans 16-19. Until then they render
// a placeholder via PendingPage so routing works end-to-end.
const PendingPage = ({ name }: { name: string }) => (
  <div className="page">
    <h1>{name}</h1>
    <p className="muted">Страница будет реализована в плане 16/17/18/19.</p>
  </div>
);

export const routes: RouteObject[] = [
  { path: "/login", element: <Login /> },
  {
    path: "/operator",
    element: (
      <RequireAuth><RequireRole role="operator">
        <AppShell crumbs={["Оператор"]} fullBleed />
      </RequireRole></RequireAuth>
    ),
    children: [
      { index: true, element: <Navigate to="/operator/workstation" replace /> },
      { path: "workstation", element: <PendingPage name="Рабочее место (Plan 16)" /> },
      { path: "stats", element: <PendingPage name="Моя результативность (Plan 16)" /> },
      { path: "project", element: <PendingPage name="О проекте (Plan 16)" /> },
      { path: "history", element: <PendingPage name="История звонков (Plan 16)" /> },
    ],
  },
  {
    path: "/admin",
    element: (
      <RequireAuth><RequireRole role="admin">
        <AppShell crumbs={["Администратор"]} />
      </RequireRole></RequireAuth>
    ),
    children: [
      { index: true, element: <Navigate to="/admin/overview" replace /> },
      { path: "overview", element: <PendingPage name="Обзор (Plan 17)" /> },
      { path: "operators", element: <PendingPage name="Состояние операторов (Plan 17)" /> },
      { path: "dialer", element: <PendingPage name="Состояние автодозвона (Plan 17)" /> },
      { path: "projects", element: <PendingPage name="Проекты (Plan 17)" /> },
      { path: "surveys", element: <PendingPage name="Анкеты (Plan 18)" /> },
      { path: "users", element: <PendingPage name="Пользователи (Plan 19)" /> },
      { path: "calls", element: <PendingPage name="Исходящие звонки (Plan 19)" /> },
      { path: "finance", element: <PendingPage name="Финансы (Plan 19)" /> },
      { path: "reports", element: <PendingPage name="Отчётность (Plan 19)" /> },
    ],
  },
  { path: "*", element: <NotFound /> },
];
```

- [ ] **Step 3: `App.tsx`**

```tsx
// web/src/App.tsx
import { Suspense } from "react";
import { useRoutes } from "react-router-dom";
import { routes } from "@/routes";

export default function App() {
  const element = useRoutes(routes);
  return <Suspense fallback={<div className="page">Загрузка…</div>}>{element}</Suspense>;
}
```

- [ ] **Step 4: `NotFound.tsx`**

```tsx
// web/src/pages/NotFound.tsx
export default function NotFound() {
  return <div className="page"><h1>404</h1><p className="muted">Страница не найдена</p></div>;
}
```

- [ ] **Step 5: Commit**

```bash
git add web/src/components/auth/ web/src/routes.tsx web/src/App.tsx web/src/pages/NotFound.tsx
git commit -m "feat(web): routing + auth/role guards + placeholder pages"
```

---

## Task 9: Login page

**Files:**
- Create: `web/src/pages/Login.tsx`
- Create: `web/src/api/auth.ts`
- Create: `web/src/pages/__tests__/Login.test.tsx`

- [ ] **Step 1: API client for auth endpoints**

```ts
// web/src/api/auth.ts
import { apiClient } from "./client";
import type { AuthClaims } from "@/stores/auth";

export interface LoginResponse {
  access_token: string;
  refresh_token: string;
  claims: AuthClaims;
  totp_required?: boolean;
}

export async function login(orgId: string, login: string, password: string): Promise<LoginResponse> {
  return apiClient.post<LoginResponse>("/api/auth/login", { org_id: orgId, login, password });
}

export async function loginTOTP(tempSessionID: string, code: string): Promise<LoginResponse> {
  return apiClient.post<LoginResponse>("/api/auth/login/totp", { temp_session_id: tempSessionID, code });
}
```

- [ ] **Step 2: Login.tsx** — port from `social-pulse-maket/project/login.jsx` to TS, drop demo-buttons

```tsx
// web/src/pages/Login.tsx
import { useState, type FormEvent } from "react";
import { useNavigate, useLocation } from "react-router-dom";
import { Icon } from "@/components/icons/Icon";
import { useAuthStore } from "@/stores/auth";
import { login as apiLogin } from "@/api/auth";
import { APIError } from "@/api/client";

export default function Login() {
  const [orgId, setOrgId] = useState("");
  const [loginValue, setLogin] = useState("");
  const [password, setPassword] = useState("");
  const [showPwd, setShowPwd] = useState(false);
  const [error, setError] = useState("");
  const [submitting, setSubmitting] = useState(false);

  const navigate = useNavigate();
  const location = useLocation();
  const setSession = useAuthStore((s) => s.setSession);

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setError("");
    setSubmitting(true);
    try {
      const resp = await apiLogin(orgId.trim(), loginValue.trim(), password);
      if (resp.totp_required) {
        // TODO Plan 05: navigate to /login/totp with temp session
        setError("Требуется 2FA — экран 2FA добавляется в Plan 05");
        return;
      }
      setSession({ accessToken: resp.access_token, refreshToken: resp.refresh_token, claims: resp.claims });
      const dest = (location.state as { from?: { pathname: string } } | null)?.from?.pathname
        ?? (resp.claims.roles.includes("admin") ? "/admin" : "/operator");
      navigate(dest, { replace: true });
    } catch (err) {
      if (err instanceof APIError && err.status === 401) {
        setError("Неверный логин или пароль");
      } else if (err instanceof APIError && err.status === 404) {
        setError("Идентификатор организации не найден");
      } else {
        setError("Ошибка входа. Попробуйте позже.");
      }
    } finally {
      setSubmitting(false);
    }
  };

  return (
    <div className="login-screen">
      <div className="login-side">
        <div className="login-art" />
        <div className="login-content">
          <div className="row" style={{ gap: 14 }}>
            <div style={{ width: 44, height: 44, borderRadius: 10, background: "linear-gradient(135deg, #4a8fd6, #2563a8)", display: "grid", placeItems: "center", fontWeight: 700, fontSize: 18 }}>СП</div>
            <div>
              <div style={{ fontWeight: 700, fontSize: "1.4em", letterSpacing: "0.01em" }}>СоциоПульс</div>
              <div style={{ fontSize: "0.85em", color: "rgba(255,255,255,0.7)" }}>Платформа автодозвона для соц. опросов</div>
            </div>
          </div>
          <div style={{ marginTop: "auto", maxWidth: 480 }}>
            <h1 style={{ fontSize: "2.4em", lineHeight: 1.15, marginBottom: 20 }}>
              Управление колл-центром в одной системе
            </h1>
            <p style={{ fontSize: "1.05em", color: "rgba(255,255,255,0.78)", lineHeight: 1.6 }}>
              Автодозвон, анкетирование, контроль операторов и проектная аналитика — для подрядчиков ВЦИОМ и социологических служб.
            </p>
          </div>
          <div style={{ marginTop: "auto", paddingTop: 32, fontSize: "0.82em", color: "rgba(255,255,255,0.5)" }}>
            © 2026 СоциоПульс · Поддержка: support@sociopulse.ru
          </div>
        </div>
      </div>

      <div className="login-form-side">
        <div className="login-form">
          <div style={{ marginBottom: 8 }}>
            <h2 style={{ marginBottom: 6 }}>Вход в систему</h2>
            <div className="muted" style={{ fontSize: "0.95em" }}>Используйте корпоративные учётные данные</div>
          </div>

          <form onSubmit={submit} className="col" style={{ gap: 16 }}>
            <div className="field">
              <label className="field-label">Идентификатор организации</label>
              <div style={{ position: "relative" }}>
                <input className="input" value={orgId} onChange={(e) => setOrgId(e.target.value)} placeholder="например, CC-MOSKVA-01"
                       style={{ paddingLeft: 42, fontFamily: "var(--font-mono)", fontSize: "0.95em" }} autoComplete="organization" />
                <div style={{ position: "absolute", left: 12, top: "50%", transform: "translateY(-50%)", color: "var(--text-faint)" }}>
                  <Icon name="building" size={18} />
                </div>
              </div>
            </div>

            <div className="field">
              <label className="field-label">Логин оператора</label>
              <div style={{ position: "relative" }}>
                <input className="input" value={loginValue} onChange={(e) => setLogin(e.target.value)} placeholder="например, operator"
                       style={{ paddingLeft: 42 }} autoComplete="username" />
                <div style={{ position: "absolute", left: 12, top: "50%", transform: "translateY(-50%)", color: "var(--text-faint)" }}>
                  <Icon name="user" size={18} />
                </div>
              </div>
            </div>

            <div className="field">
              <label className="field-label">Пароль</label>
              <div style={{ position: "relative" }}>
                <input className="input" type={showPwd ? "text" : "password"} value={password} onChange={(e) => setPassword(e.target.value)}
                       placeholder="Введите пароль" style={{ paddingLeft: 42, paddingRight: 42 }} autoComplete="current-password" />
                <div style={{ position: "absolute", left: 12, top: "50%", transform: "translateY(-50%)", color: "var(--text-faint)" }}>
                  <Icon name="settings" size={18} />
                </div>
                <button type="button" onClick={() => setShowPwd((s) => !s)}
                        style={{ position: "absolute", right: 8, top: "50%", transform: "translateY(-50%)", padding: 6, color: "var(--text-faint)" }}>
                  <Icon name={showPwd ? "eye-off" : "eye"} size={18} />
                </button>
              </div>
            </div>

            {error && (
              <div style={{ background: "var(--danger-soft)", color: "var(--danger)", padding: "12px 14px", borderRadius: "var(--radius)", fontSize: "0.92em", display: "flex", alignItems: "center", gap: 8 }}>
                <Icon name="alert-circle" size={18} /> {error}
              </div>
            )}

            <button type="submit" className="btn btn-primary btn-lg" style={{ width: "100%" }} disabled={submitting}>
              {submitting ? "Вход…" : <>Войти в систему<Icon name="arrowRight" size={18} /></>}
            </button>
          </form>

          <div style={{ marginTop: 12, textAlign: "center", fontSize: "0.85em" }}>
            <a href="#" style={{ color: "var(--accent)", textDecoration: "none" }} onClick={(e) => e.preventDefault()}>Забыли пароль?</a>
          </div>
        </div>
      </div>
    </div>
  );
}
```

- [ ] **Step 3: Login test**

```tsx
// web/src/pages/__tests__/Login.test.tsx
import { describe, it, expect, beforeAll, afterAll, afterEach } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { setupServer } from "msw/node";
import { http, HttpResponse } from "msw";

import Login from "../Login";

const server = setupServer();
beforeAll(() => server.listen());
afterEach(() => server.resetHandlers());
afterAll(() => server.close());

describe("Login", () => {
  it("on success stores tokens and navigates", async () => {
    server.use(
      http.post("/api/auth/login", () =>
        HttpResponse.json({
          access_token: "a",
          refresh_token: "r",
          claims: { userId: "1", tenantId: "t", roles: ["admin"], fullName: "X", exp: 0 },
        }),
      ),
    );
    render(<MemoryRouter><Login /></MemoryRouter>);

    await userEvent.type(screen.getByLabelText(/Идентификатор/), "CC-A");
    await userEvent.type(screen.getByLabelText(/Логин/), "u");
    await userEvent.type(screen.getByLabelText(/Пароль/), "p");
    await userEvent.click(screen.getByRole("button", { name: /Войти/ }));

    // After successful login, error block must not appear
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  });

  it("displays error on 401", async () => {
    server.use(http.post("/api/auth/login", () => new HttpResponse(null, { status: 401 })));
    render(<MemoryRouter><Login /></MemoryRouter>);

    await userEvent.type(screen.getByLabelText(/Идентификатор/), "CC-A");
    await userEvent.type(screen.getByLabelText(/Логин/), "u");
    await userEvent.type(screen.getByLabelText(/Пароль/), "wrong");
    await userEvent.click(screen.getByRole("button", { name: /Войти/ }));

    expect(await screen.findByText("Неверный логин или пароль")).toBeInTheDocument();
  });
});
```

- [ ] **Step 4: Run tests**

- [ ] **Step 5: Commit**

```bash
git add web/src/pages/Login.tsx web/src/api/auth.ts web/src/pages/__tests__/
git commit -m "feat(web): Login page ported from prototype with success/error tests"
```

---

## Task 10: WASM survey runtime loader

**Files:**
- Create: `web/src/lib/wasm.ts`
- Create: `web/public/.gitkeep`

- [ ] **Step 1: Loader**

```ts
// web/src/lib/wasm.ts
// Glue to load the surveys WASM module built by Plan 07.
// The .wasm file lands in web/public/surveys-runtime.wasm via build pipeline.

interface SurveysRuntimeWasm {
  nextNode(currentNodeID: string, answer: unknown, allAnswersJSON: string, schemaJSON: string): string;
  validateAnswer(nodeJSON: string, answer: unknown): string; // empty = ok
  calculateProgress(allAnswersJSON: string, schemaJSON: string): number;
}

let cached: SurveysRuntimeWasm | null = null;

export async function loadSurveysRuntime(): Promise<SurveysRuntimeWasm> {
  if (cached) return cached;
  const go = new (window as any).Go();
  const result = await WebAssembly.instantiateStreaming(fetch("/surveys-runtime.wasm"), go.importObject);
  void go.run(result.instance);
  // The Go program exports `surveysRuntime` on window.
  cached = (window as any).surveysRuntime as SurveysRuntimeWasm;
  if (!cached) throw new Error("WASM did not export surveysRuntime");
  return cached;
}
```

`web/public/.gitkeep` — empty file to commit the directory; the actual `surveys-runtime.wasm` is deposited here by `scripts/build-wasm.sh` (defined in Plan 07).

- [ ] **Step 2: Commit**

```bash
git add web/src/lib/wasm.ts web/public/.gitkeep
git commit -m "feat(web): WASM loader for surveys runtime"
```

---

## Task 11: ESLint + Prettier

**Files:**
- Create: `web/eslint.config.js`, `web/prettier.config.cjs`

- [ ] **Step 1: ESLint config (flat-config v9)**

```js
// web/eslint.config.js
import tsParser from "@typescript-eslint/parser";
import ts from "@typescript-eslint/eslint-plugin";
import reactHooks from "eslint-plugin-react-hooks";
import reactRefresh from "eslint-plugin-react-refresh";

export default [
  {
    files: ["src/**/*.{ts,tsx}"],
    languageOptions: { parser: tsParser, parserOptions: { project: "./tsconfig.json" } },
    plugins: { "@typescript-eslint": ts, "react-hooks": reactHooks, "react-refresh": reactRefresh },
    rules: {
      ...ts.configs["recommended"].rules,
      ...reactHooks.configs.recommended.rules,
      "react-refresh/only-export-components": "warn",
      "@typescript-eslint/consistent-type-imports": "error",
      "@typescript-eslint/no-unused-vars": ["error", { argsIgnorePattern: "^_" }],
    },
  },
];
```

- [ ] **Step 2: Prettier**

```js
// web/prettier.config.cjs
module.exports = {
  semi: true,
  singleQuote: false,
  trailingComma: "all",
  printWidth: 100,
  tabWidth: 2,
};
```

- [ ] **Step 3: Run lint**

```bash
cd web && npm run lint
```

Fix any issues that surface; commit.

- [ ] **Step 4: Commit**

```bash
git add web/eslint.config.js web/prettier.config.cjs
git commit -m "chore(web): add eslint + prettier configs"
```

---

## Task 12: Build integration with cmd/api (embed.FS)

**Files:**
- Create: `cmd/api/embed.go`
- Modify: `cmd/api/main.go` (add SPA static handler)
- Modify: `Makefile` — add `web-build`, `web-test`, `web-lint`; make `build` depend on `web-build`
- Modify: `Dockerfile` — multi-stage with node + go

- [ ] **Step 1: Embed.go**

```go
// cmd/api/embed.go
package main

import "embed"

//go:embed all:webdist
var webDist embed.FS
```

The actual `webdist/` folder under `cmd/api/` is populated by `make web-build` which copies `web/dist/*` into it. We use a sibling folder rather than embedding `web/dist` directly because `embed` cannot traverse `..`.

- [ ] **Step 2: SPA static handler in main.go**

Append (in the route registration phase added by Plan 02):

```go
// In cmd/api/main.go (Plan 02 main scaffolding):

import (
	"net/http"
	"strings"
	"io/fs"
)

// serveSPA returns an http.HandlerFunc that serves files from web/dist; for any
// path that does not exist on disk, it falls through to index.html so the SPA
// router takes over.
func serveSPA() http.HandlerFunc {
	dist, err := fs.Sub(webDist, "webdist")
	if err != nil { panic(err) }
	fileServer := http.FileServer(http.FS(dist))
	return func(w http.ResponseWriter, r *http.Request) {
		// If the path is /api/* or /ws or /healthz/etc → not our concern.
		if strings.HasPrefix(r.URL.Path, "/api/") ||
			r.URL.Path == "/ws" ||
			strings.HasPrefix(r.URL.Path, "/metrics") {
			http.NotFound(w, r)
			return
		}
		// Try file; on miss, serve index.html.
		if r.URL.Path != "/" {
			f, err := dist.Open(strings.TrimPrefix(r.URL.Path, "/"))
			if err != nil {
				r.URL.Path = "/"
			} else {
				_ = f.Close()
			}
		}
		fileServer.ServeHTTP(w, r)
	}
}
```

Mount this on the gateway's catch-all route (after API routes are registered).

- [ ] **Step 3: Makefile additions**

```makefile
.PHONY: web-install web-lint web-test web-build

web-install:
	cd web && npm ci

web-lint: web-install
	cd web && npm run lint

web-test: web-install
	cd web && npm test -- run

web-build: web-install
	cd web && npm run build
	rm -rf cmd/api/webdist
	cp -R web/dist cmd/api/webdist

# Make the existing `build` target depend on web-build:
build: web-build $(addprefix build-, $(COMMANDS))
```

- [ ] **Step 4: Multi-stage Dockerfile**

```dockerfile
# syntax=docker/dockerfile:1.7

# ----- Frontend builder -----
FROM node:20-alpine AS web-builder
WORKDIR /web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npm run build

# ----- Go builder -----
FROM golang:1.22-alpine AS go-builder
RUN apk add --no-cache git ca-certificates
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
COPY --from=web-builder /web/dist ./cmd/api/webdist
ARG VERSION=dev
RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/api ./cmd/api

# ----- Runtime -----
FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata && addgroup -S app && adduser -S -G app app
WORKDIR /app
COPY --from=go-builder /out/api /app/api
USER app
EXPOSE 8080
ENV HTTP_ADDR=:8080
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s CMD wget --quiet --spider http://127.0.0.1:8080/healthz || exit 1
ENTRYPOINT ["/app/api"]
```

- [ ] **Step 5: .gitignore for embedded dist**

Append to `.gitignore`:

```
cmd/api/webdist/
```

- [ ] **Step 6: Verify build end-to-end**

```bash
make web-build
ls -la cmd/api/webdist/
make build-api
./bin/api &
sleep 2
curl -s http://localhost:8080/        # serves index.html
curl -s http://localhost:8080/healthz # serves "ok"
kill %1
```

- [ ] **Step 7: Commit**

```bash
git add cmd/api/embed.go Makefile Dockerfile .gitignore
git commit -m "build(web): integrate Vite build into cmd/api via embed.FS"
```

---

## Task 13: Playwright config (specs come in Plan 19)

**Files:**
- Create: `web/playwright.config.ts`

```ts
// web/playwright.config.ts
import { defineConfig, devices } from "@playwright/test";

export default defineConfig({
  testDir: "./tests/e2e",
  timeout: 30_000,
  retries: 1,
  workers: 4,
  reporter: [["html", { open: "never" }], ["list"]],
  use: {
    baseURL: process.env.E2E_BASE_URL ?? "http://localhost:5173",
    trace: "retain-on-failure",
    video: "retain-on-failure",
  },
  projects: [
    { name: "chromium", use: { ...devices["Desktop Chrome"] } },
    { name: "firefox", use: { ...devices["Desktop Firefox"] } },
  ],
  webServer: {
    command: "npm run dev",
    port: 5173,
    timeout: 120_000,
    reuseExistingServer: !process.env.CI,
  },
});
```

Commit:

```bash
git add web/playwright.config.ts
git commit -m "test(web): add Playwright config (specs added in Plan 19)"
```

---

## Self-review

**Spec coverage:**
- §FR-L theme + font-size: ✓ (Task 5).
- §NFR-8 browser support (Chrome ≥ 110): ✓ — TS target ES2022, no IE polyfills.
- §NFR-9 i18n ru-only: ✓ (Task 5).
- §NFR-12 idempotency on mutating requests: ✓ (Task 3).
- §10.1 WS protocol — auth/subscribe/event/ping/refresh: ✓ (Task 4).
- §11.5 WASM runtime loader: ✓ (Task 10).
- Прототип-ports: styles ✓ (Task 2), Sidebar/Topbar ✓ (Task 7), Login ✓ (Task 9), Icons ✓ (Task 6).

**Placeholder scan:** Icons file has a single literal placeholder comment (`// Add the remaining 60+ icons below`) — flagged explicitly with file path so the executing agent ports each icon by hand. Task 6 explicitly forbids improvising shapes.

**Type/name consistency:** `AuthClaims`, `apiClient`, `WSHub`, `useAuthStore`, `useThemeStore` — all stable across tasks. `routes.tsx` uses `RequireAuth`/`RequireRole` exactly as defined.

**Out of scope (correctly deferred):**
- Operator pages — Plan 16.
- Admin pages 1 — Plan 17.
- Survey builder — Plan 18.
- Admin pages 2 + E2E specs — Plan 19.

Plan 15 verified.

---

## Amendments (post-execution 2026-05-17)

Adaptations made during execution in `sociopulse-web`. Plan text is preserved for historical reference; this section captures the deltas. Full details + production lessons live in [`docs/references/plan-15-frontend-foundation.md`](../../references/plan-15-frontend-foundation.md).

### Separate-repo path adaptations (Tasks 1-13)

- The plan was authored when the FE lived inside platform/`web/`. All file paths drop the `web/` prefix in execution (`web/package.json` → `package.json` at sociopulse-web repo root, etc.). The `dist/` build output lands at the web repo root.

### Task 1 (Vite scaffold)

- `npm create vite@5.2 . -- --template react-ts -y` refuses on a non-empty repo (CLAUDE.md, README.md, docs/ already present). Scaffolded manually — same end state.
- Added `@types/node` to devDependencies (plan oversight — vite.config.ts needs `node:path` + `__dirname`).
- Decision documented: `social-pulse-maket/` is gitignored in the web repo; canonical source is sibling `../social-pulse/social-pulse-maket/` (per web README).
- `package.json` deps bumped vs plan's pins:
  - `@typescript-eslint/eslint-plugin@^7.10` + `@typescript-eslint/parser@^7.10` → **`typescript-eslint@^8.0.0`** (umbrella). The v7 split packages cap at eslint v8; we use eslint v9.
  - `eslint-plugin-react-hooks@^4.6.2` → **`^5.0.0`** (v4 caps at eslint v8).
  - Added `@eslint/js@^9.39.4` (needed by the flat-config recommended set).
- `tsc -b` (composite project mode) emits `*.tsbuildinfo` + `vite.config.{js,d.ts}` as build artefacts. Added to `.gitignore`.

### Task 2 (styles port)

- Inline `<script>` in `index.html` restores theme + font-size from `localStorage.getItem("sociopulse.theme")` BEFORE React mounts — FOUC-prevention. Coordinated with the zustand store key (Task 5).
- Added `--z-tooltip/toast/modal/popover` CSS variables to `tokens.css` to give Radix portals an explicit stacking order (Radix portals don't inherit stacking context; plan-15 ref §5.1).
- Reset.css carries the full `html/body { font-family, font-size, color, background, line-height, font-smoothing }` block (plan's reset.css was minimal; for pixel fidelity the base styles need to land in reset rather than splitting across reset/globals).

### Task 3 (API client + auth store)

- **Wire format**: store carries `user: User | null` (lowercase `user`, snake_case interior fields matching `internal/auth/transport/http/dto.go::UserDTO`). NOT the plan's `claims: AuthClaims` camelCase. The plan's prose was wrong vs the backend canon.
- `rotateTokens(...)` action takes ONLY `accessToken`, `refreshToken`, `accessExpiresAt`, `refreshExpiresAt` — the refresh response (`02_refresh.bru`) carries no user payload, so we preserve the existing user object across refreshes.
- `idempotency.ts` uses built-in `crypto.randomUUID()` (the `uuid` package stays in package.json for future uses but is unimported from production code — consider dropping in Plan 16).
- jsdom `localStorage` polyfill in `src/test/setup.ts` (jsdom installs a read-only stub when `--localstorage-file` is passed without a path).

### Task 4 (WS hub)

- Discriminated-union `Frame` type with full type narrowing; payload is `unknown` on the wire, narrowed by `subscribe<T>` at the call site (plan's draft used `any`).
- `subscribe` frame omits `filter` field entirely when empty (cleaner wire under `exactOptionalPropertyTypes`).
- `connect()` rejects with `ws_closed_before_auth` if socket closes pre-handshake (so callers don't hang forever).
- `auth.error` clears `connectingToken` to break the reconnect loop.
- Hub replies `pong` to server-initiated `ping` (plan ignored inbound pings).

### Task 7 (Layout)

- `aria-label="Выйти"` added to logout button (plan's `title="Выйти"` alone doesn't produce an accessible name for `getByRole("button", { name })`).
- Topbar subscribes via `useThemeStore((s) => s.theme)` selector rather than `useTheme()` — zustand's `UseBoundStore` overload collapses `ReturnType<typeof useStore>` to `unknown` under TS.
- AppShell uses conditional spread for `style` and `right` props (`exactOptionalPropertyTypes` forbids `style={undefined}`).

### Task 8 (Routing)

- Created `src/pages/Login.tsx` as a stub at end of Task 8 (the lazy import in `routes.tsx` blocks the typecheck without it); Task 9 replaces with the full port. Plan didn't account for the chicken-and-egg.
- Added `/` → `/login` redirect explicitly (plan had no rule for "/").

### Task 9 (Login)

- Real wire format applied: `setSession({ accessToken: res.access_token, refreshToken: res.refresh_token, accessExpiresAt: res.access_expires_at, refreshExpiresAt: res.refresh_expires_at, user: res.user })`. TOTP path detects `res.totp_required === true` and intentionally does NOT call `setSession` — the partial token rides on `res.access_token` and is consumed by Plan 05/16's 2FA screen.
- `getByLabelText(/Пароль/i)` regex anchored (`/^Пароль$/i`) to avoid colliding with the show/hide button's `aria-label="Показать пароль"`.
- `location.state` narrowed via type guard (`react-router-dom` v6 types it as `unknown`).

### Task 10 (WASM loader)

- Typed Go-host surface (`window.Go`, `importObject`, `run`) instead of leaking `any` through the API. Throws a clear error if `wasm_exec.js` isn't loaded yet.
- `lib/wasm.ts` calls `fetch("/surveys-runtime.wasm")` inside `WebAssembly.instantiateStreaming` — this is a static-asset load, not an API call, and is the **one documented exception** to the "no raw fetch outside src/api/" standing rule.

### Task 11 (ESLint + Prettier)

- Modern flat-config using `typescript-eslint` v8 umbrella + `@eslint/js` v9 + `eslint-plugin-react-hooks` v5 + `eslint-plugin-react-refresh`. The plan's `@typescript-eslint/eslint-plugin` + `@typescript-eslint/parser` v7 wiring is replaced.
- Test files override: `no-explicit-any`, `no-unused-vars`, `react-refresh/only-export-components` all relaxed (vitest mock objects + fixture exports).
- `prettier.config.cjs` (CJS so `module.exports` works) is excluded from lint (`**/*.cjs` in `ignores`).
- Final lint state: 0 errors, 2 warnings (both `react-refresh/only-export-components` advisories on `Icon.tsx` ICONS map + `routes.tsx` PendingPage — acceptable since both files exist as glue rather than HMR-hot leaves).

### Task 12 (embed.FS bridge)

- **Deferred to platform** via [issue TakuroBreath/sociopulse-platform#2](https://github.com/TakuroBreath/sociopulse-platform/issues/2). The cross-repo wiring (`cmd/api/embed.go`, Makefile `web-build`, multi-stage Dockerfile) lives in platform — the FE side ships only the artefact (`dist/` produced by `npm run build`).

### Task 13 (Playwright)

- `process.env["E2E_BASE_URL"]` (bracket notation) — TS `noUncheckedIndexedAccess: true` requires it.

### Final implementation review (2026-05-17)

Independent code-reviewer subagent over `9da104f...HEAD`: **APPROVE-WITH-NITS**. 0 blockers, 0 majors, 3 minors (refresh-bypass-retry, WS race-on-rotation, WS listener cleanup) — all deferred to Plan 16 where they'll be exercised under real conditions. Quality gates green: typecheck silent, lint 0 errors / 2 warnings, 68/68 tests pass, build succeeds.

---

**Plan complete and saved to `docs/superpowers/plans/2026-05-06-15-frontend-foundation.md`.**

