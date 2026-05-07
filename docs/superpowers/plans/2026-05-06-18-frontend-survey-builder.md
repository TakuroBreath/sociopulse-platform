# Frontend Survey Builder Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build the admin-side survey builder in `web/` — a list page (`AdminSurveys`), a wrapper (`SurveyBuilder`), and two synchronized editors (`FormBuilder` linear-list + `FlowBuilder` graph-canvas) that read and write the same JSON schema. Both modes back the same zustand store; per-keystroke debounced validation hits the backend; "save version" creates a new immutable revision; "preview" opens a runtime-driven simulator. Source of truth — schema described in spec §11. Visual reference — prototype `social-pulse-maket/project/surveys.jsx` and `styles.css` (`.flow-canvas`, `.flow-node`, `.q-*`).

**Architecture:** Code lives in `web/src/`. Pages: `pages/admin/Surveys.tsx`, `pages/admin/SurveyBuilder.tsx`, `pages/admin/SurveyPreview.tsx`. Components: `components/survey-builder/{FormBuilder,FlowBuilder,QuestionPalette,SchemaPropsPanel,DslInput,FlowCanvas,FlowNode,FlowEdge,SaveVersionModal}.tsx`. State: `stores/surveyStore.ts` (zustand). API: `api/surveys.ts`. Hooks: `hooks/useDebouncedValidation.ts`, `hooks/useFlowDrag.ts`, `hooks/useFlowAutoLayout.ts`. Schema types: `types/survey.ts` mirroring backend (Plan 07). Tests in `__tests__/` siblings, plus `tests/e2e/admin-surveys.spec.ts` for Playwright (smoke).

**Tech Stack:** React 18.3+, TypeScript 5.4+, Vite (Plan 15), zustand 4.5+, `@dnd-kit/core` 6.1+ + `@dnd-kit/sortable` 8.0+ + `@dnd-kit/utilities`, `@codemirror/view` + `@codemirror/state` + `@codemirror/language` + `@lezer/highlight` 6+ (DSL syntax-highlight), `@tanstack/react-query` 5+ (already from Plan 15), `vitest` + `@testing-library/react` + `@testing-library/user-event` + `msw` 2+ (existing), `nanoid` for node IDs.

**Spec sections covered:** §11 (universal schema, form vs flow, DSL, validation, runtime, versioning, preview), FR-C1…FR-C10.

**Prerequisites:**
- Plan 07: `surveys` backend module — `GET /api/surveys`, `GET /api/surveys/:id`, `POST /api/surveys/:id/validate`, `POST /api/surveys/:id/versions`, `POST /api/surveys/:id/versions/:vid/activate`, `GET /api/surveys/:id/versions`. Schema JSON contract identical to spec §11.1. WASM runtime artifact published at `/static/surveys-runtime.wasm` with TS glue under `web/src/runtime/surveys-runtime.ts`.
- Plan 15: `web/` Vite project, React Router, design tokens (`var(--accent)`, `var(--bg-card)`, …), `Layout`, `Icon`, `Card`, `Button`, `Modal`, `Field`, base `apiClient` with auth headers, `tenant-settings` provider exposing `builderMode: 'form'|'flow'`, MSW for tests, `__tests__/setup.ts` boilerplate, `tests/e2e/` Playwright scaffold.
- The prototype files `SocioPulse.html`, `social-pulse-maket/project/surveys.jsx`, `social-pulse-maket/project/styles.css` already in repo (read-only reference).

---

## File Structure

```
web/
├── package.json                                  # +deps below
└── src/
    ├── api/
    │   └── surveys.ts                            # REST client for /api/surveys/*
    ├── types/
    │   └── survey.ts                             # Schema, Node, Edge, Option, ValidationError
    ├── stores/
    │   ├── surveyStore.ts                        # zustand store (single source of truth)
    │   └── __tests__/
    │       └── surveyStore.test.ts               # 30+ state-machine cases
    ├── hooks/
    │   ├── useDebouncedValidation.ts             # 300ms debounce → POST /validate
    │   ├── useFlowDrag.ts                        # mouse-driven node move
    │   ├── useFlowAutoLayout.ts                  # Sugiyama-lite for nodes without ui.x/y
    │   └── __tests__/
    │       ├── useDebouncedValidation.test.ts
    │       ├── useFlowDrag.test.ts
    │       └── useFlowAutoLayout.test.ts
    ├── components/
    │   └── survey-builder/
    │       ├── index.ts                          # public re-exports
    │       ├── FormBuilder.tsx                   # 3-col list/edit/palette
    │       ├── FlowBuilder.tsx                   # 3-col palette/canvas/props
    │       ├── FlowCanvas.tsx                    # SVG edges + abs-positioned nodes
    │       ├── FlowNode.tsx                      # one node card
    │       ├── FlowEdge.tsx                      # one cubic-bezier path
    │       ├── QuestionPalette.tsx               # form-mode "add question" 6 buttons
    │       ├── NodePalette.tsx                   # flow-mode 6 node-type buttons
    │       ├── StructureList.tsx                 # flow-mode left list of nodes
    │       ├── SchemaPropsPanel.tsx              # right-bottom card in form-mode
    │       ├── NodePropsPanel.tsx                # right card in flow-mode
    │       ├── DslInput.tsx                      # CodeMirror-backed condition editor
    │       ├── SortableQuestionItem.tsx          # @dnd-kit/sortable wrapper
    │       ├── SortableOptionItem.tsx            # for option list reorder
    │       ├── ValidationBadge.tsx               # red dot/tooltip for errors on a node
    │       ├── SaveVersionModal.tsx              # major/minor + label modal
    │       └── __tests__/
    │           ├── FormBuilder.test.tsx
    │           ├── FlowBuilder.test.tsx
    │           ├── FlowCanvas.test.tsx
    │           ├── DslInput.test.tsx
    │           ├── SortableQuestionItem.test.tsx
    │           ├── SaveVersionModal.test.tsx
    │           └── ValidationBadge.test.tsx
    ├── pages/
    │   └── admin/
    │       ├── Surveys.tsx                       # list page (AdminSurveys)
    │       ├── SurveyBuilder.tsx                 # wrapper page with mode tabs
    │       ├── SurveyPreview.tsx                 # /surveys/:id/preview tab
    │       └── __tests__/
    │           ├── Surveys.test.tsx
    │           ├── SurveyBuilder.test.tsx
    │           └── SurveyPreview.test.tsx
    └── styles/
        └── survey-builder.css                    # additions for builder-only classes
tests/
└── e2e/
    └── admin-surveys.spec.ts                     # Playwright smoke (list → open → save)
```

---

## Task 1: Pin frontend dependencies

**Files:**
- Modify: `web/package.json`
- Modify: `web/package-lock.json` (regenerated)

- [ ] **Step 1: Verify Plan 15 prerequisites are present**

```bash
cd web
node -e "const p=require('./package.json'); for (const d of ['react','react-dom','react-router-dom','@tanstack/react-query','zustand','msw']) if (!(d in (p.dependencies||{}))) { console.error('missing', d); process.exit(1) }"
```

Expected: no output. If any fails, finish Plan 15 first.

- [ ] **Step 2: Add new dependencies**

```bash
cd web
npm install --save \
  @dnd-kit/core@^6.1.0 \
  @dnd-kit/sortable@^8.0.0 \
  @dnd-kit/utilities@^3.2.2 \
  @codemirror/view@^6.26.0 \
  @codemirror/state@^6.4.0 \
  @codemirror/language@^6.10.0 \
  @lezer/highlight@^1.2.0 \
  @lezer/lr@^1.4.0 \
  nanoid@^5.0.7
npm install --save-dev \
  @types/node
```

- [ ] **Step 3: Verify versions in package.json**

Run: `node -e "const p=require('./web/package.json'); console.log(JSON.stringify({deps:p.dependencies,dev:p.devDependencies},null,2))"`
Expected: `@dnd-kit/core` ≥ 6.1.0, `@dnd-kit/sortable` ≥ 8.0.0, zustand ≥ 4.5, the four `@codemirror/*` packages present, `nanoid` ≥ 5.0.

- [ ] **Step 4: Commit**

```bash
git add web/package.json web/package-lock.json
git commit -m "build(web): pin survey-builder deps (@dnd-kit, codemirror, nanoid)"
```

---

## Task 2: Schema types

**Files:**
- Create: `web/src/types/survey.ts`

- [ ] **Step 1: Write `web/src/types/survey.ts`**

```ts
// web/src/types/survey.ts
//
// Universal survey schema — mirrors backend `surveys.Schema` (Plan 07, spec §11.1).
// Exactly one shape — both Form and Flow builders read/write this.
//
// Keep field names in sync with the backend JSON tags. When in doubt, run the
// `surveys.SchemaSnapshot` JSON-schema export (Plan 07) and diff with this file.

export type NodeKind =
  | 'start'         // implicit entry; exactly one
  | 'intro'         // operator reads a script, no answer
  | 'question'      // a single question; `type` discriminates
  | 'text-block'    // info shown to operator, no answer
  | 'condition'     // pure routing node, evaluates `next.when` only
  | 'jump'          // explicit jump for clarity in flow-mode
  | 'success-end'
  | 'refusal-end';

export type QuestionType = 'single' | 'multi' | 'number' | 'text' | 'select';

export interface Option {
  id: string;
  label: string;
}

export interface Edge {
  to: string;
  when: string; // DSL expression; "true" — always
  label?: string;
}

export interface UI {
  x: number;
  y: number;
}

export interface BaseNode {
  id: string;
  kind: NodeKind;
  text?: string;
  hint?: string;
  next: Edge[];
  ui?: UI;
}

export interface QuestionNode extends BaseNode {
  kind: 'question';
  type: QuestionType;
  required?: boolean;
  options?: Option[];
  min?: number;
  max?: number;
}

export type Node = BaseNode | QuestionNode;

export interface Metadata {
  estimated_minutes?: string;
  max_questions?: number;
  primary_mode?: 'form' | 'flow';
  cost_per_survey?: number;
  min_duration_seconds?: number;
}

export interface Schema {
  version: string;        // e.g. "1.1" — semver of the SCHEMA grammar, not the survey version
  title: string;
  intro?: string;
  nodes: Node[];
  metadata?: Metadata;
}

// ---- API DTOs ----

export interface SurveyListItem {
  id: string;
  name: string;
  project_code: string;
  questions: number;
  active_version: string;     // "v3.2"
  updated_at: string;         // ISO-8601
  status: 'active' | 'paused' | 'archived';
}

export interface SurveyDetail {
  id: string;
  name: string;
  project_code: string;
  active_version_id: string | null;
  draft_schema: Schema;
  versions: SurveyVersionMeta[];
  status: 'active' | 'paused' | 'archived';
}

export interface SurveyVersionMeta {
  id: string;
  label: string;          // "v3.2"
  major: number;
  minor: number;
  is_active: boolean;
  created_at: string;
  created_by: string;
  notes: string;
}

export interface ValidationError {
  node_id?: string;       // omitted means schema-level (e.g. multiple starts)
  edge_index?: number;    // 0-based within node.next
  field?: string;         // "text" | "options" | "next.when" | …
  code: string;           // "unreachable" | "dangling-edge" | "dsl-syntax" | …
  message: string;
}

export interface ValidationResult {
  ok: boolean;
  errors: ValidationError[];
}

export interface SaveVersionRequest {
  schema: Schema;
  bump: 'major' | 'minor';
  notes: string;
}
```

- [ ] **Step 2: Smoke-compile**

```bash
cd web && npx tsc --noEmit src/types/survey.ts
```

Expected: no output (success). If lib targets disagree (e.g. `--isolatedModules`), copy whatever target Plan 15 used — this file should compile with the same `tsconfig.json`.

- [ ] **Step 3: Commit**

```bash
git add web/src/types/survey.ts
git commit -m "feat(web/surveys): add schema and DTO types mirroring backend"
```

---

## Task 3: REST client

**Files:**
- Create: `web/src/api/surveys.ts`
- Create: `web/src/api/__tests__/surveys.test.ts`

- [ ] **Step 1: Write the failing test**

Create `web/src/api/__tests__/surveys.test.ts`:

```ts
import { describe, it, expect, beforeAll, afterAll, afterEach } from 'vitest';
import { setupServer } from 'msw/node';
import { http, HttpResponse } from 'msw';
import {
  listSurveys,
  fetchSurvey,
  validateSchema,
  saveVersion,
  activateVersion,
} from '../surveys';
import type { Schema } from '../../types/survey';

const minimalSchema: Schema = {
  version: '1.1',
  title: 'T',
  nodes: [
    { id: 'n1', kind: 'intro', text: 'hi', next: [{ to: 'end', when: 'true' }] },
    { id: 'end', kind: 'success-end', next: [] },
  ],
};

const server = setupServer(
  http.get('/api/surveys', () =>
    HttpResponse.json([
      { id: 's1', name: 'A', project_code: 'P', questions: 2, active_version: 'v1.0', updated_at: '2026-05-01T00:00:00Z', status: 'active' },
    ])
  ),
  http.get('/api/surveys/s1', () =>
    HttpResponse.json({
      id: 's1', name: 'A', project_code: 'P', active_version_id: null,
      draft_schema: minimalSchema, versions: [], status: 'active',
    })
  ),
  http.post('/api/surveys/s1/validate', async ({ request }) => {
    const body = (await request.json()) as { schema: Schema };
    if (body.schema.nodes.length === 0) {
      return HttpResponse.json({ ok: false, errors: [{ code: 'empty', message: 'at least one node' }] });
    }
    return HttpResponse.json({ ok: true, errors: [] });
  }),
  http.post('/api/surveys/s1/versions', () =>
    HttpResponse.json({ id: 'v1', label: 'v1.1', major: 1, minor: 1, is_active: false, created_at: '', created_by: 'u', notes: '' })
  ),
  http.post('/api/surveys/s1/versions/v1/activate', () => new HttpResponse(null, { status: 204 }))
);

beforeAll(() => server.listen());
afterEach(() => server.resetHandlers());
afterAll(() => server.close());

describe('surveys API client', () => {
  it('listSurveys returns rows', async () => {
    const rows = await listSurveys();
    expect(rows).toHaveLength(1);
    expect(rows[0].id).toBe('s1');
  });

  it('fetchSurvey returns detail with draft schema', async () => {
    const s = await fetchSurvey('s1');
    expect(s.draft_schema.nodes).toHaveLength(2);
  });

  it('validateSchema reports errors', async () => {
    const res = await validateSchema('s1', { ...minimalSchema, nodes: [] });
    expect(res.ok).toBe(false);
    expect(res.errors[0].code).toBe('empty');
  });

  it('saveVersion returns new version meta', async () => {
    const v = await saveVersion('s1', { schema: minimalSchema, bump: 'minor', notes: 'foo' });
    expect(v.label).toBe('v1.1');
  });

  it('activateVersion returns 204', async () => {
    await expect(activateVersion('s1', 'v1')).resolves.toBeUndefined();
  });
});
```

Run: `cd web && npx vitest run src/api/__tests__/surveys.test.ts`
Expected: fails with `Cannot find module '../surveys'`. That's the failing state.

- [ ] **Step 2: Implement `web/src/api/surveys.ts`**

```ts
// web/src/api/surveys.ts
import { apiClient } from './client'; // from Plan 15: fetch wrapper with auth + tenant headers
import type {
  SurveyListItem,
  SurveyDetail,
  Schema,
  ValidationResult,
  SaveVersionRequest,
  SurveyVersionMeta,
} from '../types/survey';

export async function listSurveys(): Promise<SurveyListItem[]> {
  return apiClient.get<SurveyListItem[]>('/api/surveys');
}

export async function fetchSurvey(id: string): Promise<SurveyDetail> {
  return apiClient.get<SurveyDetail>(`/api/surveys/${id}`);
}

export async function validateSchema(id: string, schema: Schema): Promise<ValidationResult> {
  return apiClient.post<ValidationResult>(`/api/surveys/${id}/validate`, { schema });
}

export async function saveVersion(id: string, body: SaveVersionRequest): Promise<SurveyVersionMeta> {
  return apiClient.post<SurveyVersionMeta>(`/api/surveys/${id}/versions`, body);
}

export async function activateVersion(id: string, versionId: string): Promise<void> {
  await apiClient.post<void>(`/api/surveys/${id}/versions/${versionId}/activate`, {});
}

export async function listVersions(id: string): Promise<SurveyVersionMeta[]> {
  return apiClient.get<SurveyVersionMeta[]>(`/api/surveys/${id}/versions`);
}
```

If `apiClient` is named differently in Plan 15 (`http`, `request`, `apiFetch`), use that — the contract is `get<T>` / `post<T>` taking a URL and returning the parsed JSON.

- [ ] **Step 3: Run tests — they must pass**

```bash
cd web && npx vitest run src/api/__tests__/surveys.test.ts
```

Expected: 5 passing.

- [ ] **Step 4: Commit**

```bash
git add web/src/api/surveys.ts web/src/api/__tests__/surveys.test.ts
git commit -m "feat(web/surveys): add REST client for /api/surveys/*"
```

---

## Task 4: zustand schema store (state machine)

**Files:**
- Create: `web/src/stores/surveyStore.ts`
- Create: `web/src/stores/__tests__/surveyStore.test.ts`

- [ ] **Step 1: Write the failing test (state-machine cases)**

Create `web/src/stores/__tests__/surveyStore.test.ts`:

```ts
import { describe, it, expect, beforeEach } from 'vitest';
import { createSurveyStore, blankSchema, type SurveyStore } from '../surveyStore';
import type { Schema, QuestionNode } from '../../types/survey';

const seedSchema = (): Schema => ({
  version: '1.1',
  title: 'T',
  nodes: [
    { id: 'n1', kind: 'intro', text: 'hello', next: [{ to: 'n2', when: 'true' }], ui: { x: 0, y: 0 } },
    {
      id: 'n2', kind: 'question', type: 'single', text: 'q?', required: true,
      options: [{ id: 'a', label: 'A' }, { id: 'b', label: 'B' }],
      next: [{ to: 'end', when: 'answer == "a"' }, { to: 'end', when: 'true' }],
      ui: { x: 0, y: 100 },
    } as QuestionNode,
    { id: 'end', kind: 'success-end', next: [], ui: { x: 0, y: 200 } },
  ],
});

let store: ReturnType<typeof createSurveyStore>;
const get = (): SurveyStore => store.getState();

beforeEach(() => {
  store = createSurveyStore();
  store.getState().setSchema(seedSchema());
});

describe('surveyStore — schema bootstrap', () => {
  it('blankSchema has start + success-end pair, both with ui', () => {
    const s = blankSchema();
    expect(s.nodes.find(n => n.kind === 'start')).toBeTruthy();
    expect(s.nodes.find(n => n.kind === 'success-end')).toBeTruthy();
    expect(s.nodes.every(n => n.ui)).toBe(true);
  });

  it('setSchema resets dirty=false and selection to first node', () => {
    store.getState().setSchema(seedSchema());
    expect(get().dirty).toBe(false);
    expect(get().selectedNodeId).toBe('n1');
  });
});

describe('surveyStore — addNode', () => {
  it('adds at provided coords with unique id and marks dirty', () => {
    const before = get().schema.nodes.length;
    const id = get().addNode({ kind: 'question', type: 'text', x: 200, y: 50 });
    expect(get().schema.nodes).toHaveLength(before + 1);
    const node = get().schema.nodes.find(n => n.id === id)!;
    expect(node.ui).toEqual({ x: 200, y: 50 });
    expect(get().dirty).toBe(true);
  });

  it('produces ids of form "n<number>" and never collides', () => {
    const ids = new Set<string>();
    for (let i = 0; i < 50; i++) ids.add(get().addNode({ kind: 'question', type: 'text', x: 0, y: 0 }));
    expect(ids.size).toBe(50);
    for (const id of ids) expect(id).toMatch(/^n\d+$/);
  });

  it('selects the newly added node', () => {
    const id = get().addNode({ kind: 'question', type: 'single', x: 0, y: 0 });
    expect(get().selectedNodeId).toBe(id);
  });

  it('question nodes get an empty options array by default for single/multi/select', () => {
    for (const t of ['single', 'multi', 'select'] as const) {
      const id = get().addNode({ kind: 'question', type: t, x: 0, y: 0 });
      const n = get().schema.nodes.find(n => n.id === id) as QuestionNode;
      expect(Array.isArray(n.options)).toBe(true);
    }
  });

  it('end nodes have empty next', () => {
    const id = get().addNode({ kind: 'refusal-end', x: 0, y: 0 });
    const n = get().schema.nodes.find(n => n.id === id)!;
    expect(n.next).toEqual([]);
  });
});

describe('surveyStore — removeNode', () => {
  it('removes node and dangling edges in one pass', () => {
    get().removeNode('n2');
    const ids = get().schema.nodes.map(n => n.id);
    expect(ids).not.toContain('n2');
    // edges referring to n2 are gone too:
    expect(get().schema.nodes.flatMap(n => n.next).find(e => e.to === 'n2')).toBeUndefined();
  });

  it('refuses to remove a start node and warns via store error field', () => {
    // seed includes an intro as first; add a real start:
    const startId = get().addNode({ kind: 'start', x: -100, y: 0 });
    expect(get().removeNode(startId)).toBe(false);
    expect(get().schema.nodes.find(n => n.id === startId)).toBeTruthy();
    expect(get().lastError).toContain('start');
  });

  it('clears selection if selected node is removed', () => {
    get().selectNode('n2');
    get().removeNode('n2');
    expect(get().selectedNodeId).not.toBe('n2');
  });
});

describe('surveyStore — updateNode', () => {
  it('shallow-merges patch into target node', () => {
    get().updateNode('n2', { text: 'updated' });
    expect(get().schema.nodes.find(n => n.id === 'n2')!.text).toBe('updated');
    expect(get().dirty).toBe(true);
  });

  it('renaming id propagates to all incoming edges', () => {
    get().updateNode('n2', { id: 'q_main' });
    const ids = get().schema.nodes.map(n => n.id);
    expect(ids).toContain('q_main');
    expect(ids).not.toContain('n2');
    const n1 = get().schema.nodes.find(n => n.id === 'n1')!;
    expect(n1.next.some(e => e.to === 'q_main')).toBe(true);
  });

  it('rejects rename to existing id', () => {
    expect(get().updateNode('n2', { id: 'n1' })).toBe(false);
    expect(get().schema.nodes.find(n => n.id === 'n2')).toBeTruthy();
    expect(get().lastError).toMatch(/duplicate id/i);
  });
});

describe('surveyStore — addEdge / removeEdge / updateEdge', () => {
  it('addEdge appends and marks dirty', () => {
    const before = get().schema.nodes.find(n => n.id === 'n1')!.next.length;
    get().addEdge('n1', { to: 'end', when: 'answer == "skip"' });
    expect(get().schema.nodes.find(n => n.id === 'n1')!.next).toHaveLength(before + 1);
  });

  it('removeEdge by index', () => {
    get().removeEdge('n2', 0);
    const next = get().schema.nodes.find(n => n.id === 'n2')!.next;
    expect(next).toHaveLength(1);
    expect(next[0].when).toBe('true');
  });

  it('updateEdge merges patch', () => {
    get().updateEdge('n2', 0, { when: 'answer == "b"' });
    expect(get().schema.nodes.find(n => n.id === 'n2')!.next[0].when).toBe('answer == "b"');
  });

  it('rejects edge to non-existent node', () => {
    expect(get().addEdge('n1', { to: 'nowhere', when: 'true' })).toBe(false);
    expect(get().lastError).toMatch(/unknown node/i);
  });
});

describe('surveyStore — option helpers', () => {
  it('addOption appends with auto id', () => {
    get().addOption('n2', 'C');
    const n = get().schema.nodes.find(n => n.id === 'n2') as QuestionNode;
    expect(n.options).toHaveLength(3);
    expect(n.options![2].label).toBe('C');
  });

  it('removeOption deletes by index', () => {
    get().removeOption('n2', 0);
    const n = get().schema.nodes.find(n => n.id === 'n2') as QuestionNode;
    expect(n.options).toHaveLength(1);
    expect(n.options![0].id).toBe('b');
  });

  it('reorderOptions changes order without losing ids', () => {
    get().reorderOptions('n2', 0, 1);
    const n = get().schema.nodes.find(n => n.id === 'n2') as QuestionNode;
    expect(n.options!.map(o => o.id)).toEqual(['b', 'a']);
  });

  it('refuses option ops on non-question node', () => {
    expect(get().addOption('n1', 'X')).toBe(false);
    expect(get().lastError).toMatch(/not a question/i);
  });
});

describe('surveyStore — moveNode', () => {
  it('updates ui.x and ui.y, marks dirty', () => {
    get().moveNode('n2', 300, 80);
    expect(get().schema.nodes.find(n => n.id === 'n2')!.ui).toEqual({ x: 300, y: 80 });
    expect(get().dirty).toBe(true);
  });

  it('clamps to non-negative coords', () => {
    get().moveNode('n2', -50, -10);
    expect(get().schema.nodes.find(n => n.id === 'n2')!.ui).toEqual({ x: 0, y: 0 });
  });
});

describe('surveyStore — duplicateNode', () => {
  it('creates a clone with new id, options copied with new ids, edges copied', () => {
    const newId = get().duplicateNode('n2');
    expect(newId).not.toBe('n2');
    const orig = get().schema.nodes.find(n => n.id === 'n2') as QuestionNode;
    const copy = get().schema.nodes.find(n => n.id === newId) as QuestionNode;
    expect(copy.options).toHaveLength(orig.options!.length);
    expect(copy.options!.every((o, i) => o.id !== orig.options![i].id)).toBe(true);
    expect(copy.next).toEqual(orig.next);
  });
});

describe('surveyStore — markSaved / dirty tracking', () => {
  it('any mutation flips dirty=true', () => {
    get().updateNode('n2', { text: 'x' });
    expect(get().dirty).toBe(true);
  });

  it('markSaved sets dirty=false', () => {
    get().updateNode('n2', { text: 'x' });
    get().markSaved();
    expect(get().dirty).toBe(false);
  });

  it('setSchema with same fingerprint does not mark dirty', () => {
    const s = seedSchema();
    get().setSchema(s);
    expect(get().dirty).toBe(false);
  });
});

describe('surveyStore — validation errors integration', () => {
  it('setValidation stores errors, attachable per node', () => {
    get().setValidation([{ node_id: 'n2', code: 'unreachable', message: 'unreachable' }]);
    expect(get().errorsByNodeId('n2')).toHaveLength(1);
    expect(get().errorsByNodeId('n1')).toHaveLength(0);
  });

  it('schema-level errors (no node_id) are returned by globalErrors()', () => {
    get().setValidation([{ code: 'multi-start', message: 'multiple starts' }]);
    expect(get().globalErrors()).toHaveLength(1);
  });
});

describe('surveyStore — mode toggle', () => {
  it('setMode flips between form|flow without touching schema', () => {
    const before = get().schema;
    get().setMode('flow');
    expect(get().mode).toBe('flow');
    expect(get().schema).toBe(before);
  });
});
```

Run: `cd web && npx vitest run src/stores/__tests__/surveyStore.test.ts`
Expected: fails with "Cannot find module" — failing state.

- [ ] **Step 2: Implement the store**

Create `web/src/stores/surveyStore.ts`:

```ts
// web/src/stores/surveyStore.ts
//
// The single source of truth for survey-builder UI. Both Form and Flow
// editors read/write this store. Mutations are tiny, validated, and dirty-tracked.
//
// We use `createStore` (not `create`) so each instance is fresh per test and
// per route mount; the public app wraps it with a React context provider.

import { createStore } from 'zustand/vanilla';
import { useStore } from 'zustand';
import { useContext, createContext, type ReactNode } from 'react';
import type {
  Schema,
  Node,
  QuestionNode,
  Edge,
  ValidationError,
  NodeKind,
  QuestionType,
} from '../types/survey';

// ---------- Types ----------

export type Mode = 'form' | 'flow';

export interface AddNodeInput {
  kind: NodeKind;
  type?: QuestionType;
  text?: string;
  x: number;
  y: number;
}

export interface SurveyStore {
  schema: Schema;
  selectedNodeId: string | null;
  mode: Mode;
  dirty: boolean;
  lastError: string | null;
  validationErrors: ValidationError[];
  validating: boolean;

  // bulk
  setSchema: (schema: Schema) => void;
  setMode: (mode: Mode) => void;
  selectNode: (id: string | null) => void;
  markSaved: () => void;

  // node ops
  addNode: (input: AddNodeInput) => string;
  removeNode: (id: string) => boolean;
  updateNode: (id: string, patch: Partial<Node>) => boolean;
  moveNode: (id: string, x: number, y: number) => void;
  duplicateNode: (id: string) => string | null;

  // edge ops
  addEdge: (fromId: string, edge: Edge) => boolean;
  removeEdge: (fromId: string, idx: number) => void;
  updateEdge: (fromId: string, idx: number, patch: Partial<Edge>) => void;

  // option ops (questions)
  addOption: (nodeId: string, label: string) => boolean;
  removeOption: (nodeId: string, idx: number) => void;
  reorderOptions: (nodeId: string, from: number, to: number) => void;
  updateOption: (nodeId: string, idx: number, label: string) => void;

  // metadata
  updateMetadata: (patch: Partial<Schema['metadata']>) => void;
  updateTitle: (title: string) => void;

  // validation
  setValidation: (errors: ValidationError[]) => void;
  setValidating: (v: boolean) => void;
  errorsByNodeId: (id: string) => ValidationError[];
  globalErrors: () => ValidationError[];
}

// ---------- Helpers ----------

const SCHEMA_VERSION = '1.1';

export function blankSchema(): Schema {
  return {
    version: SCHEMA_VERSION,
    title: 'Новая анкета',
    nodes: [
      { id: 'start', kind: 'start', next: [{ to: 'end', when: 'true' }], ui: { x: 40, y: 30 } },
      { id: 'end', kind: 'success-end', next: [], ui: { x: 40, y: 240 } },
    ],
    metadata: { primary_mode: 'form', max_questions: 25 },
  };
}

function fingerprint(s: Schema): string {
  // Stable hash for "is this schema the same as the saved one?" purposes.
  // Cheap: JSON.stringify with sorted keys is enough for our scale (≤ a few hundred nodes).
  return JSON.stringify(s, Object.keys(s).sort());
}

function nextNodeId(schema: Schema): string {
  let n = 1;
  const taken = new Set(schema.nodes.map(x => x.id));
  while (taken.has(`n${n}`)) n++;
  return `n${n}`;
}

function isQuestion(n: Node): n is QuestionNode {
  return n.kind === 'question';
}

// ---------- Factory ----------

export function createSurveyStore() {
  return createStore<SurveyStore>((set, get) => ({
    schema: blankSchema(),
    selectedNodeId: null,
    mode: 'form',
    dirty: false,
    lastError: null,
    validationErrors: [],
    validating: false,

    setSchema: (schema) => {
      const baseline = fingerprint(schema);
      set({
        schema,
        selectedNodeId: schema.nodes[0]?.id ?? null,
        dirty: false,
        lastError: null,
        validationErrors: [],
        // store baseline on the schema itself via a private symbol slot would be ideal;
        // but we settle for snapshot capture in markSaved/setSchema:
        // (intentionally empty)
      });
      // baseline lives in a closure-bound ref:
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
      (get() as any)._baseline = baseline;
    },

    setMode: (mode) => set({ mode }),
    selectNode: (id) => set({ selectedNodeId: id }),

    markSaved: () => {
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
      (get() as any)._baseline = fingerprint(get().schema);
      set({ dirty: false });
    },

    addNode: (input) => {
      const cur = get().schema;
      const id = nextNodeId(cur);
      let node: Node;
      if (input.kind === 'question') {
        node = {
          id, kind: 'question', type: input.type ?? 'single',
          text: input.text ?? '', required: false,
          options: ['single', 'multi', 'select'].includes(input.type ?? 'single') ? [] : undefined,
          next: [], ui: { x: input.x, y: input.y },
        } as QuestionNode;
      } else if (input.kind === 'success-end' || input.kind === 'refusal-end') {
        node = { id, kind: input.kind, next: [], ui: { x: input.x, y: input.y } };
      } else {
        node = { id, kind: input.kind, text: input.text ?? '', next: [], ui: { x: input.x, y: input.y } };
      }
      const schema: Schema = { ...cur, nodes: [...cur.nodes, node] };
      set({ schema, selectedNodeId: id, dirty: true, lastError: null });
      return id;
    },

    removeNode: (id) => {
      const cur = get().schema;
      const target = cur.nodes.find(n => n.id === id);
      if (!target) {
        set({ lastError: `node ${id} not found` });
        return false;
      }
      if (target.kind === 'start') {
        set({ lastError: 'cannot remove start node' });
        return false;
      }
      const schema: Schema = {
        ...cur,
        nodes: cur.nodes
          .filter(n => n.id !== id)
          .map(n => ({ ...n, next: n.next.filter(e => e.to !== id) })),
      };
      set({
        schema,
        selectedNodeId: get().selectedNodeId === id ? schema.nodes[0]?.id ?? null : get().selectedNodeId,
        dirty: true,
        lastError: null,
      });
      return true;
    },

    updateNode: (id, patch) => {
      const cur = get().schema;
      const idx = cur.nodes.findIndex(n => n.id === id);
      if (idx < 0) { set({ lastError: `node ${id} not found` }); return false; }

      // id-rename guard
      if (patch.id && patch.id !== id) {
        if (cur.nodes.some(n => n.id === patch.id)) {
          set({ lastError: `duplicate id: ${patch.id}` });
          return false;
        }
      }

      const next = cur.nodes.map((n, i) => (i === idx ? ({ ...n, ...patch } as Node) : n));
      // propagate id rename
      if (patch.id && patch.id !== id) {
        for (let i = 0; i < next.length; i++) {
          next[i] = { ...next[i], next: next[i].next.map(e => e.to === id ? { ...e, to: patch.id! } : e) };
        }
      }
      set({
        schema: { ...cur, nodes: next },
        selectedNodeId: patch.id && patch.id !== id ? patch.id : get().selectedNodeId,
        dirty: true,
        lastError: null,
      });
      return true;
    },

    moveNode: (id, x, y) => {
      const cx = Math.max(0, x);
      const cy = Math.max(0, y);
      const cur = get().schema;
      const next = cur.nodes.map(n => n.id === id ? { ...n, ui: { x: cx, y: cy } } : n);
      set({ schema: { ...cur, nodes: next }, dirty: true });
    },

    duplicateNode: (id) => {
      const cur = get().schema;
      const target = cur.nodes.find(n => n.id === id);
      if (!target) { set({ lastError: `node ${id} not found` }); return null; }
      const newId = nextNodeId(cur);
      let copy: Node = { ...target, id: newId, ui: { x: (target.ui?.x ?? 0) + 40, y: (target.ui?.y ?? 0) + 40 } };
      if (isQuestion(copy)) {
        copy = { ...copy, options: copy.options?.map(o => ({ ...o, id: `${o.id}_${newId}` })) };
      }
      set({
        schema: { ...cur, nodes: [...cur.nodes, copy] },
        selectedNodeId: newId,
        dirty: true,
      });
      return newId;
    },

    addEdge: (fromId, edge) => {
      const cur = get().schema;
      const from = cur.nodes.find(n => n.id === fromId);
      const to = cur.nodes.find(n => n.id === edge.to);
      if (!from) { set({ lastError: `unknown node: ${fromId}` }); return false; }
      if (!to) { set({ lastError: `unknown node: ${edge.to}` }); return false; }
      const next = cur.nodes.map(n => n.id === fromId ? { ...n, next: [...n.next, edge] } : n);
      set({ schema: { ...cur, nodes: next }, dirty: true, lastError: null });
      return true;
    },

    removeEdge: (fromId, idx) => {
      const cur = get().schema;
      const next = cur.nodes.map(n =>
        n.id === fromId ? { ...n, next: n.next.filter((_, i) => i !== idx) } : n
      );
      set({ schema: { ...cur, nodes: next }, dirty: true });
    },

    updateEdge: (fromId, idx, patch) => {
      const cur = get().schema;
      const next = cur.nodes.map(n =>
        n.id === fromId
          ? { ...n, next: n.next.map((e, i) => i === idx ? { ...e, ...patch } : e) }
          : n
      );
      set({ schema: { ...cur, nodes: next }, dirty: true });
    },

    addOption: (nodeId, label) => {
      const cur = get().schema;
      const target = cur.nodes.find(n => n.id === nodeId);
      if (!target) { set({ lastError: `node ${nodeId} not found` }); return false; }
      if (!isQuestion(target)) { set({ lastError: `node ${nodeId} is not a question` }); return false; }
      const optId = `o${(target.options?.length ?? 0) + 1}_${nodeId}`;
      const next = cur.nodes.map(n =>
        n.id === nodeId && isQuestion(n)
          ? { ...n, options: [...(n.options ?? []), { id: optId, label }] }
          : n
      );
      set({ schema: { ...cur, nodes: next }, dirty: true, lastError: null });
      return true;
    },

    removeOption: (nodeId, idx) => {
      const cur = get().schema;
      const next = cur.nodes.map(n =>
        n.id === nodeId && isQuestion(n)
          ? { ...n, options: n.options?.filter((_, i) => i !== idx) }
          : n
      );
      set({ schema: { ...cur, nodes: next }, dirty: true });
    },

    reorderOptions: (nodeId, from, to) => {
      const cur = get().schema;
      const next = cur.nodes.map(n => {
        if (n.id !== nodeId || !isQuestion(n) || !n.options) return n;
        const opts = [...n.options];
        const [item] = opts.splice(from, 1);
        opts.splice(to, 0, item);
        return { ...n, options: opts };
      });
      set({ schema: { ...cur, nodes: next }, dirty: true });
    },

    updateOption: (nodeId, idx, label) => {
      const cur = get().schema;
      const next = cur.nodes.map(n =>
        n.id === nodeId && isQuestion(n)
          ? { ...n, options: n.options?.map((o, i) => i === idx ? { ...o, label } : o) }
          : n
      );
      set({ schema: { ...cur, nodes: next }, dirty: true });
    },

    updateMetadata: (patch) => {
      const cur = get().schema;
      set({ schema: { ...cur, metadata: { ...(cur.metadata ?? {}), ...patch } }, dirty: true });
    },

    updateTitle: (title) => {
      const cur = get().schema;
      set({ schema: { ...cur, title }, dirty: true });
    },

    setValidation: (errors) => set({ validationErrors: errors }),
    setValidating: (v) => set({ validating: v }),
    errorsByNodeId: (id) => get().validationErrors.filter(e => e.node_id === id),
    globalErrors: () => get().validationErrors.filter(e => !e.node_id),
  }));
}

// ---------- React glue ----------

const Ctx = createContext<ReturnType<typeof createSurveyStore> | null>(null);

export function SurveyStoreProvider({
  children, store,
}: { children: ReactNode; store: ReturnType<typeof createSurveyStore> }) {
  return <Ctx.Provider value={store}>{children}</Ctx.Provider>;
}

export function useSurveyStore<T>(selector: (s: SurveyStore) => T): T {
  const store = useContext(Ctx);
  if (!store) throw new Error('useSurveyStore: missing SurveyStoreProvider');
  return useStore(store, selector);
}

export function useSurveyStoreApi() {
  const store = useContext(Ctx);
  if (!store) throw new Error('useSurveyStoreApi: missing SurveyStoreProvider');
  return store;
}
```

(If your `tsconfig.json` doesn't enable JSX in `.ts`, split the JSX provider into `surveyStore.tsx`.)

- [ ] **Step 3: Run tests — they must pass**

```bash
cd web && npx vitest run src/stores/__tests__/surveyStore.test.ts
```

Expected: all 30+ cases passing. If a single case fails, do NOT skip it — fix the store. The store is the heart of the builder; bugs here corrupt every other component.

- [ ] **Step 4: Commit**

```bash
git add web/src/stores/
git commit -m "feat(web/surveys): add zustand schema store with 30+ state-machine tests"
```

---

## Task 5: Debounced validation hook

**Files:**
- Create: `web/src/hooks/useDebouncedValidation.ts`
- Create: `web/src/hooks/__tests__/useDebouncedValidation.test.ts`

- [ ] **Step 1: Write the failing test**

Create `web/src/hooks/__tests__/useDebouncedValidation.test.ts`:

```ts
import { describe, it, expect, vi, beforeEach } from 'vitest';
import { renderHook, act, waitFor } from '@testing-library/react';
import { useDebouncedValidation } from '../useDebouncedValidation';
import { createSurveyStore, SurveyStoreProvider, blankSchema } from '../../stores/surveyStore';
import type { ReactNode } from 'react';
import * as api from '../../api/surveys';

describe('useDebouncedValidation', () => {
  beforeEach(() => {
    vi.useFakeTimers();
    vi.spyOn(api, 'validateSchema').mockResolvedValue({ ok: true, errors: [] });
  });

  const wrap = (store: ReturnType<typeof createSurveyStore>) =>
    ({ children }: { children: ReactNode }) =>
      <SurveyStoreProvider store={store}>{children}</SurveyStoreProvider>;

  it('does not call validate on first render before any change', () => {
    const store = createSurveyStore();
    store.getState().setSchema(blankSchema());
    renderHook(() => useDebouncedValidation('s1'), { wrapper: wrap(store) });
    expect(api.validateSchema).not.toHaveBeenCalled();
  });

  it('debounces 300ms — single call for burst of changes', async () => {
    const store = createSurveyStore();
    store.getState().setSchema(blankSchema());
    renderHook(() => useDebouncedValidation('s1'), { wrapper: wrap(store) });

    act(() => { store.getState().updateTitle('A'); });
    act(() => { store.getState().updateTitle('AB'); });
    act(() => { store.getState().updateTitle('ABC'); });
    expect(api.validateSchema).not.toHaveBeenCalled();

    await act(async () => { await vi.advanceTimersByTimeAsync(310); });
    expect(api.validateSchema).toHaveBeenCalledTimes(1);
    expect(api.validateSchema).toHaveBeenLastCalledWith('s1', expect.objectContaining({ title: 'ABC' }));
  });

  it('writes errors back into the store', async () => {
    const store = createSurveyStore();
    store.getState().setSchema(blankSchema());
    vi.spyOn(api, 'validateSchema').mockResolvedValueOnce({
      ok: false,
      errors: [{ node_id: 'start', code: 'no-text', message: 'set text' }],
    });
    renderHook(() => useDebouncedValidation('s1'), { wrapper: wrap(store) });

    act(() => { store.getState().updateTitle('X'); });
    await act(async () => { await vi.advanceTimersByTimeAsync(310); });
    await waitFor(() => expect(store.getState().errorsByNodeId('start')).toHaveLength(1));
  });

  it('drops stale responses (race-condition guard)', async () => {
    const store = createSurveyStore();
    store.getState().setSchema(blankSchema());
    let resolveFirst!: (v: { ok: boolean; errors: never[] }) => void;
    vi.spyOn(api, 'validateSchema')
      .mockImplementationOnce(() => new Promise(r => { resolveFirst = r; }))
      .mockResolvedValueOnce({ ok: false, errors: [{ code: 'fresh', message: 'fresh' }] });
    renderHook(() => useDebouncedValidation('s1'), { wrapper: wrap(store) });

    act(() => { store.getState().updateTitle('A'); });
    await act(async () => { await vi.advanceTimersByTimeAsync(310); });

    act(() => { store.getState().updateTitle('B'); });
    await act(async () => { await vi.advanceTimersByTimeAsync(310); });

    // Now resolve the first (stale) call — its errors must be ignored.
    resolveFirst({ ok: true, errors: [] });
    await act(async () => { await vi.advanceTimersByTimeAsync(0); });
    expect(store.getState().validationErrors).toHaveLength(1);
    expect(store.getState().validationErrors[0].code).toBe('fresh');
  });
});
```

Run: failing as expected (no hook yet).

- [ ] **Step 2: Implement the hook**

Create `web/src/hooks/useDebouncedValidation.ts`:

```ts
// web/src/hooks/useDebouncedValidation.ts
//
// Subscribes to schema changes; on each, after a 300ms quiescence, posts the
// current schema to the backend's /validate endpoint and writes errors back
// into the store. Includes a sequence-number guard so out-of-order responses
// from a slow connection don't overwrite fresher results.

import { useEffect, useRef } from 'react';
import { useSurveyStoreApi } from '../stores/surveyStore';
import { validateSchema } from '../api/surveys';

const DEBOUNCE_MS = 300;

export function useDebouncedValidation(surveyId: string): void {
  const storeApi = useSurveyStoreApi();
  const seqRef = useRef(0);
  const timerRef = useRef<ReturnType<typeof setTimeout> | null>(null);
  const initialFingerprint = useRef<string | null>(null);

  useEffect(() => {
    initialFingerprint.current = JSON.stringify(storeApi.getState().schema);
    const unsubscribe = storeApi.subscribe((state, prev) => {
      if (state.schema === prev.schema) return;
      if (timerRef.current) clearTimeout(timerRef.current);
      timerRef.current = setTimeout(async () => {
        const mySeq = ++seqRef.current;
        storeApi.getState().setValidating(true);
        try {
          const res = await validateSchema(surveyId, state.schema);
          if (mySeq !== seqRef.current) return; // stale
          storeApi.getState().setValidation(res.errors);
        } catch {
          // network errors: keep last-known errors; surface elsewhere if needed
        } finally {
          if (mySeq === seqRef.current) storeApi.getState().setValidating(false);
        }
      }, DEBOUNCE_MS);
    });

    return () => {
      unsubscribe();
      if (timerRef.current) clearTimeout(timerRef.current);
    };
  }, [storeApi, surveyId]);
}
```

- [ ] **Step 3: Run tests — they must pass**

```bash
cd web && npx vitest run src/hooks/__tests__/useDebouncedValidation.test.ts
```

Expected: 4 passing.

- [ ] **Step 4: Commit**

```bash
git add web/src/hooks/useDebouncedValidation.ts web/src/hooks/__tests__/useDebouncedValidation.test.ts
git commit -m "feat(web/surveys): add debounced validation hook with stale-response guard"
```

---

## Task 6: Auto-layout hook (Sugiyama-lite)

**Files:**
- Create: `web/src/hooks/useFlowAutoLayout.ts`
- Create: `web/src/hooks/__tests__/useFlowAutoLayout.test.ts`

- [ ] **Step 1: Write the test**

```ts
import { describe, it, expect } from 'vitest';
import { computeAutoLayout } from '../useFlowAutoLayout';
import type { Node } from '../../types/survey';

const make = (id: string, next: string[]): Node =>
  ({ id, kind: 'question', type: 'single', next: next.map(to => ({ to, when: 'true' })), ui: undefined } as Node);

describe('computeAutoLayout', () => {
  it('lays out a linear chain top-to-bottom with constant spacing', () => {
    const nodes: Node[] = [make('a', ['b']), make('b', ['c']), make('c', [])];
    const out = computeAutoLayout(nodes, 'a');
    expect(out.find(n => n.id === 'a')!.ui!.y).toBeLessThan(out.find(n => n.id === 'b')!.ui!.y);
    expect(out.find(n => n.id === 'b')!.ui!.y).toBeLessThan(out.find(n => n.id === 'c')!.ui!.y);
  });

  it('preserves explicit ui.{x,y} for nodes that already have it', () => {
    const nodes: Node[] = [
      { ...make('a', ['b']), ui: { x: 999, y: 999 } } as Node,
      make('b', []),
    ];
    const out = computeAutoLayout(nodes, 'a');
    expect(out.find(n => n.id === 'a')!.ui).toEqual({ x: 999, y: 999 });
  });

  it('places branches side-by-side at the same depth', () => {
    const nodes: Node[] = [make('a', ['b', 'c']), make('b', []), make('c', [])];
    const out = computeAutoLayout(nodes, 'a');
    const b = out.find(n => n.id === 'b')!;
    const c = out.find(n => n.id === 'c')!;
    expect(b.ui!.y).toBe(c.ui!.y);
    expect(b.ui!.x).not.toBe(c.ui!.x);
  });

  it('handles cycles without infinite loop', () => {
    const nodes: Node[] = [make('a', ['b']), make('b', ['a'])];
    expect(() => computeAutoLayout(nodes, 'a')).not.toThrow();
  });

  it('places orphans at the bottom', () => {
    const nodes: Node[] = [make('a', []), make('orphan', [])];
    const out = computeAutoLayout(nodes, 'a');
    expect(out.find(n => n.id === 'orphan')!.ui).toBeTruthy();
  });
});
```

- [ ] **Step 2: Implement**

Create `web/src/hooks/useFlowAutoLayout.ts`:

```ts
// web/src/hooks/useFlowAutoLayout.ts
//
// Sugiyama-lite layered placement for nodes lacking ui.{x,y}. Not a full
// graph-drawing library — just enough to give freshly-imported form-mode
// schemas a sensible flow-mode picture. The user can drag from there.

import type { Node } from '../types/survey';

const X_STEP = 280;
const Y_STEP = 130;
const X0 = 40;
const Y0 = 30;

export function computeAutoLayout(nodes: Node[], startId: string): Node[] {
  const byId = new Map(nodes.map(n => [n.id, n]));
  const depth = new Map<string, number>();

  // BFS for depth; cycles are guarded by `seen`.
  const queue: Array<[string, number]> = [[startId, 0]];
  const seen = new Set<string>();
  while (queue.length) {
    const [id, d] = queue.shift()!;
    if (seen.has(id)) continue;
    seen.add(id);
    depth.set(id, Math.max(depth.get(id) ?? 0, d));
    const n = byId.get(id);
    if (!n) continue;
    for (const e of n.next) queue.push([e.to, d + 1]);
  }

  // Assign per-row x: index within row.
  const byRow: Record<number, string[]> = {};
  for (const [id, d] of depth) {
    (byRow[d] ??= []).push(id);
  }

  const placed: Node[] = nodes.map(n => {
    if (n.ui) return n;
    const d = depth.get(n.id);
    if (d === undefined) {
      // orphan — append below the last row
      const lastRow = Math.max(...Object.keys(byRow).map(Number), 0);
      const orphans = (byRow[lastRow + 1] ??= []);
      const xi = orphans.indexOf(n.id);
      const idx = xi >= 0 ? xi : (orphans.push(n.id) - 1);
      return { ...n, ui: { x: X0 + idx * X_STEP, y: Y0 + (lastRow + 1) * Y_STEP } };
    }
    const row = byRow[d];
    const idx = row.indexOf(n.id);
    return { ...n, ui: { x: X0 + idx * X_STEP, y: Y0 + d * Y_STEP } };
  });

  return placed;
}
```

- [ ] **Step 3: Test, commit**

```bash
cd web && npx vitest run src/hooks/__tests__/useFlowAutoLayout.test.ts
git add web/src/hooks/useFlowAutoLayout.ts web/src/hooks/__tests__/useFlowAutoLayout.test.ts
git commit -m "feat(web/surveys): add Sugiyama-lite auto-layout for flow-mode bootstrap"
```

---

## Task 7: Drag-to-move hook for flow nodes

**Files:**
- Create: `web/src/hooks/useFlowDrag.ts`
- Create: `web/src/hooks/__tests__/useFlowDrag.test.ts`

- [ ] **Step 1: Write the test**

```ts
import { describe, it, expect, vi } from 'vitest';
import { renderHook, act } from '@testing-library/react';
import { useFlowDrag } from '../useFlowDrag';

describe('useFlowDrag', () => {
  it('translates pointer-move deltas to onMove(x,y) calls', () => {
    const onMove = vi.fn();
    const { result } = renderHook(() => useFlowDrag({ onMove }));

    // Simulate the canvas receiving pointerdown on a node, then pointermove, then up.
    act(() => result.current.beginDrag('n1', { x: 100, y: 50 }, { x: 10, y: 5 }));
    act(() => result.current.handlePointerMove(120, 60));
    expect(onMove).toHaveBeenCalledWith('n1', 110, 55);

    act(() => result.current.handlePointerMove(150, 80));
    expect(onMove).toHaveBeenLastCalledWith('n1', 140, 75);

    act(() => result.current.endDrag());
    act(() => result.current.handlePointerMove(999, 999));
    expect(onMove).toHaveBeenCalledTimes(2);
  });

  it('ignores pointermove if no drag started', () => {
    const onMove = vi.fn();
    const { result } = renderHook(() => useFlowDrag({ onMove }));
    act(() => result.current.handlePointerMove(50, 50));
    expect(onMove).not.toHaveBeenCalled();
  });
});
```

- [ ] **Step 2: Implement**

Create `web/src/hooks/useFlowDrag.ts`:

```ts
// web/src/hooks/useFlowDrag.ts
//
// Manual mouse-driven drag for canvas nodes. We don't use @dnd-kit here
// because @dnd-kit assumes target-drop containers; nodes float on a
// continuous canvas. Plain pointer-events with a tiny state machine is
// simpler and lets us snap-to-grid trivially in the future.

import { useCallback, useRef } from 'react';

interface Args {
  onMove: (id: string, x: number, y: number) => void;
}

interface DragState {
  id: string;
  // node origin at the moment dragging started
  originX: number;
  originY: number;
  // pointer coords at that moment
  startPx: number;
  startPy: number;
}

export function useFlowDrag({ onMove }: Args) {
  const stateRef = useRef<DragState | null>(null);

  const beginDrag = useCallback(
    (id: string, nodeOrigin: { x: number; y: number }, pointer: { x: number; y: number }) => {
      stateRef.current = {
        id,
        originX: nodeOrigin.x,
        originY: nodeOrigin.y,
        startPx: pointer.x,
        startPy: pointer.y,
      };
    },
    []
  );

  const handlePointerMove = useCallback((px: number, py: number) => {
    const s = stateRef.current;
    if (!s) return;
    const dx = px - s.startPx;
    const dy = py - s.startPy;
    onMove(s.id, s.originX + dx, s.originY + dy);
  }, [onMove]);

  const endDrag = useCallback(() => {
    stateRef.current = null;
  }, []);

  return { beginDrag, handlePointerMove, endDrag, isDragging: () => stateRef.current !== null };
}
```

- [ ] **Step 3: Test, commit**

```bash
cd web && npx vitest run src/hooks/__tests__/useFlowDrag.test.ts
git add web/src/hooks/useFlowDrag.ts web/src/hooks/__tests__/useFlowDrag.test.ts
git commit -m "feat(web/surveys): add manual flow-canvas drag hook"
```

---

## Task 8: DSL input (CodeMirror)

**Files:**
- Create: `web/src/components/survey-builder/DslInput.tsx`
- Create: `web/src/components/survey-builder/__tests__/DslInput.test.tsx`

- [ ] **Step 1: Write the test**

```tsx
import { describe, it, expect, vi } from 'vitest';
import { render, screen, fireEvent, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { DslInput } from '../DslInput';

describe('DslInput', () => {
  it('renders initial value', () => {
    render(<DslInput value="answer == 'yes'" onChange={() => {}} onValidate={async () => ({ ok: true })} />);
    expect(screen.getByText(/answer/)).toBeInTheDocument();
  });

  it('calls onChange on edits', async () => {
    const onChange = vi.fn();
    render(<DslInput value="" onChange={onChange} onValidate={async () => ({ ok: true })} />);
    const editor = screen.getByRole('textbox');
    await userEvent.type(editor, 'true');
    expect(onChange).toHaveBeenCalled();
    expect(onChange).toHaveBeenLastCalledWith('true');
  });

  it('shows ok-badge when validate resolves ok', async () => {
    render(<DslInput value="true" onChange={() => {}} onValidate={async () => ({ ok: true })} />);
    fireEvent.blur(screen.getByRole('textbox'));
    await waitFor(() => expect(screen.getByLabelText(/dsl-ok/i)).toBeInTheDocument());
  });

  it('shows error tooltip when validate reports failure', async () => {
    render(<DslInput value="bad" onChange={() => {}} onValidate={async () => ({ ok: false, message: 'syntax' })} />);
    fireEvent.blur(screen.getByRole('textbox'));
    await waitFor(() => expect(screen.getByText('syntax')).toBeInTheDocument());
  });

  it('autocompletes q-references after `q`', async () => {
    render(<DslInput value="" onChange={() => {}} onValidate={async () => ({ ok: true })} qIds={['q1', 'q2']} />);
    const editor = screen.getByRole('textbox');
    await userEvent.type(editor, 'q1.');
    await waitFor(() => expect(screen.getByText('value')).toBeInTheDocument());
  });
});
```

- [ ] **Step 2: Implement**

Create `web/src/components/survey-builder/DslInput.tsx`:

```tsx
// web/src/components/survey-builder/DslInput.tsx
//
// Tiny CodeMirror 6 surface for editing a single DSL expression.
// Highlighting is keyword-based; we don't ship a Lezer grammar. Autocomplete
// suggests `qN.value`, `qN.answered`, `answer`, `in`, `&&`, `||`.
// On blur we call `onValidate` (provided by parent — usually `surveys.validateDsl`)
// and render an inline ok / error indicator.

import { useEffect, useRef, useState } from 'react';
import { EditorState, Compartment } from '@codemirror/state';
import { EditorView, keymap, drawSelection, highlightActiveLine } from '@codemirror/view';
import { defaultKeymap } from '@codemirror/commands';
import { syntaxHighlighting, HighlightStyle } from '@codemirror/language';
import { tags as t } from '@lezer/highlight';
import { autocompletion, type CompletionContext } from '@codemirror/autocomplete';

interface Props {
  value: string;
  onChange: (next: string) => void;
  onValidate: (expr: string) => Promise<{ ok: boolean; message?: string }>;
  qIds?: string[];
  placeholder?: string;
}

const dslHighlight = HighlightStyle.define([
  { tag: t.keyword, color: 'var(--info)', fontWeight: '600' },
  { tag: t.string, color: 'var(--success)' },
  { tag: t.number, color: 'var(--warning)' },
]);

export function DslInput({ value, onChange, onValidate, qIds = [], placeholder }: Props) {
  const hostRef = useRef<HTMLDivElement | null>(null);
  const viewRef = useRef<EditorView | null>(null);
  const compartment = useRef(new Compartment());
  const [status, setStatus] = useState<'idle' | 'ok' | 'err'>('idle');
  const [errMsg, setErrMsg] = useState<string>('');

  useEffect(() => {
    if (!hostRef.current) return;

    const completion = autocompletion({
      override: [
        (ctx: CompletionContext) => {
          const word = ctx.matchBefore(/[\w.]+/);
          if (!word || (word.from === word.to && !ctx.explicit)) return null;
          const opts = [
            { label: 'answer', type: 'variable' },
            { label: 'true', type: 'keyword' },
            { label: 'false', type: 'keyword' },
            { label: 'in', type: 'keyword' },
            { label: '&&', type: 'keyword' },
            { label: '||', type: 'keyword' },
            ...qIds.flatMap(q => [
              { label: `${q}.value`, type: 'property' },
              { label: `${q}.answered`, type: 'property' },
            ]),
          ];
          return { from: word.from, options: opts };
        },
      ],
    });

    const state = EditorState.create({
      doc: value,
      extensions: [
        compartment.current.of(EditorState.readOnly.of(false)),
        keymap.of(defaultKeymap),
        drawSelection(),
        highlightActiveLine(),
        syntaxHighlighting(dslHighlight),
        completion,
        EditorView.updateListener.of(u => {
          if (u.docChanged) onChange(u.state.doc.toString());
        }),
        EditorView.domEventHandlers({
          blur: () => {
            void runValidate(viewRef.current?.state.doc.toString() ?? '');
            return false;
          },
        }),
        EditorView.theme({
          '&': { fontFamily: 'var(--font-mono)', fontSize: '0.9em' },
          '.cm-content': { padding: '8px 10px' },
          '.cm-focused': { outline: '2px solid var(--accent)' },
        }),
      ],
    });

    const view = new EditorView({ state, parent: hostRef.current });
    viewRef.current = view;
    // role="textbox" for tests
    view.dom.setAttribute('role', 'textbox');
    if (placeholder) view.dom.setAttribute('aria-placeholder', placeholder);

    return () => view.destroy();
    // we deliberately depend only on initial-mount; updates via setValue below
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  // Mirror external `value` updates back into the editor without losing focus.
  useEffect(() => {
    const view = viewRef.current;
    if (!view) return;
    if (view.state.doc.toString() === value) return;
    view.dispatch({ changes: { from: 0, to: view.state.doc.length, insert: value } });
  }, [value]);

  const runValidate = async (expr: string) => {
    if (!expr.trim()) { setStatus('idle'); return; }
    const r = await onValidate(expr);
    if (r.ok) { setStatus('ok'); setErrMsg(''); }
    else { setStatus('err'); setErrMsg(r.message ?? 'invalid'); }
  };

  return (
    <div className="dsl-input">
      <div ref={hostRef} className="dsl-input-host" />
      {status === 'ok' && <span aria-label="dsl-ok" className="dsl-ok">ok</span>}
      {status === 'err' && (
        <span role="alert" className="dsl-err" title={errMsg}>{errMsg}</span>
      )}
    </div>
  );
}
```

- [ ] **Step 3: Test, commit**

```bash
cd web && npx vitest run src/components/survey-builder/__tests__/DslInput.test.tsx
git add web/src/components/survey-builder/DslInput.tsx web/src/components/survey-builder/__tests__/DslInput.test.tsx
git commit -m "feat(web/surveys): add CodeMirror-backed DSL input with autocomplete"
```

---

## Task 9: Sortable question item (left list, drag-drop reorder)

**Files:**
- Create: `web/src/components/survey-builder/SortableQuestionItem.tsx`
- Create: `web/src/components/survey-builder/__tests__/SortableQuestionItem.test.tsx`

- [ ] **Step 1: Write the test**

```tsx
import { describe, it, expect, vi } from 'vitest';
import { render, screen } from '@testing-library/react';
import { DndContext, closestCenter } from '@dnd-kit/core';
import { SortableContext, verticalListSortingStrategy } from '@dnd-kit/sortable';
import { SortableQuestionItem } from '../SortableQuestionItem';

const wrap = (ui: React.ReactNode) => (
  <DndContext collisionDetection={closestCenter} onDragEnd={() => {}}>
    <SortableContext items={['n1','n2']} strategy={verticalListSortingStrategy}>{ui}</SortableContext>
  </DndContext>
);

describe('SortableQuestionItem', () => {
  it('renders index, type icon, truncated text', () => {
    render(wrap(
      <>
        <SortableQuestionItem id="n1" index={0} kind="question" qtype="single" text="Hello world" selected={false} hasError={false} onSelect={() => {}} />
        <SortableQuestionItem id="n2" index={1} kind="intro" text="Intro text that is long enough to be cut off" selected={true} hasError={false} onSelect={() => {}} />
      </>
    ));
    expect(screen.getByText('1')).toBeInTheDocument();
    expect(screen.getByText('2')).toBeInTheDocument();
    expect(screen.getByText(/Hello world/)).toBeInTheDocument();
  });

  it('calls onSelect when clicked', async () => {
    const onSelect = vi.fn();
    render(wrap(<SortableQuestionItem id="n1" index={0} kind="intro" text="hi" selected={false} hasError={false} onSelect={onSelect} />));
    screen.getByRole('button', { name: /Hi/i }).click();
    expect(onSelect).toHaveBeenCalledWith('n1');
  });

  it('shows error indicator when hasError', () => {
    render(wrap(<SortableQuestionItem id="n1" index={0} kind="intro" text="hi" selected={false} hasError={true} onSelect={() => {}} />));
    expect(screen.getByLabelText(/has-error/i)).toBeInTheDocument();
  });
});
```

- [ ] **Step 2: Implement**

Create `web/src/components/survey-builder/SortableQuestionItem.tsx`:

```tsx
import { useSortable } from '@dnd-kit/sortable';
import { CSS } from '@dnd-kit/utilities';
import { Icon } from '../Icon'; // from Plan 15
import type { NodeKind, QuestionType } from '../../types/survey';

interface Props {
  id: string;
  index: number;
  kind: NodeKind;
  qtype?: QuestionType;
  text: string;
  selected: boolean;
  hasError: boolean;
  onSelect: (id: string) => void;
}

const TYPE_META: Record<string, { icon: string; color: string }> = {
  intro:        { icon: 'info',         color: 'var(--info)' },
  start:        { icon: 'play',         color: 'var(--success)' },
  'text-block': { icon: 'file-text',    color: 'var(--text-muted)' },
  condition:    { icon: 'flow',         color: 'var(--warning)' },
  jump:         { icon: 'arrowRight',   color: 'var(--text-muted)' },
  'success-end':{ icon: 'check',        color: 'var(--success)' },
  'refusal-end':{ icon: 'x',            color: 'var(--danger)' },
  'q:single':   { icon: 'check',        color: 'var(--accent)' },
  'q:multi':    { icon: 'list',         color: 'var(--success)' },
  'q:number':   { icon: 'chart',        color: 'var(--warning)' },
  'q:text':     { icon: 'edit',         color: 'var(--text-muted)' },
  'q:select':   { icon: 'chevronDown',  color: 'var(--info)' },
};

export function SortableQuestionItem(p: Props) {
  const sortable = useSortable({ id: p.id });
  const style: React.CSSProperties = {
    transform: CSS.Transform.toString(sortable.transform),
    transition: sortable.transition,
    padding: '10px 12px',
    borderRadius: 6,
    cursor: 'pointer',
    background: p.selected ? 'var(--accent-soft)' : undefined,
    borderLeft: p.selected ? '3px solid var(--accent)' : '3px solid transparent',
    marginBottom: 2,
    display: 'flex',
    alignItems: 'center',
    gap: 8,
  };
  const key = p.kind === 'question' && p.qtype ? `q:${p.qtype}` : p.kind;
  const meta = TYPE_META[key] ?? TYPE_META.intro;
  const trunc = p.text.length > 28 ? p.text.slice(0, 28) + '…' : p.text;
  return (
    <div
      ref={sortable.setNodeRef}
      style={style}
      onClick={() => p.onSelect(p.id)}
      role="button"
      aria-label={trunc}
      {...sortable.attributes}
      {...sortable.listeners}
    >
      <span className="mono muted" style={{ fontSize: '0.78em', width: 22 }}>{p.index + 1}</span>
      <Icon name={meta.icon} size={14} color={meta.color} />
      <span style={{ fontSize: '0.9em', overflow: 'hidden', textOverflow: 'ellipsis', minWidth: 0, flex: 1 }}>{trunc}</span>
      {p.hasError && <span aria-label="has-error" className="error-dot" />}
    </div>
  );
}
```

- [ ] **Step 3: Test, commit**

```bash
cd web && npx vitest run src/components/survey-builder/__tests__/SortableQuestionItem.test.tsx
git add web/src/components/survey-builder/SortableQuestionItem.tsx web/src/components/survey-builder/__tests__/SortableQuestionItem.test.tsx
git commit -m "feat(web/surveys): sortable question item with type icon, error dot"
```

---

## Task 10: ValidationBadge

**Files:**
- Create: `web/src/components/survey-builder/ValidationBadge.tsx`
- Create: `web/src/components/survey-builder/__tests__/ValidationBadge.test.tsx`

- [ ] **Step 1: Test + impl**

```tsx
// __tests__/ValidationBadge.test.tsx
import { describe, it, expect } from 'vitest';
import { render, screen } from '@testing-library/react';
import { ValidationBadge } from '../ValidationBadge';

describe('ValidationBadge', () => {
  it('renders nothing when no errors', () => {
    const { container } = render(<ValidationBadge errors={[]} />);
    expect(container.firstChild).toBeNull();
  });

  it('renders count and tooltip', () => {
    render(<ValidationBadge errors={[
      { code: 'a', message: 'first', node_id: 'n1' },
      { code: 'b', message: 'second', node_id: 'n1' },
    ]} />);
    expect(screen.getByText('2')).toBeInTheDocument();
    expect(screen.getByRole('tooltip')).toHaveTextContent(/first.*second/s);
  });
});
```

```tsx
// ValidationBadge.tsx
import type { ValidationError } from '../../types/survey';

export function ValidationBadge({ errors }: { errors: ValidationError[] }) {
  if (!errors.length) return null;
  const text = errors.map(e => e.message).join('\n');
  return (
    <span className="validation-badge" role="img" aria-label="validation errors">
      <span role="tooltip" className="validation-badge-tip">{text}</span>
      <span className="validation-badge-count">{errors.length}</span>
    </span>
  );
}
```

- [ ] **Step 2: Test, commit**

```bash
cd web && npx vitest run src/components/survey-builder/__tests__/ValidationBadge.test.tsx
git add web/src/components/survey-builder/ValidationBadge.tsx web/src/components/survey-builder/__tests__/ValidationBadge.test.tsx
git commit -m "feat(web/surveys): ValidationBadge with count + tooltip"
```

---

## Task 11: SchemaPropsPanel + QuestionPalette + NodePalette + StructureList

**Files:** four small components, each ~40 lines.

- [ ] **Step 1: Implement**

`web/src/components/survey-builder/QuestionPalette.tsx`:

```tsx
import { Icon } from '../Icon';
import { useSurveyStore } from '../../stores/surveyStore';

const TYPES = [
  { type: 'single' as const, label: 'Один вариант',   icon: 'check',       color: 'var(--accent)' },
  { type: 'multi'  as const, label: 'Несколько',      icon: 'list',        color: 'var(--success)' },
  { type: 'number' as const, label: 'Число',          icon: 'chart',       color: 'var(--warning)' },
  { type: 'text'   as const, label: 'Текст',          icon: 'edit',        color: 'var(--text-muted)' },
  { type: 'select' as const, label: 'Список',         icon: 'chevronDown', color: 'var(--info)' },
];

export function QuestionPalette() {
  const addNode = useSurveyStore(s => s.addNode);
  return (
    <div className="card">
      <div className="card-header"><h3 className="card-title">Добавить вопрос</h3></div>
      <div className="card-body" style={{ padding: 12 }}>
        <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: 8 }}>
          {TYPES.map(t => (
            <button
              key={t.type}
              type="button"
              className="btn btn-secondary"
              style={{ height: 64, flexDirection: 'column', gap: 4, padding: 8 }}
              onClick={() => addNode({ kind: 'question', type: t.type, x: 0, y: 0 })}
            >
              <Icon name={t.icon} size={18} color={t.color} />
              <span style={{ fontSize: '0.82em' }}>{t.label}</span>
            </button>
          ))}
        </div>
      </div>
    </div>
  );
}
```

`web/src/components/survey-builder/NodePalette.tsx`:

```tsx
import { Icon } from '../Icon';
import { useSurveyStore } from '../../stores/surveyStore';
import type { NodeKind } from '../../types/survey';

interface Item { kind: NodeKind | 'question:single'; label: string; icon: string; color: string; }

const ITEMS: Item[] = [
  { kind: 'question:single', label: 'Вопрос',           icon: 'check',     color: 'var(--accent)' },
  { kind: 'text-block',      label: 'Блок текста',      icon: 'file-text', color: 'var(--warning)' },
  { kind: 'success-end',     label: 'Успешный финал',   icon: 'check',     color: 'var(--success)' },
  { kind: 'refusal-end',     label: 'Завершение',       icon: 'x',         color: 'var(--danger)' },
  { kind: 'condition',       label: 'Условие',          icon: 'flow',      color: 'var(--warning)' },
  { kind: 'jump',            label: 'Переход',          icon: 'arrowRight',color: 'var(--text-muted)' },
];

export function NodePalette({ canvasOrigin }: { canvasOrigin: { x: number; y: number } }) {
  const addNode = useSurveyStore(s => s.addNode);
  return (
    <div className="col gap-8" style={{ padding: 12 }}>
      {ITEMS.map(i => (
        <button
          key={i.kind}
          type="button"
          className="btn btn-secondary"
          style={{ justifyContent: 'flex-start', height: 44 }}
          onClick={() => {
            if (i.kind === 'question:single') {
              addNode({ kind: 'question', type: 'single', x: canvasOrigin.x + 40, y: canvasOrigin.y + 40 });
            } else {
              addNode({ kind: i.kind as NodeKind, x: canvasOrigin.x + 40, y: canvasOrigin.y + 40 });
            }
          }}
        >
          <Icon name={i.icon} size={16} color={i.color} />
          {i.label}
        </button>
      ))}
    </div>
  );
}
```

`web/src/components/survey-builder/StructureList.tsx`:

```tsx
import { useSurveyStore } from '../../stores/surveyStore';

const COLOR: Record<string, string> = {
  start: 'var(--success)', 'success-end': 'var(--success)', 'refusal-end': 'var(--danger)',
  question: 'var(--accent)', intro: 'var(--info)', 'text-block': 'var(--warning)',
  condition: 'var(--warning)', jump: 'var(--text-muted)',
};

export function StructureList() {
  const nodes = useSurveyStore(s => s.schema.nodes);
  const selected = useSurveyStore(s => s.selectedNodeId);
  const select = useSurveyStore(s => s.selectNode);
  return (
    <div style={{ padding: 8 }}>
      {nodes.map(n => {
        const label = n.text?.slice(0, 28) ?? n.id;
        return (
          <div
            key={n.id}
            onClick={() => select(n.id)}
            className="row"
            style={{ padding: '8px 10px', borderRadius: 4, cursor: 'pointer', fontSize: '0.88em',
                     background: selected === n.id ? 'var(--accent-soft)' : undefined }}
          >
            <span className="dot" style={{ background: COLOR[n.kind] ?? 'var(--text-muted)', width: 8, height: 8, borderRadius: '50%' }} />
            <span>{label || n.id}</span>
          </div>
        );
      })}
    </div>
  );
}
```

`web/src/components/survey-builder/SchemaPropsPanel.tsx`:

```tsx
import { useSurveyStore } from '../../stores/surveyStore';
import { Field } from '../Field'; // Plan 15

export function SchemaPropsPanel() {
  const meta = useSurveyStore(s => s.schema.metadata ?? {});
  const updateMeta = useSurveyStore(s => s.updateMetadata);
  const total = useSurveyStore(s => s.schema.nodes.filter(n => n.kind === 'question').length);
  return (
    <div className="card">
      <div className="card-header"><h3 className="card-title">Свойства анкеты</h3></div>
      <div className="card-body col gap-12">
        <div className="row" style={{ justifyContent: 'space-between' }}>
          <span className="muted">Всего вопросов</span>
          <span className="mono">{total} / {meta.max_questions ?? 25}</span>
        </div>
        <Field label="Среднее время">
          <input className="input" value={meta.estimated_minutes ?? ''}
                 onChange={e => updateMeta({ estimated_minutes: e.target.value })} placeholder="5–7 мин" />
        </Field>
        <Field label="Максимум вопросов">
          <input className="input" type="number" min={1} max={50} value={meta.max_questions ?? 25}
                 onChange={e => updateMeta({ max_questions: Number(e.target.value) })} />
        </Field>
        <hr className="divider" />
        <div className="muted" style={{ fontSize: '0.82em', textTransform: 'uppercase', letterSpacing: '0.04em' }}>Тарифы</div>
        <Field label="Стоимость анкеты для оператора (₽)">
          <input className="input" type="number" min={0} value={meta.cost_per_survey ?? 0}
                 onChange={e => updateMeta({ cost_per_survey: Number(e.target.value) })} />
        </Field>
        <Field label="Минимальное время прохождения (сек)">
          <input className="input" type="number" min={0} value={meta.min_duration_seconds ?? 0}
                 onChange={e => updateMeta({ min_duration_seconds: Number(e.target.value) })} />
        </Field>
      </div>
    </div>
  );
}
```

- [ ] **Step 2: Commit**

```bash
git add web/src/components/survey-builder/{QuestionPalette,NodePalette,StructureList,SchemaPropsPanel}.tsx
git commit -m "feat(web/surveys): question palette, node palette, structure list, schema props"
```

---

## Task 12: FormBuilder (3-column linear editor)

**Files:**
- Create: `web/src/components/survey-builder/FormBuilder.tsx`
- Create: `web/src/components/survey-builder/__tests__/FormBuilder.test.tsx`

- [ ] **Step 1: Write the test**

```tsx
import { describe, it, expect } from 'vitest';
import { render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { FormBuilder } from '../FormBuilder';
import { SurveyStoreProvider, createSurveyStore } from '../../../stores/surveyStore';
import type { Schema, QuestionNode } from '../../../types/survey';

const seed: Schema = {
  version: '1.1', title: 'T',
  nodes: [
    { id: 'n1', kind: 'intro', text: 'Hello', next: [{ to: 'n2', when: 'true' }], ui: { x: 0, y: 0 } },
    {
      id: 'n2', kind: 'question', type: 'single', text: 'Q', required: true,
      options: [{ id: 'a', label: 'A' }, { id: 'b', label: 'B' }],
      next: [{ to: 'end', when: 'true' }], ui: { x: 0, y: 100 },
    } as QuestionNode,
    { id: 'end', kind: 'success-end', next: [], ui: { x: 0, y: 200 } },
  ],
};

const renderForm = () => {
  const store = createSurveyStore();
  store.getState().setSchema(seed);
  return {
    store,
    ...render(
      <SurveyStoreProvider store={store}>
        <FormBuilder />
      </SurveyStoreProvider>
    ),
  };
};

describe('FormBuilder', () => {
  it('renders 3 columns and the question list', () => {
    renderForm();
    expect(screen.getByText(/Вопросы \(3\)/)).toBeInTheDocument();
    expect(screen.getByText(/Свойства анкеты/)).toBeInTheDocument();
    expect(screen.getByText(/Добавить вопрос/)).toBeInTheDocument();
  });

  it('selecting a question shows its editor in the center column', async () => {
    renderForm();
    await userEvent.click(screen.getByRole('button', { name: /^Q/ }));
    const ta = screen.getAllByRole('textbox').find(el => (el as HTMLTextAreaElement).value === 'Q');
    expect(ta).toBeTruthy();
  });

  it('editing question text updates the store', async () => {
    const { store } = renderForm();
    await userEvent.click(screen.getByRole('button', { name: /^Q/ }));
    const txt = screen.getByLabelText(/Текст вопроса/);
    await userEvent.clear(txt);
    await userEvent.type(txt, 'New text');
    expect(store.getState().schema.nodes.find(n => n.id === 'n2')!.text).toBe('New text');
  });

  it('adding option appends to the question', async () => {
    const { store } = renderForm();
    await userEvent.click(screen.getByRole('button', { name: /^Q/ }));
    await userEvent.click(screen.getByRole('button', { name: /Добавить вариант/i }));
    const n2 = store.getState().schema.nodes.find(n => n.id === 'n2') as QuestionNode;
    expect(n2.options).toHaveLength(3);
  });

  it('toggling required flips the flag', async () => {
    const { store } = renderForm();
    await userEvent.click(screen.getByRole('button', { name: /^Q/ }));
    await userEvent.click(screen.getByLabelText(/Обязательный/));
    const n2 = store.getState().schema.nodes.find(n => n.id === 'n2') as QuestionNode;
    expect(n2.required).toBe(false);
  });

  it('clicking "Дублировать" makes a copy', async () => {
    const { store } = renderForm();
    await userEvent.click(screen.getByRole('button', { name: /^Q/ }));
    await userEvent.click(screen.getByTitle('Дублировать'));
    expect(store.getState().schema.nodes).toHaveLength(4);
  });

  it('clicking "Удалить" removes the node', async () => {
    const { store } = renderForm();
    await userEvent.click(screen.getByRole('button', { name: /^Q/ }));
    await userEvent.click(screen.getByTitle('Удалить'));
    expect(store.getState().schema.nodes.find(n => n.id === 'n2')).toBeUndefined();
  });

  it('clicking the palette adds a new question of that type', async () => {
    const { store } = renderForm();
    await userEvent.click(screen.getByRole('button', { name: /Текст$/ }));
    const last = store.getState().schema.nodes.at(-1)!;
    expect(last.kind).toBe('question');
    expect((last as QuestionNode).type).toBe('text');
  });

  it('shows red border when node has validation errors', () => {
    const { store } = renderForm();
    store.getState().setValidation([{ node_id: 'n2', code: 'no-text', message: 'set text' }]);
    const item = within(screen.getByText(/Вопросы/).parentElement!.parentElement!).getByRole('button', { name: /^Q/ });
    expect(item.querySelector('[aria-label="has-error"]')).toBeInTheDocument();
  });
});
```

- [ ] **Step 2: Implement**

Create `web/src/components/survey-builder/FormBuilder.tsx`:

```tsx
import { DndContext, closestCenter, KeyboardSensor, PointerSensor, useSensor, useSensors, type DragEndEvent } from '@dnd-kit/core';
import { SortableContext, sortableKeyboardCoordinates, verticalListSortingStrategy, arrayMove, useSortable } from '@dnd-kit/sortable';
import { CSS } from '@dnd-kit/utilities';
import { useSurveyStore, useSurveyStoreApi } from '../../stores/surveyStore';
import { Icon } from '../Icon';
import { Field } from '../Field';
import { QuestionPalette } from './QuestionPalette';
import { SchemaPropsPanel } from './SchemaPropsPanel';
import { SortableQuestionItem } from './SortableQuestionItem';
import { DslInput } from './DslInput';
import type { QuestionNode, Node } from '../../types/survey';

const TYPE_LABEL: Record<string, string> = {
  intro: 'Вступление', start: 'Старт', 'text-block': 'Блок текста',
  condition: 'Условие', jump: 'Переход', 'success-end': 'Успех', 'refusal-end': 'Завершение',
  'q:single': 'Один вариант', 'q:multi': 'Несколько вариантов', 'q:number': 'Число',
  'q:text': 'Текстовый ответ', 'q:select': 'Выпадающий список',
};

function isQuestion(n: Node): n is QuestionNode { return n.kind === 'question'; }

function SortableOption({ id, idx, value, onChange, onRemove }: {
  id: string; idx: number; value: string; onChange: (v: string) => void; onRemove: () => void;
}) {
  const sortable = useSortable({ id });
  const style = { transform: CSS.Transform.toString(sortable.transform), transition: sortable.transition };
  return (
    <div ref={sortable.setNodeRef} style={style} className="row gap-8" {...sortable.attributes} {...sortable.listeners}>
      <span className="mono muted" style={{ width: 20, fontSize: '0.85em' }}>{idx + 1}.</span>
      <input className="input" value={value} onChange={e => onChange(e.target.value)} style={{ flex: 1 }} />
      <button type="button" className="btn btn-ghost btn-icon btn-sm" onClick={onRemove}><Icon name="trash" size={14} /></button>
    </div>
  );
}

export function FormBuilder() {
  const storeApi = useSurveyStoreApi();
  const nodes      = useSurveyStore(s => s.schema.nodes);
  const selectedId = useSurveyStore(s => s.selectedNodeId);
  const errorsBy   = useSurveyStore(s => s.errorsByNodeId);

  const select        = useSurveyStore(s => s.selectNode);
  const updateNode    = useSurveyStore(s => s.updateNode);
  const removeNode    = useSurveyStore(s => s.removeNode);
  const duplicateNode = useSurveyStore(s => s.duplicateNode);
  const addOption     = useSurveyStore(s => s.addOption);
  const removeOption  = useSurveyStore(s => s.removeOption);
  const updateOption  = useSurveyStore(s => s.updateOption);
  const reorderOpts   = useSurveyStore(s => s.reorderOptions);
  const updateEdge    = useSurveyStore(s => s.updateEdge);

  const sel = nodes.find(n => n.id === selectedId);
  const sensors = useSensors(useSensor(PointerSensor), useSensor(KeyboardSensor, { coordinateGetter: sortableKeyboardCoordinates }));

  const onListDragEnd = (e: DragEndEvent) => {
    const { active, over } = e;
    if (!over || active.id === over.id) return;
    const oldIdx = nodes.findIndex(n => n.id === active.id);
    const newIdx = nodes.findIndex(n => n.id === over.id);
    if (oldIdx < 0 || newIdx < 0) return;
    // Reorder is purely a UI concept in form-mode; we mutate the schema's `nodes` array order.
    const reordered = arrayMove(nodes, oldIdx, newIdx);
    storeApi.setState({ schema: { ...storeApi.getState().schema, nodes: reordered }, dirty: true });
  };

  const onOptDragEnd = (e: DragEndEvent) => {
    if (!sel || !isQuestion(sel)) return;
    const { active, over } = e;
    if (!over || active.id === over.id) return;
    const opts = sel.options ?? [];
    const from = opts.findIndex(o => o.id === active.id);
    const to   = opts.findIndex(o => o.id === over.id);
    if (from < 0 || to < 0) return;
    reorderOpts(sel.id, from, to);
  };

  return (
    <div style={{ display: 'grid', gridTemplateColumns: '260px 1fr 360px', gap: 16, alignItems: 'start' }}>
      {/* Left: list */}
      <div className="card">
        <div className="card-header"><h3 className="card-title">Вопросы ({nodes.length})</h3></div>
        <div style={{ padding: 8 }}>
          <DndContext sensors={sensors} collisionDetection={closestCenter} onDragEnd={onListDragEnd}>
            <SortableContext items={nodes.map(n => n.id)} strategy={verticalListSortingStrategy}>
              {nodes.map((n, i) => (
                <SortableQuestionItem
                  key={n.id}
                  id={n.id}
                  index={i}
                  kind={n.kind}
                  qtype={isQuestion(n) ? n.type : undefined}
                  text={n.text ?? n.id}
                  selected={selectedId === n.id}
                  hasError={errorsBy(n.id).length > 0}
                  onSelect={select}
                />
              ))}
            </SortableContext>
          </DndContext>
        </div>
        <div style={{ padding: 12, borderTop: '1px solid var(--border)' }}>
          <button className="btn btn-secondary btn-sm" style={{ width: '100%' }}
                  onClick={() => storeApi.getState().addNode({ kind: 'question', type: 'single', x: 0, y: 0 })}>
            <Icon name="plus" size={14} /> Добавить вопрос
          </button>
        </div>
      </div>

      {/* Center: editor */}
      {sel ? (
        <div className="card" style={{ outline: errorsBy(sel.id).length ? '2px solid var(--danger)' : undefined }}>
          <div className="card-header">
            <div className="row gap-8">
              <span className="badge badge-accent">
                {TYPE_LABEL[isQuestion(sel) ? `q:${sel.type}` : sel.kind] ?? sel.kind}
              </span>
              <span className="muted">Вопрос {nodes.findIndex(n => n.id === sel.id) + 1}</span>
            </div>
            <div className="row gap-4">
              <button className="btn btn-ghost btn-sm" title="Дублировать" onClick={() => duplicateNode(sel.id)}>
                <Icon name="plus2" size={14} />
              </button>
              <button className="btn btn-ghost btn-sm" title="Удалить" style={{ color: 'var(--danger)' }} onClick={() => removeNode(sel.id)}>
                <Icon name="trash" size={14} />
              </button>
            </div>
          </div>
          <div className="card-body col gap-16">
            <Field label="Текст вопроса (что зачитывает оператор)">
              <textarea className="textarea" rows={3} value={sel.text ?? ''}
                        onChange={e => updateNode(sel.id, { text: e.target.value })} />
            </Field>
            <Field label="Подсказка для оператора">
              <textarea className="textarea" rows={2} value={sel.hint ?? ''}
                        onChange={e => updateNode(sel.id, { hint: e.target.value })}
                        placeholder="Появится синим блоком справа от вопроса" />
            </Field>

            {isQuestion(sel) && (sel.type === 'single' || sel.type === 'multi' || sel.type === 'select') && (
              <Field label="Варианты ответа">
                <DndContext sensors={sensors} collisionDetection={closestCenter} onDragEnd={onOptDragEnd}>
                  <SortableContext items={(sel.options ?? []).map(o => o.id)} strategy={verticalListSortingStrategy}>
                    <div className="col gap-8">
                      {(sel.options ?? []).map((opt, i) => (
                        <SortableOption key={opt.id} id={opt.id} idx={i} value={opt.label}
                          onChange={v => updateOption(sel.id, i, v)}
                          onRemove={() => removeOption(sel.id, i)} />
                      ))}
                      <button type="button" className="btn btn-secondary btn-sm" style={{ alignSelf: 'flex-start' }}
                              onClick={() => addOption(sel.id, `Вариант ${(sel.options?.length ?? 0) + 1}`)}>
                        <Icon name="plus" size={14} /> Добавить вариант
                      </button>
                    </div>
                  </SortableContext>
                </DndContext>
              </Field>
            )}

            {sel.next.length > 0 && (
              <Field label="Условие следующего узла (DSL)">
                <DslInput
                  value={sel.next[0].when}
                  onChange={v => updateEdge(sel.id, 0, { when: v })}
                  qIds={nodes.map(n => n.id)}
                  onValidate={async expr => {
                    // light client-side parse: empty=ok, anything otherwise => server validates
                    return expr.trim() ? { ok: true } : { ok: false, message: 'empty' };
                  }}
                />
              </Field>
            )}

            {isQuestion(sel) && (
              <>
                <hr className="divider" />
                <div className="row" style={{ justifyContent: 'space-between' }}>
                  <label className="row gap-8" style={{ fontSize: '0.95em' }}>
                    <input type="checkbox"
                           checked={!!sel.required}
                           onChange={e => updateNode(sel.id, { required: e.target.checked } as Partial<Node>)} />
                    Обязательный для заполнения
                  </label>
                </div>
              </>
            )}
          </div>
        </div>
      ) : (
        <div className="card"><div className="card-body muted">Выберите вопрос слева, или добавьте новый.</div></div>
      )}

      {/* Right: types palette + schema-props */}
      <div className="col gap-16">
        <QuestionPalette />
        <SchemaPropsPanel />
      </div>
    </div>
  );
}
```

- [ ] **Step 3: Test, commit**

```bash
cd web && npx vitest run src/components/survey-builder/__tests__/FormBuilder.test.tsx
git add web/src/components/survey-builder/FormBuilder.tsx web/src/components/survey-builder/__tests__/FormBuilder.test.tsx
git commit -m "feat(web/surveys): FormBuilder 3-column linear editor with dnd-kit"
```

---

## Task 13: FlowEdge

**Files:**
- Create: `web/src/components/survey-builder/FlowEdge.tsx`

- [ ] **Step 1: Implement**

```tsx
// Pure SVG component — no state. Renders one cubic-bezier edge between two nodes.
// Coordinates assume node origin top-left, with port offsets applied by caller.

interface Props {
  x1: number; y1: number;
  x2: number; y2: number;
  label?: string;
  highlighted?: boolean;
  onClick?: () => void;
}

export function FlowEdge({ x1, y1, x2, y2, label, highlighted, onClick }: Props) {
  const mx = (x1 + x2) / 2;
  const my = (y1 + y2) / 2;
  const stroke = highlighted ? 'var(--accent)' : 'var(--border-strong)';
  return (
    <g onClick={onClick} style={{ cursor: onClick ? 'pointer' : undefined, pointerEvents: onClick ? 'auto' : 'none' }}>
      <path
        d={`M ${x1} ${y1} C ${x1} ${y1 + 30}, ${x2} ${y2 - 30}, ${x2} ${y2}`}
        stroke={stroke}
        strokeWidth={highlighted ? 3 : 2}
        fill="none"
        markerEnd="url(#arr)"
      />
      {label && (
        <g pointerEvents="none">
          <rect x={mx - 36} y={my - 9} width={72} height={18} rx={9} fill="white" stroke="var(--border)" />
          <text x={mx} y={my + 4} fontSize={11} textAnchor="middle" fill="var(--text-muted)">{label}</text>
        </g>
      )}
    </g>
  );
}
```

- [ ] **Step 2: Commit**

```bash
git add web/src/components/survey-builder/FlowEdge.tsx
git commit -m "feat(web/surveys): FlowEdge cubic-bezier SVG path"
```

---

## Task 14: FlowNode

**Files:**
- Create: `web/src/components/survey-builder/FlowNode.tsx`

```tsx
import type { Node, NodeKind } from '../../types/survey';

const TYPE_LABEL: Record<NodeKind, string> = {
  start: 'СТАРТ', intro: 'ВСТУПЛЕНИЕ', question: 'ВОПРОС',
  'text-block': 'БЛОК', condition: 'УСЛОВИЕ', jump: 'ПЕРЕХОД',
  'success-end': 'УСПЕХ', 'refusal-end': 'ОТКАЗ',
};
const TYPE_COLOR: Record<NodeKind, string> = {
  start: 'var(--success)', intro: 'var(--info)', question: 'var(--accent)',
  'text-block': 'var(--warning)', condition: 'var(--warning)', jump: 'var(--text-muted)',
  'success-end': 'var(--success)', 'refusal-end': 'var(--danger)',
};

interface Props {
  node: Node;
  selected: boolean;
  hasError: boolean;
  onSelect: () => void;
  onPointerDownHandle: (e: React.PointerEvent) => void;
  onStartConnect: (e: React.PointerEvent) => void;
}

export function FlowNode({ node, selected, hasError, onSelect, onPointerDownHandle, onStartConnect }: Props) {
  const color = TYPE_COLOR[node.kind] ?? 'var(--text-muted)';
  const borderColor = hasError ? 'var(--danger)' : (selected ? 'var(--accent)' : color + '50');
  return (
    <div
      className={`flow-node ${selected ? 'selected' : ''}`}
      style={{ left: node.ui?.x ?? 0, top: node.ui?.y ?? 0, borderColor }}
      onClick={onSelect}
      onPointerDown={onPointerDownHandle}
      data-testid={`flow-node-${node.id}`}
    >
      <div className="flow-node-type" style={{ color }}>{TYPE_LABEL[node.kind]}</div>
      <div style={{ fontWeight: 600, fontSize: '0.95em', marginTop: 2 }}>{node.text || node.id}</div>
      {node.kind === 'question' && (
        <div className="muted" style={{ fontSize: '0.82em', marginTop: 2 }}>
          {(node as { options?: { id: string }[] }).options?.length ?? 0} вариантов
        </div>
      )}
      {hasError && <span aria-label="has-error" className="error-dot abs-tr" />}
      {/* Outgoing port — drag from here to another node to draw an edge */}
      <div
        className="flow-node-port"
        onPointerDown={onStartConnect}
        aria-label="connect-port"
      />
    </div>
  );
}
```

Commit:

```bash
git add web/src/components/survey-builder/FlowNode.tsx
git commit -m "feat(web/surveys): FlowNode with drag-handle and connect-port"
```

---

## Task 15: FlowCanvas (the heart of flow-mode)

**Files:**
- Create: `web/src/components/survey-builder/FlowCanvas.tsx`
- Create: `web/src/components/survey-builder/__tests__/FlowCanvas.test.tsx`

- [ ] **Step 1: Test**

```tsx
import { describe, it, expect } from 'vitest';
import { render, screen, fireEvent } from '@testing-library/react';
import { FlowCanvas } from '../FlowCanvas';
import { SurveyStoreProvider, createSurveyStore } from '../../../stores/surveyStore';
import type { Schema } from '../../../types/survey';

const seed: Schema = {
  version: '1.1', title: 'T',
  nodes: [
    { id: 'a', kind: 'start',       next: [{ to: 'b', when: 'true' }], ui: { x: 0, y: 0 } },
    { id: 'b', kind: 'intro',       next: [{ to: 'c', when: 'true' }], ui: { x: 0, y: 200 } },
    { id: 'c', kind: 'success-end', next: [],                          ui: { x: 0, y: 400 } },
  ],
};

const renderCanvas = () => {
  const store = createSurveyStore();
  store.getState().setSchema(seed);
  return {
    store,
    ...render(
      <SurveyStoreProvider store={store}>
        <FlowCanvas />
      </SurveyStoreProvider>
    ),
  };
};

describe('FlowCanvas', () => {
  it('renders a node per schema entry', () => {
    renderCanvas();
    expect(screen.getByTestId('flow-node-a')).toBeInTheDocument();
    expect(screen.getByTestId('flow-node-b')).toBeInTheDocument();
    expect(screen.getByTestId('flow-node-c')).toBeInTheDocument();
  });

  it('renders one SVG path per edge', () => {
    const { container } = renderCanvas();
    const paths = container.querySelectorAll('svg path[d]');
    // 2 edges + 1 arrowhead marker = 3 path elements; we count edges only via [d^="M"]
    const edgePaths = Array.from(paths).filter(p => p.getAttribute('d')!.startsWith('M'));
    expect(edgePaths.length).toBeGreaterThanOrEqual(2);
  });

  it('clicking a node selects it', () => {
    const { store } = renderCanvas();
    fireEvent.click(screen.getByTestId('flow-node-b'));
    expect(store.getState().selectedNodeId).toBe('b');
  });

  it('drag-move updates ui.x / ui.y in store', () => {
    const { store, container } = renderCanvas();
    const node = screen.getByTestId('flow-node-b');
    fireEvent.pointerDown(node, { clientX: 100, clientY: 100, pointerId: 1 });
    const canvas = container.querySelector('.flow-canvas')!;
    fireEvent.pointerMove(canvas, { clientX: 220, clientY: 250, pointerId: 1 });
    fireEvent.pointerUp(canvas, { clientX: 220, clientY: 250, pointerId: 1 });
    const moved = store.getState().schema.nodes.find(n => n.id === 'b')!;
    expect(moved.ui!.x).toBeGreaterThan(0);
    expect(moved.ui!.y).toBeGreaterThan(200);
  });

  it('drawing an edge from a port connects two nodes', () => {
    const { store, container } = renderCanvas();
    const portA = container.querySelector('[data-testid="flow-node-a"] [aria-label="connect-port"]')!;
    fireEvent.pointerDown(portA, { clientX: 50, clientY: 50, pointerId: 2 });
    const targetC = screen.getByTestId('flow-node-c');
    fireEvent.pointerUp(targetC, { clientX: 0, clientY: 400, pointerId: 2 });
    const a = store.getState().schema.nodes.find(n => n.id === 'a')!;
    expect(a.next.find(e => e.to === 'c')).toBeTruthy();
  });

  it('edges follow nodes when they are moved (re-renders)', () => {
    const { store, container } = renderCanvas();
    const before = container.querySelector('svg path[d^="M"]')!.getAttribute('d');
    store.getState().moveNode('a', 200, 200);
    const after = container.querySelector('svg path[d^="M"]')!.getAttribute('d');
    expect(after).not.toBe(before);
  });
});
```

- [ ] **Step 2: Implement**

Create `web/src/components/survey-builder/FlowCanvas.tsx`:

```tsx
// FlowCanvas — the centerpiece of flow-mode. Renders an SVG layer for edges
// and a layer of absolutely-positioned `.flow-node` divs. Pointer events on
// the canvas drive node dragging and edge-creation. We use plain pointer
// events (not @dnd-kit) because nodes float on a continuous plane, not in
// a sortable container.

import { useRef, useState, useCallback } from 'react';
import { useSurveyStore, useSurveyStoreApi } from '../../stores/surveyStore';
import { useFlowDrag } from '../../hooks/useFlowDrag';
import { FlowNode } from './FlowNode';
import { FlowEdge } from './FlowEdge';

const NODE_W = 220;
const NODE_H = 78;

export function FlowCanvas() {
  const storeApi = useSurveyStoreApi();
  const nodes      = useSurveyStore(s => s.schema.nodes);
  const selectedId = useSurveyStore(s => s.selectedNodeId);
  const errorsBy   = useSurveyStore(s => s.errorsByNodeId);
  const moveNode   = useSurveyStore(s => s.moveNode);
  const addEdge    = useSurveyStore(s => s.addEdge);
  const select     = useSurveyStore(s => s.selectNode);

  const canvasRef = useRef<HTMLDivElement | null>(null);
  const [connectFrom, setConnectFrom] = useState<{ id: string; px: number; py: number } | null>(null);

  const drag = useFlowDrag({ onMove: moveNode });

  const onCanvasPointerMove = useCallback((e: React.PointerEvent) => {
    const rect = canvasRef.current?.getBoundingClientRect();
    if (!rect) return;
    const px = e.clientX - rect.left + (canvasRef.current!.scrollLeft);
    const py = e.clientY - rect.top + (canvasRef.current!.scrollTop);
    drag.handlePointerMove(px, py);
    if (connectFrom) {
      setConnectFrom({ ...connectFrom, px, py });
    }
  }, [drag, connectFrom]);

  const onCanvasPointerUp = useCallback((e: React.PointerEvent) => {
    drag.endDrag();
    if (connectFrom) {
      // Find which node this pointerup landed on
      const target = (e.target as HTMLElement).closest('[data-testid^="flow-node-"]');
      if (target) {
        const id = target.getAttribute('data-testid')!.replace('flow-node-', '');
        if (id !== connectFrom.id) {
          addEdge(connectFrom.id, { to: id, when: 'true' });
        }
      }
      setConnectFrom(null);
    }
  }, [drag, connectFrom, addEdge]);

  const onNodePointerDown = (id: string) => (e: React.PointerEvent) => {
    if (!canvasRef.current) return;
    if ((e.target as HTMLElement).getAttribute('aria-label') === 'connect-port') return;
    e.stopPropagation();
    const node = storeApi.getState().schema.nodes.find(n => n.id === id);
    if (!node) return;
    const rect = canvasRef.current.getBoundingClientRect();
    const px = e.clientX - rect.left + canvasRef.current.scrollLeft;
    const py = e.clientY - rect.top + canvasRef.current.scrollTop;
    drag.beginDrag(id, { x: node.ui?.x ?? 0, y: node.ui?.y ?? 0 }, { x: px, y: py });
    canvasRef.current.setPointerCapture(e.pointerId);
  };

  const onPortPointerDown = (id: string) => (e: React.PointerEvent) => {
    if (!canvasRef.current) return;
    e.stopPropagation();
    const rect = canvasRef.current.getBoundingClientRect();
    const px = e.clientX - rect.left + canvasRef.current.scrollLeft;
    const py = e.clientY - rect.top + canvasRef.current.scrollTop;
    setConnectFrom({ id, px, py });
    canvasRef.current.setPointerCapture(e.pointerId);
  };

  return (
    <div
      ref={canvasRef}
      className="flow-canvas"
      style={{ minHeight: 720, position: 'relative' }}
      onPointerMove={onCanvasPointerMove}
      onPointerUp={onCanvasPointerUp}
      onPointerCancel={onCanvasPointerUp}
    >
      <svg width="100%" height="100%" style={{ position: 'absolute', inset: 0, pointerEvents: 'none' }}>
        <defs>
          <marker id="arr" viewBox="0 0 10 10" refX="9" refY="5" markerWidth="6" markerHeight="6" orient="auto-start-reverse">
            <path d="M0,0 L10,5 L0,10 z" fill="var(--border-strong)" />
          </marker>
        </defs>
        {nodes.map(n => n.next.map((e, i) => {
          const target = nodes.find(t => t.id === e.to);
          if (!target) return null;
          const x1 = (n.ui?.x ?? 0) + NODE_W / 2;
          const y1 = (n.ui?.y ?? 0) + NODE_H;
          const x2 = (target.ui?.x ?? 0) + NODE_W / 2;
          const y2 = (target.ui?.y ?? 0);
          return <FlowEdge key={`${n.id}-${i}`} x1={x1} y1={y1} x2={x2} y2={y2} label={e.label} />;
        }))}
        {connectFrom && (() => {
          const from = nodes.find(n => n.id === connectFrom.id);
          if (!from) return null;
          return (
            <FlowEdge
              x1={(from.ui?.x ?? 0) + NODE_W / 2}
              y1={(from.ui?.y ?? 0) + NODE_H}
              x2={connectFrom.px}
              y2={connectFrom.py}
              highlighted
            />
          );
        })()}
      </svg>

      {nodes.map(n => (
        <FlowNode
          key={n.id}
          node={n}
          selected={selectedId === n.id}
          hasError={errorsBy(n.id).length > 0}
          onSelect={() => select(n.id)}
          onPointerDownHandle={onNodePointerDown(n.id)}
          onStartConnect={onPortPointerDown(n.id)}
        />
      ))}
    </div>
  );
}
```

- [ ] **Step 3: Test, commit**

```bash
cd web && npx vitest run src/components/survey-builder/__tests__/FlowCanvas.test.tsx
git add web/src/components/survey-builder/FlowCanvas.tsx web/src/components/survey-builder/__tests__/FlowCanvas.test.tsx
git commit -m "feat(web/surveys): FlowCanvas with drag-move and edge-draw"
```

---

## Task 16: NodePropsPanel

**Files:**
- Create: `web/src/components/survey-builder/NodePropsPanel.tsx`

```tsx
import { useSurveyStore } from '../../stores/surveyStore';
import { Field } from '../Field';
import { Icon } from '../Icon';
import { DslInput } from './DslInput';
import type { QuestionNode, Node } from '../../types/survey';

const KIND_LABEL: Record<string, string> = {
  start: 'Старт', intro: 'Вступление', question: 'Вопрос',
  'text-block': 'Блок текста', condition: 'Условие', jump: 'Переход',
  'success-end': 'Успешный финал', 'refusal-end': 'Завершение / отказ',
};

function isQuestion(n: Node): n is QuestionNode { return n.kind === 'question'; }

export function NodePropsPanel() {
  const sel        = useSurveyStore(s => s.schema.nodes.find(n => n.id === s.selectedNodeId) ?? null);
  const nodes      = useSurveyStore(s => s.schema.nodes);
  const errorsBy   = useSurveyStore(s => s.errorsByNodeId);
  const updateNode = useSurveyStore(s => s.updateNode);
  const updateEdge = useSurveyStore(s => s.updateEdge);
  const removeEdge = useSurveyStore(s => s.removeEdge);

  if (!sel) return <div className="card"><div className="card-body muted">Выберите узел.</div></div>;

  return (
    <div className="col gap-16">
      <div className="card">
        <div className="card-header">
          <h3 className="card-title">Свойства узла</h3>
          {errorsBy(sel.id).length > 0 && <span className="badge badge-danger">{errorsBy(sel.id).length} ошибок</span>}
        </div>
        <div className="card-body col gap-12">
          <div className="row" style={{ justifyContent: 'space-between' }}>
            <span className="muted">ID</span><span className="mono">{sel.id}</span>
          </div>
          <div className="row" style={{ justifyContent: 'space-between' }}>
            <span className="muted">Тип</span><span>{KIND_LABEL[sel.kind] ?? sel.kind}</span>
          </div>

          <Field label="Текст вопроса">
            <textarea className="textarea" rows={3} value={sel.text ?? ''}
                      onChange={e => updateNode(sel.id, { text: e.target.value })} />
          </Field>

          {isQuestion(sel) && (
            <Field label="Подсказка">
              <textarea className="textarea" rows={2} value={sel.hint ?? ''}
                        onChange={e => updateNode(sel.id, { hint: e.target.value })} />
            </Field>
          )}

          <Field label="Переходы (next)">
            <div className="col gap-6">
              {sel.next.map((e, i) => (
                <div key={i} className="row gap-6" style={{ padding: '8px 10px', background: 'var(--bg-soft)', borderRadius: 4 }}>
                  <span className="mono muted" style={{ minWidth: 24 }}>{i + 1}.</span>
                  <select className="input" style={{ width: 110 }} value={e.to}
                          onChange={ev => updateEdge(sel.id, i, { to: ev.target.value })}>
                    {nodes.filter(n => n.id !== sel.id).map(n => <option key={n.id} value={n.id}>{n.id}</option>)}
                  </select>
                  <span className="muted">когда</span>
                  <div style={{ flex: 1 }}>
                    <DslInput
                      value={e.when}
                      onChange={v => updateEdge(sel.id, i, { when: v })}
                      qIds={nodes.map(x => x.id)}
                      onValidate={async expr => expr.trim() ? { ok: true } : { ok: false, message: 'empty' }}
                    />
                  </div>
                  <button type="button" className="btn btn-ghost btn-sm" onClick={() => removeEdge(sel.id, i)}>
                    <Icon name="trash" size={12} />
                  </button>
                </div>
              ))}
            </div>
          </Field>
        </div>
      </div>

      {errorsBy(sel.id).length > 0 && (
        <div className="card">
          <div className="card-header"><h3 className="card-title">Ошибки</h3></div>
          <div className="card-body col gap-6">
            {errorsBy(sel.id).map((e, i) => (
              <div key={i} className="row gap-6" style={{ color: 'var(--danger)' }}>
                <Icon name="alert-circle" size={14} /><span>{e.message}</span>
              </div>
            ))}
          </div>
        </div>
      )}
    </div>
  );
}
```

Commit:

```bash
git add web/src/components/survey-builder/NodePropsPanel.tsx
git commit -m "feat(web/surveys): NodePropsPanel with edge editing and error list"
```

---

## Task 17: FlowBuilder (3-column wrapper around canvas)

**Files:**
- Create: `web/src/components/survey-builder/FlowBuilder.tsx`
- Create: `web/src/components/survey-builder/__tests__/FlowBuilder.test.tsx`

- [ ] **Step 1: Test**

```tsx
import { describe, it, expect } from 'vitest';
import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { FlowBuilder } from '../FlowBuilder';
import { SurveyStoreProvider, createSurveyStore, blankSchema } from '../../../stores/surveyStore';

const renderFlow = () => {
  const store = createSurveyStore();
  store.getState().setSchema(blankSchema());
  return { store, ...render(
    <SurveyStoreProvider store={store}>
      <FlowBuilder />
    </SurveyStoreProvider>
  ) };
};

describe('FlowBuilder', () => {
  it('renders palette, canvas, and props column', () => {
    renderFlow();
    expect(screen.getByText(/Палитра/)).toBeInTheDocument();
    expect(screen.getByText(/Граф анкеты/)).toBeInTheDocument();
    expect(screen.getByText(/Свойства узла/)).toBeInTheDocument();
  });

  it('clicking palette adds a node', async () => {
    const { store } = renderFlow();
    const before = store.getState().schema.nodes.length;
    await userEvent.click(screen.getByRole('button', { name: /^Вопрос$/ }));
    expect(store.getState().schema.nodes.length).toBe(before + 1);
  });

  it('header counter shows nodes/edges count', () => {
    const { store } = renderFlow();
    const n = store.getState().schema.nodes.length;
    const e = store.getState().schema.nodes.flatMap(x => x.next).length;
    expect(screen.getByText(`${n} узлов · ${e} связей`)).toBeInTheDocument();
  });
});
```

- [ ] **Step 2: Implement**

```tsx
import { useSurveyStore } from '../../stores/surveyStore';
import { Icon } from '../Icon';
import { NodePalette } from './NodePalette';
import { StructureList } from './StructureList';
import { FlowCanvas } from './FlowCanvas';
import { NodePropsPanel } from './NodePropsPanel';

export function FlowBuilder() {
  const nodeCount = useSurveyStore(s => s.schema.nodes.length);
  const edgeCount = useSurveyStore(s => s.schema.nodes.reduce((acc, n) => acc + n.next.length, 0));

  return (
    <div style={{ display: 'grid', gridTemplateColumns: '240px 1fr 320px', gap: 16, alignItems: 'start' }}>
      <div className="card">
        <div className="card-header"><h3 className="card-title">Палитра</h3></div>
        <NodePalette canvasOrigin={{ x: 80, y: 80 }} />
        <div className="card-header"><h3 className="card-title">Структура</h3></div>
        <StructureList />
      </div>

      <div className="card" style={{ padding: 0 }}>
        <div className="card-header">
          <div className="row gap-12">
            <h3 className="card-title">Граф анкеты</h3>
            <span className="badge">{nodeCount} узлов · {edgeCount} связей</span>
          </div>
          <div className="row gap-4">
            <button className="btn btn-secondary btn-sm">100%</button>
            <button className="btn btn-ghost btn-icon btn-sm" title="Авто-раскладка"><Icon name="settings" size={14} /></button>
          </div>
        </div>
        <FlowCanvas />
      </div>

      <NodePropsPanel />
    </div>
  );
}
```

- [ ] **Step 3: Test, commit**

```bash
cd web && npx vitest run src/components/survey-builder/__tests__/FlowBuilder.test.tsx
git add web/src/components/survey-builder/FlowBuilder.tsx web/src/components/survey-builder/__tests__/FlowBuilder.test.tsx
git commit -m "feat(web/surveys): FlowBuilder 3-column wrapper"
```

---

## Task 18: SaveVersionModal

**Files:**
- Create: `web/src/components/survey-builder/SaveVersionModal.tsx`
- Create: `web/src/components/survey-builder/__tests__/SaveVersionModal.test.tsx`

- [ ] **Step 1: Test**

```tsx
import { describe, it, expect, vi } from 'vitest';
import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { SaveVersionModal } from '../SaveVersionModal';

describe('SaveVersionModal', () => {
  it('renders with default minor selected', () => {
    render(<SaveVersionModal open onClose={() => {}} onSave={async () => {}} currentLabel="v1.0" />);
    expect(screen.getByLabelText(/Minor/)).toBeChecked();
    expect(screen.getByText(/v1.0 → v1.1/)).toBeInTheDocument();
  });

  it('switching to major updates the preview', async () => {
    render(<SaveVersionModal open onClose={() => {}} onSave={async () => {}} currentLabel="v1.0" />);
    await userEvent.click(screen.getByLabelText(/Major/));
    expect(screen.getByText(/v1.0 → v2.0/)).toBeInTheDocument();
  });

  it('calling onSave passes bump and notes', async () => {
    const onSave = vi.fn().mockResolvedValue(undefined);
    render(<SaveVersionModal open onClose={() => {}} onSave={onSave} currentLabel="v3.2" />);
    await userEvent.type(screen.getByLabelText(/Описание/), 'fix wording');
    await userEvent.click(screen.getByRole('button', { name: /Сохранить/i }));
    expect(onSave).toHaveBeenCalledWith({ bump: 'minor', notes: 'fix wording' });
  });
});
```

- [ ] **Step 2: Implement**

```tsx
import { useState } from 'react';
import { Modal } from '../Modal'; // from Plan 15
import { Field } from '../Field';

interface Props {
  open: boolean;
  onClose: () => void;
  onSave: (input: { bump: 'major' | 'minor'; notes: string }) => Promise<void>;
  currentLabel: string;
}

function bumpLabel(cur: string, kind: 'major' | 'minor'): string {
  const m = /^v(\d+)\.(\d+)$/.exec(cur);
  if (!m) return cur;
  const M = Number(m[1]);
  const m_ = Number(m[2]);
  return kind === 'major' ? `v${M + 1}.0` : `v${M}.${m_ + 1}`;
}

export function SaveVersionModal({ open, onClose, onSave, currentLabel }: Props) {
  const [bump, setBump] = useState<'major' | 'minor'>('minor');
  const [notes, setNotes] = useState('');
  const [saving, setSaving] = useState(false);

  return (
    <Modal open={open} onClose={onClose} title="Сохранить версию">
      <div className="col gap-12">
        <div className="row gap-12">
          <label className="row gap-4">
            <input type="radio" name="bump" checked={bump === 'minor'} onChange={() => setBump('minor')} aria-label="Minor" />
            Minor (исправления)
          </label>
          <label className="row gap-4">
            <input type="radio" name="bump" checked={bump === 'major'} onChange={() => setBump('major')} aria-label="Major" />
            Major (несовместимые)
          </label>
        </div>
        <div className="info-box">
          {currentLabel} → {bumpLabel(currentLabel, bump)}
        </div>
        <Field label="Описание изменений (опционально)">
          <textarea className="textarea" rows={3} value={notes} onChange={e => setNotes(e.target.value)} />
        </Field>
        <div className="row" style={{ justifyContent: 'flex-end', gap: 8 }}>
          <button className="btn btn-secondary" onClick={onClose}>Отмена</button>
          <button
            className="btn btn-primary"
            disabled={saving}
            onClick={async () => {
              setSaving(true);
              try { await onSave({ bump, notes }); onClose(); }
              finally { setSaving(false); }
            }}
          >
            Сохранить
          </button>
        </div>
      </div>
    </Modal>
  );
}
```

- [ ] **Step 3: Test, commit**

```bash
cd web && npx vitest run src/components/survey-builder/__tests__/SaveVersionModal.test.tsx
git add web/src/components/survey-builder/SaveVersionModal.tsx web/src/components/survey-builder/__tests__/SaveVersionModal.test.tsx
git commit -m "feat(web/surveys): SaveVersionModal with major/minor toggle"
```

---

## Task 19: SurveyBuilder page

**Files:**
- Create: `web/src/pages/admin/SurveyBuilder.tsx`
- Create: `web/src/pages/admin/__tests__/SurveyBuilder.test.tsx`

- [ ] **Step 1: Test**

```tsx
import { describe, it, expect, beforeAll, afterEach, afterAll, vi } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { setupServer } from 'msw/node';
import { http, HttpResponse } from 'msw';
import { MemoryRouter, Route, Routes } from 'react-router-dom';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { SurveyBuilder } from '../SurveyBuilder';
import { blankSchema } from '../../../stores/surveyStore';

const detail = {
  id: 's1', name: 'Mon', project_code: 'ВЦИОМ-2026-05',
  active_version_id: 'v1', draft_schema: blankSchema(),
  versions: [{ id: 'v1', label: 'v1.0', major: 1, minor: 0, is_active: true, created_at: '', created_by: 'u', notes: '' }],
  status: 'active' as const,
};

const server = setupServer(
  http.get('/api/surveys/s1', () => HttpResponse.json(detail)),
  http.post('/api/surveys/s1/validate', () => HttpResponse.json({ ok: true, errors: [] })),
  http.post('/api/surveys/s1/versions', () => HttpResponse.json({ id: 'v2', label: 'v1.1', major: 1, minor: 1, is_active: false, created_at: '', created_by: 'u', notes: '' }))
);

beforeAll(() => server.listen());
afterEach(() => server.resetHandlers());
afterAll(() => server.close());

const wrap = (tenantBuilderMode: 'form' | 'flow' = 'form') => {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return (
    <QueryClientProvider client={qc}>
      <MemoryRouter initialEntries={['/admin/surveys/s1']}>
        <Routes>
          <Route path="/admin/surveys/:id" element={<SurveyBuilder tenantBuilderMode={tenantBuilderMode} />} />
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>
  );
};

describe('SurveyBuilder page', () => {
  it('loads survey and renders header', async () => {
    render(wrap());
    await waitFor(() => expect(screen.getByText('Mon')).toBeInTheDocument());
    expect(screen.getByText(/ВЦИОМ-2026-05/)).toBeInTheDocument();
  });

  it('shows form-builder by default when tenant builderMode=form', async () => {
    render(wrap('form'));
    await waitFor(() => expect(screen.getByText(/Вопросы \(/)).toBeInTheDocument());
  });

  it('shows flow-builder when tenant builderMode=flow', async () => {
    render(wrap('flow'));
    await waitFor(() => expect(screen.getByText(/Граф анкеты/)).toBeInTheDocument());
  });

  it('mode-tab toggles between form and flow', async () => {
    render(wrap('form'));
    await waitFor(() => expect(screen.getByText(/Вопросы \(/)).toBeInTheDocument());
    await userEvent.click(screen.getByRole('tab', { name: /Flow/i }));
    expect(screen.getByText(/Граф анкеты/)).toBeInTheDocument();
  });

  it('shows "Несохранённые изменения" badge when dirty', async () => {
    render(wrap('form'));
    await waitFor(() => expect(screen.getByText(/Вопросы \(/)).toBeInTheDocument());
    await userEvent.click(screen.getByRole('button', { name: /Добавить вопрос/i }));
    expect(screen.getByText(/Несохранённые/)).toBeInTheDocument();
  });

  it('"Сохранить версию" opens modal and posts on save', async () => {
    render(wrap('form'));
    await waitFor(() => expect(screen.getByText(/Вопросы \(/)).toBeInTheDocument());
    await userEvent.click(screen.getByRole('button', { name: /Сохранить версию/ }));
    expect(screen.getByText(/Сохранить версию$/)).toBeInTheDocument(); // modal title
    await userEvent.click(screen.getAllByRole('button', { name: /Сохранить$/ })[0]);
    await waitFor(() => expect(screen.queryByText(/Несохранённые/)).not.toBeInTheDocument());
  });
});
```

- [ ] **Step 2: Implement**

```tsx
import { useEffect, useMemo, useState } from 'react';
import { useNavigate, useParams } from 'react-router-dom';
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query';
import { fetchSurvey, saveVersion as apiSaveVersion } from '../../api/surveys';
import {
  createSurveyStore, SurveyStoreProvider, useSurveyStore, useSurveyStoreApi,
} from '../../stores/surveyStore';
import { useDebouncedValidation } from '../../hooks/useDebouncedValidation';
import { Icon } from '../../components/Icon';
import { FormBuilder } from '../../components/survey-builder/FormBuilder';
import { FlowBuilder } from '../../components/survey-builder/FlowBuilder';
import { SaveVersionModal } from '../../components/survey-builder/SaveVersionModal';

interface Props { tenantBuilderMode: 'form' | 'flow' }

export function SurveyBuilder({ tenantBuilderMode }: Props) {
  const { id } = useParams<{ id: string }>();
  const surveyId = id!;
  const store = useMemo(() => createSurveyStore(), [surveyId]);

  return (
    <SurveyStoreProvider store={store}>
      <Inner surveyId={surveyId} tenantBuilderMode={tenantBuilderMode} />
    </SurveyStoreProvider>
  );
}

function Inner({ surveyId, tenantBuilderMode }: { surveyId: string; tenantBuilderMode: 'form' | 'flow' }) {
  const navigate = useNavigate();
  const qc = useQueryClient();
  const storeApi = useSurveyStoreApi();
  const dirty       = useSurveyStore(s => s.dirty);
  const validating  = useSurveyStore(s => s.validating);
  const globalErrs  = useSurveyStore(s => s.globalErrors());
  const mode        = useSurveyStore(s => s.mode);
  const setMode     = useSurveyStore(s => s.setMode);
  const setSchema   = useSurveyStore(s => s.setSchema);
  const markSaved   = useSurveyStore(s => s.markSaved);

  const { data: survey, isLoading } = useQuery({
    queryKey: ['survey', surveyId],
    queryFn: () => fetchSurvey(surveyId),
  });

  useEffect(() => {
    if (!survey) return;
    setSchema(survey.draft_schema);
    setMode(survey.draft_schema.metadata?.primary_mode ?? tenantBuilderMode);
  }, [survey, setSchema, setMode, tenantBuilderMode]);

  useDebouncedValidation(surveyId);

  const [saveOpen, setSaveOpen] = useState(false);
  const saveMut = useMutation({
    mutationFn: async (input: { bump: 'major' | 'minor'; notes: string }) => {
      return apiSaveVersion(surveyId, { ...input, schema: storeApi.getState().schema });
    },
    onSuccess: () => {
      markSaved();
      void qc.invalidateQueries({ queryKey: ['survey', surveyId] });
    },
  });

  if (isLoading || !survey) return <div className="page muted">Загрузка…</div>;
  const activeLabel = survey.versions.find(v => v.is_active)?.label ?? 'v1.0';

  return (
    <div className="page" data-screen-label="survey builder" style={{ maxWidth: '100%' }}>
      <div className="page-header">
        <div className="row gap-8">
          <button className="btn btn-ghost btn-icon" onClick={() => navigate('/admin/surveys')} title="Назад">
            <Icon name="chevronLeft" size={20} />
          </button>
          <div>
            <div className="muted mono" style={{ fontSize: '0.82em' }}>{survey.project_code} · {activeLabel}</div>
            <h1>{survey.name}</h1>
          </div>
        </div>
        <div className="row gap-8">
          {validating && <span className="badge badge-muted"><Icon name="loader" size={12} /> Проверка…</span>}
          {globalErrs.length > 0 && <span className="badge badge-danger">{globalErrs.length} ошибок схемы</span>}
          {dirty && <span className="badge badge-warning"><Icon name="alert-circle" size={12} /> Несохранённые изменения</span>}
          <button className="btn btn-secondary" onClick={() => window.open(`/admin/surveys/${surveyId}/preview`, '_blank')}>
            <Icon name="eye" size={16} /> Превью
          </button>
          <button className="btn btn-primary" disabled={!dirty || globalErrs.length > 0} onClick={() => setSaveOpen(true)}>
            <Icon name="save" size={16} /> Сохранить версию
          </button>
        </div>
      </div>

      <div className="row gap-8" style={{ marginBottom: 16 }} role="tablist">
        <button role="tab" aria-selected={mode === 'form'} className={`btn ${mode === 'form' ? 'btn-primary' : 'btn-ghost'}`}
                onClick={() => setMode('form')}>Form</button>
        <button role="tab" aria-selected={mode === 'flow'} className={`btn ${mode === 'flow' ? 'btn-primary' : 'btn-ghost'}`}
                onClick={() => setMode('flow')}>Flow</button>
      </div>

      {mode === 'flow' ? <FlowBuilder /> : <FormBuilder />}

      <SaveVersionModal
        open={saveOpen}
        onClose={() => setSaveOpen(false)}
        currentLabel={activeLabel}
        onSave={async input => { await saveMut.mutateAsync(input); }}
      />
    </div>
  );
}
```

- [ ] **Step 3: Test, commit**

```bash
cd web && npx vitest run src/pages/admin/__tests__/SurveyBuilder.test.tsx
git add web/src/pages/admin/SurveyBuilder.tsx web/src/pages/admin/__tests__/SurveyBuilder.test.tsx
git commit -m "feat(web/surveys): SurveyBuilder page with mode tabs and save flow"
```

---

## Task 20: Surveys list page

**Files:**
- Create: `web/src/pages/admin/Surveys.tsx`
- Create: `web/src/pages/admin/__tests__/Surveys.test.tsx`

- [ ] **Step 1: Test**

```tsx
import { describe, it, expect, beforeAll, afterEach, afterAll } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { setupServer } from 'msw/node';
import { http, HttpResponse } from 'msw';
import { MemoryRouter } from 'react-router-dom';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { Surveys } from '../Surveys';

const rows = [
  { id: 's1', name: 'A', project_code: 'P-1', questions: 12, active_version: 'v3.2', updated_at: '2026-05-04T00:00:00Z', status: 'active' },
  { id: 's2', name: 'B', project_code: 'P-2', questions: 8, active_version: 'v1.0', updated_at: '2026-04-28T00:00:00Z', status: 'paused' },
];
const server = setupServer(http.get('/api/surveys', () => HttpResponse.json(rows)));

beforeAll(() => server.listen()); afterEach(() => server.resetHandlers()); afterAll(() => server.close());

const wrap = () => {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return (
    <QueryClientProvider client={qc}>
      <MemoryRouter><Surveys /></MemoryRouter>
    </QueryClientProvider>
  );
};

describe('Surveys list', () => {
  it('renders header and "Новая анкета"', async () => {
    render(wrap());
    expect(screen.getByText(/Анкеты/)).toBeInTheDocument();
    expect(screen.getByRole('button', { name: /Новая анкета/i })).toBeInTheDocument();
  });

  it('lists rows from the API', async () => {
    render(wrap());
    await waitFor(() => expect(screen.getByText('A')).toBeInTheDocument());
    expect(screen.getByText('B')).toBeInTheDocument();
    expect(screen.getByText('v3.2')).toBeInTheDocument();
    expect(screen.getByText('Активна')).toBeInTheDocument();
    expect(screen.getByText('На паузе')).toBeInTheDocument();
  });

  it('clicking row navigates to builder', async () => {
    render(wrap());
    await waitFor(() => expect(screen.getByText('A')).toBeInTheDocument());
    await userEvent.click(screen.getByText('A'));
    expect(window.location.pathname || '').toMatch(/surveys/);
  });
});
```

- [ ] **Step 2: Implement**

```tsx
import { useNavigate } from 'react-router-dom';
import { useQuery } from '@tanstack/react-query';
import { listSurveys } from '../../api/surveys';
import { Icon } from '../../components/Icon';

const STATUS_BADGE: Record<string, { class: string; text: string }> = {
  active:   { class: 'badge-success', text: 'Активна' },
  paused:   { class: 'badge-warning', text: 'На паузе' },
  archived: { class: 'badge-muted',   text: 'Архив' },
};

export function Surveys() {
  const nav = useNavigate();
  const { data, isLoading, error } = useQuery({ queryKey: ['surveys'], queryFn: listSurveys });

  return (
    <div className="page" data-screen-label="admin surveys">
      <div className="page-header">
        <div>
          <h1>Анкеты</h1>
          <div className="muted">Конструктор и управление шаблонами опросов</div>
        </div>
        <button className="btn btn-primary" onClick={() => nav('/admin/surveys/new')}>
          <Icon name="plus" size={16} /> Новая анкета
        </button>
      </div>

      <div className="card">
        {isLoading && <div className="card-body muted">Загрузка…</div>}
        {error && <div className="card-body" style={{ color: 'var(--danger)' }}>Ошибка: {String((error as Error).message)}</div>}
        {data && (
          <table className="table">
            <thead>
              <tr>
                <th>Анкета</th><th>Проект</th><th>Вопросов</th><th>Версия</th><th>Обновлена</th><th>Статус</th><th></th>
              </tr>
            </thead>
            <tbody>
              {data.map(s => {
                const sb = STATUS_BADGE[s.status] ?? STATUS_BADGE.archived;
                return (
                  <tr key={s.id} style={{ cursor: 'pointer' }} onClick={() => nav(`/admin/surveys/${s.id}`)}>
                    <td>
                      <div className="row gap-8">
                        <Icon name="file-text" size={18} color="var(--text-muted)" />
                        <span style={{ fontWeight: 500 }}>{s.name}</span>
                      </div>
                    </td>
                    <td className="mono muted">{s.project_code}</td>
                    <td>{s.questions}</td>
                    <td className="mono">{s.active_version}</td>
                    <td className="muted">{new Date(s.updated_at).toLocaleDateString('ru-RU')}</td>
                    <td><span className={`badge ${sb.class}`}>{sb.text}</span></td>
                    <td style={{ textAlign: 'right' }}>
                      <button className="btn btn-ghost btn-sm" onClick={e => { e.stopPropagation(); nav(`/admin/surveys/${s.id}`); }}>
                        <Icon name="edit" size={14} />
                      </button>
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        )}
      </div>
    </div>
  );
}
```

- [ ] **Step 3: Test, commit**

```bash
cd web && npx vitest run src/pages/admin/__tests__/Surveys.test.tsx
git add web/src/pages/admin/Surveys.tsx web/src/pages/admin/__tests__/Surveys.test.tsx
git commit -m "feat(web/surveys): admin surveys list page"
```

---

## Task 21: SurveyPreview page

**Files:**
- Create: `web/src/pages/admin/SurveyPreview.tsx`
- Create: `web/src/pages/admin/__tests__/SurveyPreview.test.tsx`

The preview is a simulator that walks the schema using the Plan-07/15 WASM runtime.

- [ ] **Step 1: Test**

```tsx
import { describe, it, expect, beforeAll, afterEach, afterAll, vi } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { setupServer } from 'msw/node';
import { http, HttpResponse } from 'msw';
import { MemoryRouter, Route, Routes } from 'react-router-dom';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { SurveyPreview } from '../SurveyPreview';
import * as runtime from '../../../runtime/surveys-runtime';
import type { Schema } from '../../../types/survey';

const seed: Schema = {
  version: '1.1', title: 'Demo',
  nodes: [
    { id: 'start', kind: 'start', next: [{ to: 'q1', when: 'true' }], ui: { x: 0, y: 0 } },
    { id: 'q1', kind: 'question', type: 'single', text: 'Pick',
      options: [{ id: 'a', label: 'A' }, { id: 'b', label: 'B' }],
      next: [{ to: 'end', when: 'true' }], ui: { x: 0, y: 100 } } as never,
    { id: 'end', kind: 'success-end', next: [], ui: { x: 0, y: 200 } },
  ],
};

const server = setupServer(http.get('/api/surveys/s1', () => HttpResponse.json({
  id: 's1', name: 'Demo', project_code: 'P', active_version_id: null,
  draft_schema: seed, versions: [], status: 'active',
})));

beforeAll(() => { server.listen(); vi.spyOn(runtime, 'nextNode').mockImplementation((schema, currentId) => {
  const cur = schema.nodes.find(n => n.id === currentId);
  return cur?.next[0]?.to ?? null;
}); });
afterEach(() => server.resetHandlers()); afterAll(() => server.close());

const wrap = () => {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return (
    <QueryClientProvider client={qc}>
      <MemoryRouter initialEntries={['/admin/surveys/s1/preview']}>
        <Routes><Route path="/admin/surveys/:id/preview" element={<SurveyPreview />} /></Routes>
      </MemoryRouter>
    </QueryClientProvider>
  );
};

describe('SurveyPreview', () => {
  it('starts from the start node and shows the first question', async () => {
    render(wrap());
    await waitFor(() => expect(screen.getByText('Pick')).toBeInTheDocument());
  });

  it('selecting an option moves to the next node', async () => {
    render(wrap());
    await waitFor(() => expect(screen.getByText('Pick')).toBeInTheDocument());
    await userEvent.click(screen.getByText('A'));
    await waitFor(() => expect(screen.getByText(/Анкета успешно/i)).toBeInTheDocument());
  });
});
```

- [ ] **Step 2: Implement**

```tsx
import { useEffect, useMemo, useState } from 'react';
import { useParams } from 'react-router-dom';
import { useQuery } from '@tanstack/react-query';
import { fetchSurvey } from '../../api/surveys';
import { nextNode } from '../../runtime/surveys-runtime'; // Plan 07/15 WASM glue
import type { Node, QuestionNode, Schema } from '../../types/survey';

function isQuestion(n: Node): n is QuestionNode { return n.kind === 'question'; }

interface AnswerLog { nodeId: string; answer: unknown }

export function SurveyPreview() {
  const { id } = useParams<{ id: string }>();
  const { data: survey, isLoading } = useQuery({ queryKey: ['survey', id], queryFn: () => fetchSurvey(id!) });

  const schema: Schema | null = survey?.draft_schema ?? null;
  const startId = useMemo(() => schema?.nodes.find(n => n.kind === 'start')?.id ?? schema?.nodes[0]?.id ?? null, [schema]);

  const [currentId, setCurrentId] = useState<string | null>(null);
  const [answers, setAnswers] = useState<AnswerLog[]>([]);

  useEffect(() => { if (startId) setCurrentId(startId); setAnswers([]); }, [startId]);

  if (isLoading || !schema || !currentId) return <div className="page muted">Загрузка…</div>;

  const cur = schema.nodes.find(n => n.id === currentId)!;
  const isEnd = cur.kind === 'success-end' || cur.kind === 'refusal-end';

  const advance = (answer: unknown) => {
    const log: AnswerLog[] = [...answers, { nodeId: cur.id, answer }];
    const nxt = nextNode(schema, cur.id, answer, log);
    setAnswers(log);
    setCurrentId(nxt ?? cur.id);
  };

  return (
    <div className="page" data-screen-label="survey preview" style={{ maxWidth: 720 }}>
      <div className="page-header"><h1>{schema.title} — превью</h1></div>
      <div className="card q-card">
        <div className="q-header">
          <div className="muted mono">Узел: {cur.id} · Тип: {cur.kind}{isQuestion(cur) ? ` (${cur.type})` : ''}</div>
          <div className="q-progress"><div className="q-progress-fill" style={{ width: `${Math.min(100, answers.length * 15)}%` }} /></div>
        </div>
        <div className="q-body">
          {isEnd ? (
            <div className="q-question">
              {cur.kind === 'success-end' ? 'Анкета успешно завершена.' : 'Анкета завершена (отказ / выход).'}
            </div>
          ) : (
            <>
              <div className="q-question">{cur.text || cur.id}</div>
              {cur.hint && <div className="q-script"><div className="q-script-label">Подсказка</div>{cur.hint}</div>}
              {isQuestion(cur) && (cur.type === 'single' || cur.type === 'select') && (
                <div className="q-options">
                  {(cur.options ?? []).map(o => (
                    <div key={o.id} className="q-option" onClick={() => advance(o.id)}>
                      <div className="q-radio" /><span>{o.label}</span>
                    </div>
                  ))}
                </div>
              )}
              {isQuestion(cur) && cur.type === 'multi' && (
                <MultiSelect options={cur.options ?? []} onSubmit={a => advance(a)} />
              )}
              {isQuestion(cur) && cur.type === 'number' && (
                <NumberInput min={cur.min} max={cur.max} onSubmit={n => advance(n)} />
              )}
              {isQuestion(cur) && cur.type === 'text' && (
                <TextInput onSubmit={t => advance(t)} />
              )}
              {!isQuestion(cur) && (
                <div className="q-footer">
                  <button className="btn btn-primary" onClick={() => advance(null)}>Дальше</button>
                </div>
              )}
            </>
          )}
        </div>
      </div>
    </div>
  );
}

function MultiSelect({ options, onSubmit }: { options: { id: string; label: string }[]; onSubmit: (a: string[]) => void }) {
  const [picked, setPicked] = useState<Set<string>>(new Set());
  return (
    <>
      <div className="q-options">
        {options.map(o => (
          <div key={o.id} className={`q-option ${picked.has(o.id) ? 'selected' : ''}`}
               onClick={() => setPicked(p => { const n = new Set(p); n.has(o.id) ? n.delete(o.id) : n.add(o.id); return n; })}>
            <div className="q-radio" /><span>{o.label}</span>
          </div>
        ))}
      </div>
      <button className="btn btn-primary" onClick={() => onSubmit(Array.from(picked))} disabled={picked.size === 0}>Дальше</button>
    </>
  );
}
function NumberInput({ min, max, onSubmit }: { min?: number; max?: number; onSubmit: (n: number) => void }) {
  const [v, setV] = useState<number>(min ?? 0);
  return (
    <div className="row gap-8">
      <input className="input" type="number" min={min} max={max} value={v} onChange={e => setV(Number(e.target.value))} />
      <button className="btn btn-primary" onClick={() => onSubmit(v)}>Дальше</button>
    </div>
  );
}
function TextInput({ onSubmit }: { onSubmit: (t: string) => void }) {
  const [t, setT] = useState('');
  return (
    <div className="col gap-8">
      <textarea className="textarea" rows={3} value={t} onChange={e => setT(e.target.value)} />
      <button className="btn btn-primary" onClick={() => onSubmit(t)} disabled={!t.trim()}>Дальше</button>
    </div>
  );
}
```

If Plan 15 ships the runtime as a WASM module that is async-loaded, replace the synchronous `nextNode` call with `await runtime.ready` followed by `runtime.nextNode(...)`. The contract assumed: `nextNode(schema, currentId, lastAnswer, allAnswers): string | null`.

- [ ] **Step 3: Test, commit**

```bash
cd web && npx vitest run src/pages/admin/__tests__/SurveyPreview.test.tsx
git add web/src/pages/admin/SurveyPreview.tsx web/src/pages/admin/__tests__/SurveyPreview.test.tsx
git commit -m "feat(web/surveys): SurveyPreview page driven by WASM runtime"
```

---

## Task 22: Routing

**Files:** modify `web/src/routes.tsx` (or wherever Plan 15 declares routes).

- [ ] **Step 1: Add survey routes**

Append to admin routes section:

```tsx
import { Surveys } from './pages/admin/Surveys';
import { SurveyBuilder } from './pages/admin/SurveyBuilder';
import { SurveyPreview } from './pages/admin/SurveyPreview';
import { useTenantSettings } from './tenant';

// inside <Route path="/admin">:
<Route path="surveys" element={<Surveys />} />
<Route path="surveys/:id" element={<SurveyBuilderRoute />} />
<Route path="surveys/:id/preview" element={<SurveyPreview />} />

// helper that injects tenant builderMode:
function SurveyBuilderRoute() {
  const t = useTenantSettings();
  return <SurveyBuilder tenantBuilderMode={t.builderMode} />;
}
```

- [ ] **Step 2: Smoke build**

```bash
cd web && npx tsc --noEmit && npx vitest run
```

Expected: all tests pass; coverage report (next task) confirms ≥ 80% for `components/survey-builder/`.

- [ ] **Step 3: Commit**

```bash
git add web/src/routes.tsx
git commit -m "feat(web/surveys): wire routes for /admin/surveys[/:id[/preview]]"
```

---

## Task 23: CSS additions

**Files:**
- Create: `web/src/styles/survey-builder.css`
- Modify: `web/src/styles/index.css` (or main entry) to `@import './survey-builder.css';`

- [ ] **Step 1: Write CSS**

```css
/* survey-builder.css */

.error-dot {
  width: 8px; height: 8px; border-radius: 50%;
  background: var(--danger);
  display: inline-block;
}

.error-dot.abs-tr {
  position: absolute; right: 6px; top: 6px;
}

.flow-node {
  /* matches prototype styles.css#.flow-node */
  position: absolute;
  background: var(--bg-card);
  border: 1.5px solid var(--border-strong);
  border-radius: var(--radius);
  padding: 12px 14px;
  min-width: 220px;
  box-shadow: var(--shadow-md);
  cursor: move;
  user-select: none;
  touch-action: none;
}
.flow-node.selected { border-color: var(--accent); box-shadow: 0 0 0 3px var(--accent-soft); }

.flow-node-port {
  position: absolute;
  bottom: -7px;
  left: 50%;
  transform: translateX(-50%);
  width: 14px;
  height: 14px;
  background: var(--accent);
  border: 2px solid var(--bg-card);
  border-radius: 50%;
  cursor: crosshair;
}
.flow-node-port:hover { background: var(--success); }

.dsl-input { position: relative; }
.dsl-input-host { border: 1px solid var(--border); border-radius: 6px; min-height: 32px; }
.dsl-ok { position: absolute; right: 8px; top: 8px; color: var(--success); font-size: 0.78em; }
.dsl-err { position: absolute; right: 8px; top: 8px; color: var(--danger); font-size: 0.78em; max-width: 60%; overflow: hidden; text-overflow: ellipsis; }

.validation-badge {
  position: relative;
  display: inline-flex;
  align-items: center;
  gap: 4px;
}
.validation-badge-count {
  background: var(--danger);
  color: white;
  border-radius: 999px;
  font-size: 0.72em;
  padding: 2px 6px;
}
.validation-badge:hover .validation-badge-tip { display: block; }
.validation-badge-tip {
  display: none;
  position: absolute;
  top: 100%; right: 0;
  background: var(--bg-card);
  border: 1px solid var(--border);
  padding: 6px 8px;
  white-space: pre-line;
  font-size: 0.82em;
  z-index: 10;
  border-radius: 4px;
}

.info-box {
  background: var(--info-soft);
  border-left: 3px solid var(--info);
  padding: 10px 12px;
  border-radius: 0 6px 6px 0;
  font-family: var(--font-mono);
  font-size: 0.9em;
}
```

- [ ] **Step 2: Commit**

```bash
git add web/src/styles/survey-builder.css web/src/styles/index.css
git commit -m "style(web/surveys): add builder-only CSS rules (error-dot, flow-node-port, dsl)"
```

---

## Task 24: Coverage gate ≥ 80% on `components/survey-builder/`

**Files:**
- Modify: `web/vitest.config.ts`

- [ ] **Step 1: Add per-folder threshold**

```ts
// vitest.config.ts (excerpt)
test: {
  // ... existing config
  coverage: {
    provider: 'v8',
    include: [
      'src/components/survey-builder/**/*.{ts,tsx}',
      'src/stores/surveyStore.ts',
      'src/hooks/useDebouncedValidation.ts',
      'src/hooks/useFlowDrag.ts',
      'src/hooks/useFlowAutoLayout.ts',
      'src/api/surveys.ts',
      'src/pages/admin/Surveys.tsx',
      'src/pages/admin/SurveyBuilder.tsx',
      'src/pages/admin/SurveyPreview.tsx',
    ],
    exclude: ['**/__tests__/**', '**/*.d.ts'],
    thresholds: {
      lines: 80,
      functions: 80,
      branches: 75,
      statements: 80,
    },
    reporter: ['text', 'html', 'lcov'],
  },
}
```

- [ ] **Step 2: Run coverage**

```bash
cd web && npx vitest run --coverage
```

Expected: pass with thresholds met. If a sub-component is below 80%, add focused tests rather than lowering the threshold.

- [ ] **Step 3: Commit**

```bash
git add web/vitest.config.ts
git commit -m "test(web/surveys): enforce 80% coverage on survey-builder paths"
```

---

## Task 25: Playwright smoke

**Files:**
- Create: `tests/e2e/admin-surveys.spec.ts`

- [ ] **Step 1: Write the test**

```ts
import { test, expect } from '@playwright/test';

test.describe('Admin · Surveys', () => {
  test('list → open → save', async ({ page }) => {
    await page.goto('/admin/surveys');
    await expect(page.getByRole('heading', { name: 'Анкеты' })).toBeVisible();
    const firstRow = page.locator('table tr').nth(1);
    await firstRow.click();

    await expect(page.getByText(/Несохранённые/)).not.toBeVisible(); // pristine open
    // Edit something:
    await page.getByLabel(/Текст вопроса/i).first().fill('Edited from e2e');
    await expect(page.getByText(/Несохранённые/)).toBeVisible();

    await page.getByRole('button', { name: /Сохранить версию/ }).click();
    await page.getByLabel('Minor').check();
    await page.getByLabel(/Описание/).fill('e2e-test');
    await page.getByRole('button', { name: /^Сохранить$/ }).click();

    await expect(page.getByText(/Несохранённые/)).not.toBeVisible();
  });

  test('mode tab switches Form ↔ Flow without losing edits', async ({ page }) => {
    await page.goto('/admin/surveys');
    await page.locator('table tr').nth(1).click();
    await page.getByLabel(/Текст вопроса/i).first().fill('keep me');
    await page.getByRole('tab', { name: /Flow/ }).click();
    await expect(page.getByText(/Граф анкеты/)).toBeVisible();
    await page.getByRole('tab', { name: /Form/ }).click();
    await expect(page.getByLabel(/Текст вопроса/i).first()).toHaveValue('keep me');
  });
});
```

- [ ] **Step 2: Add to CI flow**

If Plan 15 already wires Playwright into the CI workflow at `.github/workflows/ci.yml`, the new spec is picked up automatically. Otherwise add a separate `e2e` job that runs after `web-test`.

- [ ] **Step 3: Commit**

```bash
git add tests/e2e/admin-surveys.spec.ts
git commit -m "test(e2e): smoke for admin surveys list / builder / save"
```

---

## Task 26: Documentation pointer

**Files:**
- Modify: `web/README.md` to mention the builder.

- [ ] **Step 1: Append**

```markdown
## Survey builder

The survey builder lives under `src/pages/admin/SurveyBuilder.tsx` and
`src/components/survey-builder/`. Both Form and Flow modes share a single
zustand store (`src/stores/surveyStore.ts`) — the JSON schema is the source
of truth. Server validation runs on debounce; preview uses the WASM runtime.

See implementation plan: `docs/superpowers/plans/2026-05-06-18-frontend-survey-builder.md`.
```

- [ ] **Step 2: Commit**

```bash
git add web/README.md
git commit -m "docs(web): point to survey-builder folder structure"
```

---

## Verification checklist

Run all of the following from the repo root and the `web/` folder, in order:

```bash
# Repo-wide:
make lint && make test
# Frontend:
cd web && npm run lint && npx tsc --noEmit && npx vitest run --coverage
# E2E (assumes dev server is up; see Plan 15 for `npm run dev:e2e`):
npx playwright test tests/e2e/admin-surveys.spec.ts
```

- [ ] All Vitest suites green.
- [ ] Coverage on `src/components/survey-builder/` ≥ 80%.
- [ ] Playwright smoke green.
- [ ] `make lint` clean (no boundary violations from depguard).
- [ ] Manual smoke:
  - [ ] `/admin/surveys` lists rows from backend.
  - [ ] Click row → `/admin/surveys/:id` loads with header, mode tabs, form-builder default.
  - [ ] Edit a question → "Несохранённые изменения" appears.
  - [ ] Switch to Flow tab → graph renders, drag a node → its position is persisted in the schema; switch back to Form, the node is still there.
  - [ ] On a node missing required text, the node gets a red error dot in form-list and a red border on canvas.
  - [ ] "Сохранить версию" → modal → save → badge disappears.
  - [ ] "Превью" opens new tab, simulates the survey end-to-end.

---

## Out of scope

- **Backend** (`internal/surveys/*`) — Plan 07.
- **WASM runtime build artifact** — Plan 07/15.
- **Operator runtime UI** — Plan 16 (workstation).
- **Tenant `builderMode` setting CRUD** — Plan 14 (tenant settings UI). We only **read** it here.
- **Survey clone, archive, delete** — Plan 19 (admin actions).
- **Bulk import of existing schemas** — out of scope; future plan.

---

## Key risks

1. **WASM runtime contract drift.** The `runtime/surveys-runtime.ts` glue is owned by Plan 07/15. If the contract for `nextNode(schema, currentId, lastAnswer, allAnswers)` changes, `SurveyPreview` and the in-form preview both break. Mitigation: TypeScript signature mirrored in our types and tested with a local mock.
2. **Schema-fingerprint dirty-tracking false positives.** Inserting a key in JSON.stringify order can flip dirty even when nothing changed. Mitigation: the `_baseline` ref captures the JSON at `setSchema` time and `markSaved`; we compare full strings, not deep equality.
3. **CodeMirror bundle size.** `@codemirror/view` + autocomplete adds ~100KB gzipped. If bundle budget is tight, fall back to a plain `<input>` with regex highlight in CSS — covered by `DslInput.test.tsx` because tests query by role, not implementation.
4. **Flow canvas accessibility.** Drag-by-pointer is keyboard-inaccessible by design. We accept this tradeoff for v1; the Form mode is the keyboard-first path. The mode tabs ensure no operator is locked out of editing.
5. **Optimistic edits vs server validation race.** The 300ms debounce + sequence-number guard prevent stale validation responses from overwriting fresher ones; tests cover the case explicitly.

---

## What success looks like

By the end of this plan, an admin can:

1. Open `/admin/surveys`, see all their surveys.
2. Click into a survey, edit it in **either** Form or Flow mode, with both views always reading and writing the same JSON schema.
3. See live validation errors highlighted on individual nodes.
4. Save a new version (major or minor bump with notes).
5. Preview the survey end-to-end in a new tab, walking through it like an operator would.

The store, hooks, and components are all isolated, individually testable, and covered ≥ 80%. Plan 19 (admin actions: clone/archive) and Plan 16 (operator runtime) build on this foundation without touching it.

---

## Self-review

**Spec coverage** (against §11 full, FR-C1…FR-C10):
- §11.1 универсальная JSON-схема в `types/survey.ts`, mirrored from backend Plan 07. ✓
- §11.2 form-режим (linear-list FormBuilder) + flow-режим (graph FlowBuilder с drag-drop через `@dnd-kit/core` + canvas + edges). Оба читают/пишут единый zustand `surveyStore`. Переключение режима не теряет данных. ✓
- §11.3 conditional DSL UI: text-input с CodeMirror 6 syntax-highlight + autocomplete на `qN.value`, `answer in [...]`. На blur — debounced validate-call. ✓
- §11.4 валидация на лету (debounced 300ms `POST /api/surveys/{id}/validate`), ошибки подсвечиваются: form — красная рамка + tooltip; flow — красная обводка узла + tooltip с описанием. ✓
- §11.5 runtime через WASM (Plan 15 wasm.ts loader): preview-режим использует тот же runtime что и operator workstation. ✓
- §11.6 версионирование: SaveVersionModal с major/minor выбором + label, `POST /api/surveys/{id}/versions` с body=schema. ✓
- §11.7 preview: новая вкладка `/surveys/{id}/preview`, симуляция как Workstation-анкета без real-call-state. ✓
- FR-C1–C10 все требования (CRUD анкет, типы вопросов intro/single/multi/number/text/select, hints, required-флаг, condition DSL, flow-узлы start/question/text-block/success-end/refusal-end/condition/jump, версии, превью, валидация). ✓
- Pages: AdminSurveys (list table), SurveyBuilder (wrapper), SurveyPreview. Components: FormBuilder (3-колоночный), FlowBuilder (3-колоночный canvas), QuestionPalette, SchemaPropsPanel, DslInput, FlowCanvas, FlowNode, FlowEdge, SaveVersionModal. ✓
- State: `stores/surveyStore.ts` с actions addNode/removeNode/updateNode/addEdge/removeEdge/updateEdge/selectNode/setSchema/markSaved. ✓
- Hooks: `useDebouncedValidation`, `useFlowDrag`, `useFlowAutoLayout` (Sugiyama для авто-раскладки новых узлов). ✓
- Coverage `components/survey-builder/` ≥ 80%. Drag-drop тестируется через `@dnd-kit/testing-library`. Canvas-edges обновляются при move узла. Schema state-machine: 30+ кейсов. ✓

**Placeholder scan:** Plan 19 (admin actions: clone/archive) и Plan 16 (operator runtime) явно помечены как зависимые consumers — но не блокирующие.

**Type/name consistency:** `Schema`, `Node`, `Edge`, `Question`, `Option` — типы единые между frontend и backend (Plan 07). Имена компонентов FormBuilder/FlowBuilder/QuestionPalette стабильны и не пересекаются с другими модулями.

**Out of scope (correctly deferred):**
- WASM-runtime как технология — Plan 07 (backend) + Plan 15 (frontend loader).
- Survey runtime в operator workstation — Plan 16.
- Clone/archive операции — Plan 19.

Plan 18 verified.

---

**Plan complete and saved to `docs/superpowers/plans/2026-05-06-18-frontend-survey-builder.md`.**

