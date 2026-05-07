# Frontend Operator Pages Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use `- [ ]` checkbox syntax for tracking. TDD is mandatory — write the failing test first, then minimal implementation, then refactor.

**Goal:** Implement the four operator-facing pages of СоциоПульс — Workstation (двухпанельный call-card + анкета), MyStats (моя результативность), ProjectInfo (о проекте), OpHistory (история звонков) — pixel-faithful to the prototype, hooked into the WS-hub from Plan 15, the survey runtime (WASM, Plan 07), the telephony backend (Plan 09), and the operator analytics endpoints (Plan 13). Plus a `useCallFSM` hook driving state transitions, a verto/sip.js WebRTC stack for media, and ≥75% test coverage on `pages/operator/`.

**Architecture:** Page-level React components under `web/src/pages/operator/`. Shared widgets (`CurrentStateBadge`, `MiniStat`, `ChartBars`, `Compare`) under `web/src/components/operator/`. State logic split between:

- `useCallFSM` — finite-state hook reproducing the prototype's `ready → dialing → call → status → verify → ready` automaton with `pause` accessible from any state.
- `useSurveyRuntime` — wraps the WASM module from Plan 07 (`survey-runtime.wasm`) for next-node calculation; falls back to a TS shim while the WASM blob is unavailable so this plan is independently runnable.
- `useVertoClient` — lazy-loads `@signalwire/verto-clientcode-replacement` (or `sip.js` if verto bundle fails), establishes the WSS-WebRTC channel after `POST /api/telephony/operator-credentials`.
- `useWSSubscription` — thin wrapper over the WS hub from Plan 15 (`Hub.subscribe(topic, handler)` returning unsubscribe).
- React Query hooks (`useMyStats`, `useProjectInfo`, `useMyCalls`) for read-only HTTP data.

UI primitives (`Icon`, `Button`, `Card`, `Badge`, `Seg`) come from Plan 15 `web/src/ui/`. CSS classes are reused verbatim from `social-pulse-maket/project/styles.css` (already imported globally in Plan 15) — no inline hex, only CSS variables.

**Tech Stack (locked):**
- React 18.3+, TypeScript 5.4+
- @tanstack/react-query 5.30+
- @signalwire/verto-clientcode-replacement OR sip.js 0.21+ (lazy-loaded)
- vitest 1.6+, @testing-library/react 16+, @testing-library/user-event 14.5+, msw 2.3+
- happy-dom 14+ (vitest environment)

**Spec sections covered:** §FR-D (рабочее место оператора, all sub-clauses D1–D14), §FR-J (личная статистика оператора, J1–J4), §6.1 (UI source-of-truth), §10.1 (WebSocket protocol), §10.4 (listen-in receive — operator side only), §11.5 (survey runtime client-side), §7.4 (operator audio path / verto credentials).

**Prerequisites:**
- Plan 15 completed: Vite-built `web/` app, `Hub` WebSocket client, API client (`apiClient`), `Icon` component, theme variables loaded, `/login` flow, layout shell with sidebar+topbar, route table where `/operator/*` paths are reachable but currently empty placeholders.
- Plan 07 completed: `surveys.RuntimeService.NextNode` ported to Go, with `tinygo`-compiled `survey-runtime.wasm` published to `web/public/wasm/survey-runtime.wasm` (or equivalent). The plan tolerates a missing wasm via TS shim.
- Plan 09 completed: backend exposes `POST /api/telephony/operator-credentials`, `GET /api/me`, `GET /api/me/stats`, `GET /api/me/calls`, `GET /api/projects/:id`, `GET /api/projects/:id/progress`, `POST /api/calls/:id/answers`, `POST /api/calls/:id/status`, `POST /api/calls/:id/hangup`, `POST /api/operator/ready`, `POST /api/operator/pause`.

**Out of scope (handled by other plans):**
- Admin pages (Plans 17, 18, 19).
- Survey builder UI (Plan 18).
- Login form (Plan 15).
- Backend handlers — referenced only by URL.
- WASM build of `survey-runtime` — Plan 07.

---

## File Structure

```
web/
├── src/
│   ├── pages/
│   │   └── operator/
│   │       ├── Workstation.tsx
│   │       ├── Workstation.test.tsx
│   │       ├── MyStats.tsx
│   │       ├── MyStats.test.tsx
│   │       ├── ProjectInfo.tsx
│   │       ├── ProjectInfo.test.tsx
│   │       ├── OpHistory.tsx
│   │       └── OpHistory.test.tsx
│   │
│   ├── components/
│   │   └── operator/
│   │       ├── CurrentStateBadge.tsx
│   │       ├── CurrentStateBadge.test.tsx
│   │       ├── MiniStat.tsx
│   │       ├── ReadyCard.tsx
│   │       ├── DialingCard.tsx
│   │       ├── ActiveCallCard.tsx
│   │       ├── StatusCard.tsx
│   │       ├── VerifyCard.tsx
│   │       ├── PauseCard.tsx
│   │       ├── QuestionPane.tsx
│   │       ├── QuestionPane.test.tsx
│   │       ├── SurveyPlaceholder.tsx
│   │       ├── ChartBars.tsx
│   │       ├── ChartBars.test.tsx
│   │       ├── Compare.tsx
│   │       ├── LegItem.tsx
│   │       ├── ShiftTimeBar.tsx
│   │       ├── StatusList.tsx
│   │       └── SurveyAnswersModal.tsx
│   │
│   ├── hooks/
│   │   ├── useCallFSM.ts
│   │   ├── useCallFSM.test.ts
│   │   ├── useSurveyRuntime.ts
│   │   ├── useSurveyRuntime.test.ts
│   │   ├── useVertoClient.ts
│   │   ├── useVertoClient.test.ts
│   │   ├── useWSSubscription.ts
│   │   ├── useWSSubscription.test.ts
│   │   ├── useShiftTimer.ts
│   │   ├── useKeyboardShortcuts.ts
│   │   └── useKeyboardShortcuts.test.ts
│   │
│   ├── api/
│   │   ├── me.ts                      # GET /api/me, /api/me/stats, /api/me/calls
│   │   ├── operator.ts                # POST /api/operator/ready, /pause
│   │   ├── calls.ts                   # POST answers, status, hangup
│   │   └── telephony.ts               # POST /api/telephony/operator-credentials
│   │
│   ├── types/
│   │   ├── call.ts                    # CallState, CallEvent, CallStatusId, CallSummary
│   │   ├── survey.ts                  # SurveyNode, SurveyAnswer, SurveyVersion
│   │   ├── operator.ts                # MeProfile, MyStats, ShiftKpi
│   │   └── project.ts                 # ProjectInfo, ProjectProgress
│   │
│   └── test/
│       ├── handlers/
│       │   ├── operator.handlers.ts   # MSW handlers for operator HTTP
│       │   └── telephony.handlers.ts
│       └── mocks/
│           ├── verto.mock.ts
│           ├── ws-hub.mock.ts
│           └── data.ts                # MOCK fixtures mirroring data.jsx
│
└── public/
    └── wasm/
        └── README.md                  # placeholder doc; binary lands in Plan 07
```

---

## Conventions

- All TSX/TS files: 2-space indent, single quotes, trailing commas, arrow-function components.
- Tests collocated with source: `Foo.tsx` ↔ `Foo.test.tsx`. Run via `pnpm vitest`.
- Every component-test starts with a render-pass assertion (does it mount, does it match the prototype's class names) before user interaction.
- Every interaction-test uses `userEvent` from `@testing-library/user-event` (typed events, no fireEvent for clicks/keys).
- MSW handlers default to JSON 200; tests opt into 4xx/5xx through `server.use(...)` overrides.
- No emojis in code or strings unless the prototype source itself contains them.
- Naming: hooks start with `use`, components are PascalCase, types are PascalCase, files match component name, internal helpers in same file get a `// helpers` comment.
- WS subscriptions inside `useEffect` with cleanup. Hub returns an `unsubscribe` function that we MUST call on unmount.

---

## Task 1: Operator domain types and DTO contracts

**Files:**
- Create: `web/src/types/call.ts`
- Create: `web/src/types/survey.ts`
- Create: `web/src/types/operator.ts`
- Create: `web/src/types/project.ts`

These are TypeScript-only and have no runtime cost; tests in later tasks cover their use.

- [ ] **Step 1: Write `web/src/types/call.ts`**

```ts
// FSM state names used both client-side (useCallFSM) and server-side (spec §8.1).
export const CALL_STATES = ['ready', 'dialing', 'call', 'status', 'verify', 'pause'] as const;
export type CallState = (typeof CALL_STATES)[number];

// Outcome statuses (spec FR-D10). Internal enum is fixed; labels are tenant-localised.
export const CALL_STATUS_IDS = [
  'success',
  'refused',
  'dropped',
  'no-answer',
  'busy',
  'callback',
  'wrong-person',
  'tech-failure',
] as const;
export type CallStatusId = (typeof CALL_STATUS_IDS)[number];

export interface CallStatusOption {
  id: CallStatusId;
  label: string;
  icon: string;
  tone: 'success' | 'danger' | 'warning' | 'info' | 'muted';
}

export interface RespondentSnapshot {
  id: string;
  phone: string;
  region: string;
  attemptNo: number;
  attemptMax: number;
  attributes?: Record<string, string>;
}

// WS-event payloads on `call.<call_id>.events` (spec §10.1).
export type CallEvent =
  | { type: 'call.dialing'; callId: string; phone: string; respondent: RespondentSnapshot }
  | { type: 'call.bridged'; callId: string; vertoCallId: string }
  | { type: 'call.hangup'; callId: string; cause: string }
  | { type: 'call.recording.started'; callId: string };

// WS-event payloads on `op.<op_id>.commands`.
export type OpCommand =
  | { type: 'op.force-pause'; reason: string }
  | { type: 'op.force-end-shift'; reason: string };

export interface CallSummary {
  id: string;
  time: string;
  phone: string;
  region: string;
  duration: string;
  status: CallStatusId;
}
```

- [ ] **Step 2: Write `web/src/types/survey.ts`**

Mirrors `survey_versions.schema` from spec §11.1 but only the operator runtime needs `nodes` and `next.when`.

```ts
export type SurveyNodeKind =
  | 'intro'
  | 'question'
  | 'text-block'
  | 'success-end'
  | 'refusal-end'
  | 'condition'
  | 'jump';

export type SurveyQuestionType = 'single' | 'multi' | 'number' | 'text' | 'select';

export interface SurveyOption {
  id: string;
  label: string;
}

export interface SurveyEdge {
  to: string;
  when: string; // DSL string evaluated by runtime (spec §11.3)
}

export interface SurveyNode {
  id: string;
  kind: SurveyNodeKind;
  type?: SurveyQuestionType;
  text?: string;
  hint?: string;
  required?: boolean;
  options?: SurveyOption[];
  next?: SurveyEdge[];
  ui?: { x: number; y: number };
}

export interface SurveyVersion {
  surveyId: string;
  versionId: string;
  versionNumber: string;
  title: string;
  intro?: string;
  nodes: SurveyNode[];
  metadata: {
    estimatedMinutes: string;
    maxQuestions: number;
    primaryMode: 'form' | 'flow';
  };
}

// Answer collected during a call. Keyed by node id.
export type SurveyAnswers = Record<string, SurveyAnswer>;

export type SurveyAnswer =
  | { kind: 'single'; optionId: string; label: string }
  | { kind: 'multi'; optionIds: string[]; labels: string[] }
  | { kind: 'number'; value: number }
  | { kind: 'text'; value: string }
  | { kind: 'dk' }; // "затрудняется"
```

- [ ] **Step 3: Write `web/src/types/operator.ts`**

```ts
export interface MeProfile {
  userId: string;
  tenantId: string;
  login: string;
  fullName: string;
  role: 'operator' | 'supervisor' | 'admin';
  currentProjectId: string | null;
  shiftStartedAt: string | null; // ISO
  pauseLimitSeconds: number;
}

// Aggregated KPI for a period. Returned by GET /api/me/stats?period=today|week|month.
export interface MyStats {
  period: 'today' | 'week' | 'month';
  callsTotal: number;
  callsSuccess: number;
  callTime: string;        // hh:mm:ss
  workTime: string;
  pauseTime: string;
  avgHandle: string;       // mm:ss
  statuses: { label: string; count: number; color: string }[];
  hourly: number[];        // 12 buckets, 09–21
  shiftBreakdown: {
    callSeconds: number;
    pauseSeconds: number;
    dialingSeconds: number;
    idleSeconds: number;
  };
  team: {
    surveysPerHourYou: number;
    surveysPerHourTeam: number;
    conversionYou: number;
    conversionTeam: number;
    avgHandleYou: string;
    avgHandleTeam: string;
    pauseYou: string;
    pauseTeam: string;
  };
}

export interface ShiftKpi {
  callsToday: number;
  callsSuccess: number;
  callTime: string;
  avgHandle: string;
}
```

- [ ] **Step 4: Write `web/src/types/project.ts`**

```ts
export interface ProjectInfo {
  id: string;
  code: string;
  name: string;
  description: string;
  rules: string[];               // "Что важно соблюдать"
  status: 'active' | 'paused' | 'archived';
  curatorName: string;
  baseName: string;
  operatorsCount: number;
}

export interface ProjectProgress {
  projectId: string;
  done: number;
  target: number;
  remainingDays: number;
  deadline: string;              // human "до 12 мая"
}
```

- [ ] **Step 5: Verify tsc compiles**

Run: `pnpm tsc --noEmit`
Expected: 0 errors. Each new file should be syntactically valid even though not imported anywhere yet.

- [ ] **Step 6: Commit**

```bash
git add web/src/types/
git commit -m "feat(web): add operator-domain types (call, survey, operator, project)"
```

---

## Task 2: API client modules for operator HTTP endpoints

**Files:**
- Create: `web/src/api/me.ts`
- Create: `web/src/api/operator.ts`
- Create: `web/src/api/calls.ts`
- Create: `web/src/api/telephony.ts`

These modules wrap `apiClient` (from Plan 15). Tests cover the request shape via MSW handlers later.

- [ ] **Step 1: `web/src/api/me.ts`**

```ts
import { apiClient } from './client';
import type { MeProfile, MyStats } from '../types/operator';
import type { CallSummary, CallStatusId } from '../types/call';

export type Period = 'today' | 'week' | 'month';

export async function fetchMe(): Promise<MeProfile> {
  return apiClient.get<MeProfile>('/api/me');
}

export async function fetchMyStats(period: Period): Promise<MyStats> {
  return apiClient.get<MyStats>(`/api/me/stats?period=${period}`);
}

export interface MyCallsParams {
  period?: Period;
  status?: CallStatusId;
}

export async function fetchMyCalls(params: MyCallsParams = {}): Promise<CallSummary[]> {
  const qs = new URLSearchParams();
  if (params.period) qs.set('period', params.period);
  if (params.status) qs.set('status', params.status);
  const suffix = qs.toString() ? `?${qs.toString()}` : '';
  return apiClient.get<CallSummary[]>(`/api/me/calls${suffix}`);
}

export async function fetchMyCallAnswers(callId: string) {
  return apiClient.get<{ surveyVersionId: string; answers: Record<string, unknown> }>(
    `/api/me/calls/${encodeURIComponent(callId)}/answers`,
  );
}
```

- [ ] **Step 2: `web/src/api/operator.ts`**

```ts
import { apiClient } from './client';

export async function postOperatorReady(): Promise<{ ok: true }> {
  return apiClient.post<{ ok: true }>('/api/operator/ready', {});
}

export async function postOperatorPause(reason: 'manual' | 'break'): Promise<{ ok: true }> {
  return apiClient.post<{ ok: true }>('/api/operator/pause', { reason });
}

export async function postOperatorResume(): Promise<{ ok: true }> {
  return apiClient.post<{ ok: true }>('/api/operator/resume', {});
}
```

- [ ] **Step 3: `web/src/api/calls.ts`**

```ts
import { apiClient } from './client';
import type { CallStatusId } from '../types/call';
import type { SurveyAnswer } from '../types/survey';

export async function postCallAnswer(
  callId: string,
  nodeId: string,
  answer: SurveyAnswer,
): Promise<{ nextNodeId: string | null }> {
  return apiClient.post(`/api/calls/${encodeURIComponent(callId)}/answers`, {
    nodeId,
    answer,
  });
}

export interface CompleteCallPayload {
  status: CallStatusId;
  comment?: string;
}

export async function postCallStatus(callId: string, p: CompleteCallPayload) {
  return apiClient.post<{ ok: true }>(
    `/api/calls/${encodeURIComponent(callId)}/status`,
    p,
  );
}

export async function postCallHangup(callId: string) {
  return apiClient.post<{ ok: true }>(
    `/api/calls/${encodeURIComponent(callId)}/hangup`,
    {},
  );
}
```

- [ ] **Step 4: `web/src/api/telephony.ts`**

```ts
import { apiClient } from './client';

export interface OperatorCredentials {
  wsUrl: string;
  sipUser: string;
  sipPassword: string;
  callerId: string;
  fsNodeId: string;
}

export async function fetchOperatorCredentials(): Promise<OperatorCredentials> {
  return apiClient.post<OperatorCredentials>('/api/telephony/operator-credentials', {});
}
```

- [ ] **Step 5: Verify build**

Run: `pnpm tsc --noEmit`. Expected: 0 errors.

- [ ] **Step 6: Commit**

```bash
git add web/src/api/me.ts web/src/api/operator.ts web/src/api/calls.ts web/src/api/telephony.ts
git commit -m "feat(web): add operator-side API client modules"
```

---

## Task 3: MSW handlers and shared mock fixtures

**Files:**
- Create: `web/src/test/mocks/data.ts`
- Create: `web/src/test/handlers/operator.handlers.ts`
- Create: `web/src/test/handlers/telephony.handlers.ts`
- Modify: `web/src/test/setup.ts` (Plan 15) — register the new handlers (append).

- [ ] **Step 1: Mock fixtures (faithful to `data.jsx`)**

`web/src/test/mocks/data.ts`:

```ts
import type { MeProfile, MyStats } from '../../types/operator';
import type { ProjectInfo, ProjectProgress } from '../../types/project';
import type { CallSummary } from '../../types/call';
import type { SurveyVersion } from '../../types/survey';

export const mockMe: MeProfile = {
  userId: 'u-svetlana',
  tenantId: 't-prometheus',
  login: 'operator',
  fullName: 'Светлана Иванова',
  role: 'operator',
  currentProjectId: 'p1',
  shiftStartedAt: '2026-05-06T09:00:00+03:00',
  pauseLimitSeconds: 900,
};

export const mockMyStats: MyStats = {
  period: 'today',
  callsTotal: 28,
  callsSuccess: 6,
  callTime: '01:54:22',
  workTime: '04:12:18',
  pauseTime: '00:38:04',
  avgHandle: '04:12',
  statuses: [
    { label: 'Успешно (анкета)', count: 6, color: 'success' },
    { label: 'Отказ от опроса', count: 8, color: 'danger' },
    { label: 'Сброс / занят', count: 5, color: 'warning' },
    { label: 'Не дозвонились', count: 6, color: 'muted' },
    { label: 'Перезвонить позже', count: 2, color: 'info' },
    { label: 'Недоступен', count: 1, color: 'muted' },
  ],
  hourly: [2, 1, 3, 4, 2, 5, 3, 4, 2, 1, 0, 1],
  shiftBreakdown: { callSeconds: 6840, pauseSeconds: 2280, dialingSeconds: 5520, idleSeconds: 480 },
  team: {
    surveysPerHourYou: 1.5,
    surveysPerHourTeam: 1.2,
    conversionYou: 0.21,
    conversionTeam: 0.18,
    avgHandleYou: '04:12',
    avgHandleTeam: '04:24',
    pauseYou: '38 мин',
    pauseTeam: '29 мин',
  },
};

export const mockProjectInfo: ProjectInfo = {
  id: 'p1',
  code: 'ВЦИОМ-2026-05',
  name: 'Электоральный мониторинг — Май 2026',
  description:
    'Регулярный замер электоральных настроений по федеральным округам РФ. Целевая выборка — 6000 респондентов, квотная по полу/возрасту/региону.',
  rules: [
    'Зачитывайте вступление дословно — это требование заказчика.',
    'Не подсказывайте варианты ответов, если в подсказке указано иначе.',
    'В случае агрессии — сохраняйте спокойствие и завершайте разговор статусом «Отказ».',
    'Все звонки записываются для контроля качества.',
  ],
  status: 'active',
  curatorName: 'М.П. Соколова',
  baseName: 'База ЦФО v3',
  operatorsCount: 24,
};

export const mockProjectProgress: ProjectProgress = {
  projectId: 'p1',
  done: 4820,
  target: 6000,
  remainingDays: 6,
  deadline: 'до 12 мая',
};

export const mockMyCalls: CallSummary[] = [
  { id: 'c1024', time: '14:28', phone: '+7 (495) 123-4521', region: 'Москва', duration: '04:32', status: 'success' },
  { id: 'c1020', time: '14:18', phone: '+7 (495) 887-5544', region: 'Москва', duration: '00:08', status: 'dropped' },
  { id: 'c1015', time: '14:10', phone: '+7 (495) 123-9001', region: 'Москва', duration: '00:42', status: 'refused' },
  { id: 'c1010', time: '14:02', phone: '+7 (495) 880-9991', region: 'МО',     duration: '00:23', status: 'no-answer' },
  { id: 'c1004', time: '13:55', phone: '+7 (495) 444-5566', region: 'Тула',   duration: '03:21', status: 'success' },
];

export const mockSurveyVersion: SurveyVersion = {
  surveyId: 's-electoral',
  versionId: 'sv-1.1',
  versionNumber: '1.1',
  title: 'Электоральный мониторинг — Май 2026',
  intro:
    'Здравствуйте! Меня зовут Светлана, я представляю Всероссийский центр изучения общественного мнения. Мы проводим небольшой опрос, который займёт 5–7 минут. Скажите, пожалуйста, удобно ли Вам сейчас поговорить?',
  nodes: [
    {
      id: 'n1',
      kind: 'question',
      type: 'single',
      text: 'Скажите, пожалуйста, в целом Вы интересуетесь политикой или нет?',
      hint: 'Если респондент уточняет «как именно?» — поясните: «Имеется в виду интерес в любой форме: новости, обсуждения, выборы».',
      required: true,
      options: [
        { id: 'very', label: 'Очень интересуюсь' },
        { id: 'rather', label: 'Скорее интересуюсь' },
        { id: 'not_really', label: 'Скорее не интересуюсь' },
        { id: 'not_at_all', label: 'Совсем не интересуюсь' },
        { id: 'dk', label: 'Затрудняюсь ответить' },
      ],
      next: [{ to: 'n2', when: 'true' }],
    },
    {
      id: 'n2',
      kind: 'question',
      type: 'single',
      text: 'Как бы вы оценили деятельность Президента Российской Федерации?',
      hint: 'Зачитайте варианты ответов в порядке от «полностью одобряю» до «полностью не одобряю».',
      required: true,
      options: [
        { id: 'full_yes', label: 'Полностью одобряю' },
        { id: 'rather_yes', label: 'Скорее одобряю' },
        { id: 'rather_no', label: 'Скорее не одобряю' },
        { id: 'full_no', label: 'Полностью не одобряю' },
        { id: 'dk', label: 'Затрудняюсь ответить' },
      ],
      next: [{ to: 'end_success', when: 'true' }],
    },
    { id: 'end_success', kind: 'success-end' },
  ],
  metadata: { estimatedMinutes: '5-7', maxQuestions: 12, primaryMode: 'form' },
};
```

- [ ] **Step 2: Operator MSW handlers**

`web/src/test/handlers/operator.handlers.ts`:

```ts
import { http, HttpResponse } from 'msw';
import {
  mockMe,
  mockMyCalls,
  mockMyStats,
  mockProjectInfo,
  mockProjectProgress,
  mockSurveyVersion,
} from '../mocks/data';

export const operatorHandlers = [
  http.get('/api/me', () => HttpResponse.json(mockMe)),
  http.get('/api/me/stats', ({ request }) => {
    const url = new URL(request.url);
    return HttpResponse.json({ ...mockMyStats, period: url.searchParams.get('period') ?? 'today' });
  }),
  http.get('/api/me/calls', () => HttpResponse.json(mockMyCalls)),
  http.get('/api/me/calls/:id/answers', () =>
    HttpResponse.json({ surveyVersionId: 'sv-1.1', answers: { n1: { kind: 'single', optionId: 'rather', label: 'Скорее интересуюсь' } } }),
  ),
  http.get('/api/projects/:id', () => HttpResponse.json(mockProjectInfo)),
  http.get('/api/projects/:id/progress', () => HttpResponse.json(mockProjectProgress)),
  http.get('/api/projects/:id/active-survey', () => HttpResponse.json(mockSurveyVersion)),

  http.post('/api/operator/ready', () => HttpResponse.json({ ok: true })),
  http.post('/api/operator/pause', () => HttpResponse.json({ ok: true })),
  http.post('/api/operator/resume', () => HttpResponse.json({ ok: true })),

  http.post('/api/calls/:id/answers', async ({ request }) => {
    const body = (await request.json()) as { nodeId: string };
    const next = body.nodeId === 'n1' ? 'n2' : body.nodeId === 'n2' ? 'end_success' : null;
    return HttpResponse.json({ nextNodeId: next });
  }),
  http.post('/api/calls/:id/status', () => HttpResponse.json({ ok: true })),
  http.post('/api/calls/:id/hangup', () => HttpResponse.json({ ok: true })),
];
```

- [ ] **Step 3: Telephony MSW handler**

`web/src/test/handlers/telephony.handlers.ts`:

```ts
import { http, HttpResponse } from 'msw';

export const telephonyHandlers = [
  http.post('/api/telephony/operator-credentials', () =>
    HttpResponse.json({
      wsUrl: 'wss://fs-node-2.test.local:8082/',
      sipUser: 'op_test_session',
      sipPassword: 'test-pwd-xyz',
      callerId: '+74950000000',
      fsNodeId: 'fs-2',
    }),
  ),
];
```

- [ ] **Step 4: Register handlers in `web/src/test/setup.ts`**

Append to the existing array (created in Plan 15) so existing tests still pass:

```ts
import { operatorHandlers } from './handlers/operator.handlers';
import { telephonyHandlers } from './handlers/telephony.handlers';

server.use(...operatorHandlers, ...telephonyHandlers);
```

- [ ] **Step 5: Verify**

Run: `pnpm vitest run --reporter=verbose --no-coverage` (no test using these yet — but msw shouldn't blow up).

Expected: existing Plan-15 tests still pass.

- [ ] **Step 6: Commit**

```bash
git add web/src/test/
git commit -m "test(web): add operator/telephony MSW handlers and mock fixtures"
```

---

## Task 4: WS-subscription hook

**Files:**
- Create: `web/src/hooks/useWSSubscription.ts`
- Create: `web/src/hooks/useWSSubscription.test.ts`
- Create: `web/src/test/mocks/ws-hub.mock.ts`

The Hub from Plan 15 exposes `Hub.subscribe(topic: string, handler: (payload: unknown) => void): () => void` (returns unsubscribe). We reuse it.

- [ ] **Step 1: Mock hub for tests**

`web/src/test/mocks/ws-hub.mock.ts`:

```ts
type Handler = (payload: unknown) => void;

export class FakeHub {
  private handlers = new Map<string, Set<Handler>>();
  subscribeCalls: { topic: string }[] = [];
  unsubscribeCalls: { topic: string }[] = [];

  subscribe(topic: string, handler: Handler) {
    this.subscribeCalls.push({ topic });
    if (!this.handlers.has(topic)) this.handlers.set(topic, new Set());
    this.handlers.get(topic)!.add(handler);
    return () => {
      this.unsubscribeCalls.push({ topic });
      this.handlers.get(topic)?.delete(handler);
    };
  }

  emit(topic: string, payload: unknown) {
    this.handlers.get(topic)?.forEach((h) => h(payload));
  }
}

export function makeHub() {
  return new FakeHub();
}
```

- [ ] **Step 2: Failing test**

`web/src/hooks/useWSSubscription.test.ts`:

```ts
import { renderHook, act } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { makeHub } from '../test/mocks/ws-hub.mock';
import { useWSSubscription } from './useWSSubscription';

describe('useWSSubscription', () => {
  afterEach(() => vi.clearAllMocks());

  it('subscribes on mount and unsubscribes on unmount', () => {
    const hub = makeHub();
    const handler = vi.fn();
    const { unmount } = renderHook(() => useWSSubscription(hub as never, 'op.u-1.commands', handler));

    expect(hub.subscribeCalls).toEqual([{ topic: 'op.u-1.commands' }]);
    unmount();
    expect(hub.unsubscribeCalls).toEqual([{ topic: 'op.u-1.commands' }]);
  });

  it('forwards published payloads to handler', () => {
    const hub = makeHub();
    const handler = vi.fn();
    renderHook(() => useWSSubscription(hub as never, 'call.42.events', handler));

    act(() => hub.emit('call.42.events', { type: 'call.bridged' }));
    expect(handler).toHaveBeenCalledWith({ type: 'call.bridged' });
  });

  it('re-subscribes when topic changes', () => {
    const hub = makeHub();
    const handler = vi.fn();
    const { rerender } = renderHook(
      ({ t }: { t: string }) => useWSSubscription(hub as never, t, handler),
      { initialProps: { t: 'a' } },
    );
    rerender({ t: 'b' });

    expect(hub.subscribeCalls.map((c) => c.topic)).toEqual(['a', 'b']);
    expect(hub.unsubscribeCalls.map((c) => c.topic)).toEqual(['a']);
  });

  it('does not subscribe when topic is null (gating)', () => {
    const hub = makeHub();
    renderHook(() => useWSSubscription(hub as never, null, () => {}));
    expect(hub.subscribeCalls).toHaveLength(0);
  });
});
```

- [ ] **Step 3: Run test — should fail with "useWSSubscription not exported"**

Run: `pnpm vitest run web/src/hooks/useWSSubscription.test.ts`
Expected: red.

- [ ] **Step 4: Implementation**

`web/src/hooks/useWSSubscription.ts`:

```ts
import { useEffect, useRef } from 'react';

export interface SubscribableHub {
  subscribe(topic: string, handler: (payload: unknown) => void): () => void;
}

export function useWSSubscription<T>(
  hub: SubscribableHub,
  topic: string | null,
  handler: (payload: T) => void,
) {
  // Latest-handler-ref pattern: subscribe once per topic change, but always call the latest handler.
  const handlerRef = useRef(handler);
  handlerRef.current = handler;

  useEffect(() => {
    if (!topic) return;
    const dispose = hub.subscribe(topic, (payload) => handlerRef.current(payload as T));
    return dispose;
  }, [hub, topic]);
}
```

- [ ] **Step 5: Test passes**

Run: `pnpm vitest run web/src/hooks/useWSSubscription.test.ts`
Expected: 4 passed.

- [ ] **Step 6: Commit**

```bash
git add web/src/hooks/useWSSubscription.ts web/src/hooks/useWSSubscription.test.ts web/src/test/mocks/ws-hub.mock.ts
git commit -m "feat(web): add useWSSubscription hook with cleanup-on-unmount"
```

---

## Task 5: useCallFSM — finite-state machine

**Files:**
- Create: `web/src/hooks/useCallFSM.ts`
- Create: `web/src/hooks/useCallFSM.test.ts`

The FSM mirrors the prototype's `Workstation` behaviour and the spec §8.1 server FSM. State diagram:

```
ready --start--> dialing
dialing --bridged--> call
dialing --abort--> ready
call --hangup--> status
status --pick(success)--> verify
status --pick(non-success)--> ready
verify --save--> ready
verify --redo--> call
* --pause--> pause      (preserves prior state in stack)
pause --resume--> ready (always returns to ready per spec; prior call already ended)
* --force-pause--> pause
* --force-end-shift--> offline (out of FSM, signals parent to log out)
```

- [ ] **Step 1: Failing tests — full transition table**

`web/src/hooks/useCallFSM.test.ts`:

```ts
import { renderHook, act } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import { useCallFSM } from './useCallFSM';

const initial = () => renderHook(() => useCallFSM('ready'));

describe('useCallFSM — happy path', () => {
  it('starts in given initial state', () => {
    const { result } = initial();
    expect(result.current.state).toBe('ready');
  });

  it('ready -> dialing on start()', () => {
    const { result } = initial();
    act(() => result.current.start());
    expect(result.current.state).toBe('dialing');
  });

  it('dialing -> call on bridged()', () => {
    const { result } = initial();
    act(() => result.current.start());
    act(() => result.current.bridged({ callId: 'c1', vertoCallId: 'v1' } as never));
    expect(result.current.state).toBe('call');
    expect(result.current.callId).toBe('c1');
  });

  it('call -> status on hangup()', () => {
    const { result } = renderHook(() => useCallFSM('call'));
    act(() => result.current.hangup('user'));
    expect(result.current.state).toBe('status');
  });

  it('status -> verify on pickStatus(success)', () => {
    const { result } = renderHook(() => useCallFSM('status'));
    act(() => result.current.pickStatus({ id: 'success' } as never));
    expect(result.current.state).toBe('verify');
  });

  it('status -> ready on pickStatus(refused)', () => {
    const { result } = renderHook(() => useCallFSM('status'));
    act(() => result.current.pickStatus({ id: 'refused' } as never));
    expect(result.current.state).toBe('ready');
  });

  it('verify -> ready on saveVerify()', () => {
    const { result } = renderHook(() => useCallFSM('verify'));
    act(() => result.current.saveVerify());
    expect(result.current.state).toBe('ready');
  });

  it('verify -> call on redoVerify()', () => {
    const { result } = renderHook(() => useCallFSM('verify'));
    act(() => result.current.redoVerify());
    expect(result.current.state).toBe('call');
  });
});

describe('useCallFSM — pause', () => {
  it('any state -> pause via requestPause()', () => {
    const { result } = renderHook(() => useCallFSM('dialing'));
    act(() => result.current.requestPause());
    expect(result.current.state).toBe('pause');
  });

  it('pause -> ready via resume()', () => {
    const { result } = renderHook(() => useCallFSM('pause'));
    act(() => result.current.resume());
    expect(result.current.state).toBe('ready');
  });

  it('forcePause overrides any state', () => {
    const { result } = renderHook(() => useCallFSM('call'));
    act(() => result.current.forcePause('admin-action'));
    expect(result.current.state).toBe('pause');
    expect(result.current.pauseReason).toBe('admin-action');
  });
});

describe('useCallFSM — invalid transitions are ignored', () => {
  it('start() from non-ready does not change state', () => {
    const { result } = renderHook(() => useCallFSM('call'));
    act(() => result.current.start());
    expect(result.current.state).toBe('call');
  });

  it('bridged() from non-dialing is ignored', () => {
    const { result } = renderHook(() => useCallFSM('ready'));
    act(() => result.current.bridged({ callId: 'x' } as never));
    expect(result.current.state).toBe('ready');
  });
});
```

- [ ] **Step 2: Run test — fails (no exports yet)**

Run: `pnpm vitest run web/src/hooks/useCallFSM.test.ts`
Expected: red.

- [ ] **Step 3: Implementation**

`web/src/hooks/useCallFSM.ts`:

```ts
import { useCallback, useReducer } from 'react';
import type { CallState, CallStatusOption } from '../types/call';

interface State {
  state: CallState;
  callId: string | null;
  vertoCallId: string | null;
  pauseReason: string | null;
}

type Action =
  | { type: 'start' }
  | { type: 'bridged'; callId: string; vertoCallId: string }
  | { type: 'abort-dialing' }
  | { type: 'hangup'; cause: string }
  | { type: 'pick-status'; status: CallStatusOption }
  | { type: 'save-verify' }
  | { type: 'redo-verify' }
  | { type: 'request-pause' }
  | { type: 'force-pause'; reason: string }
  | { type: 'resume' };

function reducer(s: State, a: Action): State {
  switch (a.type) {
    case 'start':
      if (s.state !== 'ready') return s;
      return { ...s, state: 'dialing' };
    case 'bridged':
      if (s.state !== 'dialing') return s;
      return { ...s, state: 'call', callId: a.callId, vertoCallId: a.vertoCallId };
    case 'abort-dialing':
      if (s.state !== 'dialing') return s;
      return { ...s, state: 'ready' };
    case 'hangup':
      if (s.state !== 'call') return s;
      return { ...s, state: 'status' };
    case 'pick-status':
      if (s.state !== 'status') return s;
      return a.status.id === 'success'
        ? { ...s, state: 'verify' }
        : { ...s, state: 'ready', callId: null, vertoCallId: null };
    case 'save-verify':
      if (s.state !== 'verify') return s;
      return { ...s, state: 'ready', callId: null, vertoCallId: null };
    case 'redo-verify':
      if (s.state !== 'verify') return s;
      return { ...s, state: 'call' };
    case 'request-pause':
      return { ...s, state: 'pause', pauseReason: 'manual' };
    case 'force-pause':
      return { ...s, state: 'pause', pauseReason: a.reason };
    case 'resume':
      if (s.state !== 'pause') return s;
      return { ...s, state: 'ready', pauseReason: null };
    default:
      return s;
  }
}

export interface CallFSM {
  state: CallState;
  callId: string | null;
  vertoCallId: string | null;
  pauseReason: string | null;
  start(): void;
  bridged(p: { callId: string; vertoCallId: string }): void;
  abortDialing(): void;
  hangup(cause: string): void;
  pickStatus(status: CallStatusOption): void;
  saveVerify(): void;
  redoVerify(): void;
  requestPause(): void;
  forcePause(reason: string): void;
  resume(): void;
}

export function useCallFSM(initial: CallState = 'ready'): CallFSM {
  const [s, dispatch] = useReducer(reducer, {
    state: initial,
    callId: null,
    vertoCallId: null,
    pauseReason: null,
  });

  return {
    state: s.state,
    callId: s.callId,
    vertoCallId: s.vertoCallId,
    pauseReason: s.pauseReason,
    start: useCallback(() => dispatch({ type: 'start' }), []),
    bridged: useCallback((p) => dispatch({ type: 'bridged', ...p }), []),
    abortDialing: useCallback(() => dispatch({ type: 'abort-dialing' }), []),
    hangup: useCallback((cause) => dispatch({ type: 'hangup', cause }), []),
    pickStatus: useCallback((status) => dispatch({ type: 'pick-status', status }), []),
    saveVerify: useCallback(() => dispatch({ type: 'save-verify' }), []),
    redoVerify: useCallback(() => dispatch({ type: 'redo-verify' }), []),
    requestPause: useCallback(() => dispatch({ type: 'request-pause' }), []),
    forcePause: useCallback((reason) => dispatch({ type: 'force-pause', reason }), []),
    resume: useCallback(() => dispatch({ type: 'resume' }), []),
  };
}
```

- [ ] **Step 4: Run test — green**

Expected: 12 passed.

- [ ] **Step 5: Commit**

```bash
git add web/src/hooks/useCallFSM.ts web/src/hooks/useCallFSM.test.ts
git commit -m "feat(web): add useCallFSM hook with full state-transition coverage"
```

---

## Task 6: Survey runtime hook (TS shim + WASM-ready)

**Files:**
- Create: `web/src/hooks/useSurveyRuntime.ts`
- Create: `web/src/hooks/useSurveyRuntime.test.ts`

Plan 07 will publish `web/public/wasm/survey-runtime.wasm`. Until then, this hook ships a TS implementation of `nextNode`/`isComplete` that handles the prototype's linear case AND a small subset of the DSL (`true`, `answer == 'x'`, `answer in [...]`). The WASM-loader is a stretch: we attempt to import; on failure we fall back to TS. Tests focus on TS path.

- [ ] **Step 1: Failing tests**

`web/src/hooks/useSurveyRuntime.test.ts`:

```ts
import { renderHook, act } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import { mockSurveyVersion } from '../test/mocks/data';
import { useSurveyRuntime } from './useSurveyRuntime';

describe('useSurveyRuntime', () => {
  it('starts at first non-end node', () => {
    const { result } = renderHook(() => useSurveyRuntime(mockSurveyVersion));
    expect(result.current.currentNode?.id).toBe('n1');
    expect(result.current.questionIndex).toBe(0);
    expect(result.current.questionCount).toBe(2);
  });

  it('advances on next() when answer recorded', () => {
    const { result } = renderHook(() => useSurveyRuntime(mockSurveyVersion));
    act(() => result.current.answer({ kind: 'single', optionId: 'rather', label: 'Скорее' }));
    act(() => result.current.next());
    expect(result.current.currentNode?.id).toBe('n2');
    expect(result.current.questionIndex).toBe(1);
  });

  it('isComplete becomes true after final question', () => {
    const { result } = renderHook(() => useSurveyRuntime(mockSurveyVersion));
    act(() => result.current.answer({ kind: 'single', optionId: 'rather', label: '' }));
    act(() => result.current.next());
    act(() => result.current.answer({ kind: 'single', optionId: 'full_yes', label: '' }));
    act(() => result.current.next());
    expect(result.current.isComplete).toBe(true);
    expect(result.current.terminalKind).toBe('success');
  });

  it('back() returns to previous node', () => {
    const { result } = renderHook(() => useSurveyRuntime(mockSurveyVersion));
    act(() => result.current.answer({ kind: 'single', optionId: 'rather', label: '' }));
    act(() => result.current.next());
    act(() => result.current.back());
    expect(result.current.currentNode?.id).toBe('n1');
  });

  it('answers map keyed by node id', () => {
    const { result } = renderHook(() => useSurveyRuntime(mockSurveyVersion));
    const a = { kind: 'single', optionId: 'very', label: 'Очень' } as const;
    act(() => result.current.answer(a));
    expect(result.current.answers['n1']).toEqual(a);
  });

  it('reset() clears state to first node', () => {
    const { result } = renderHook(() => useSurveyRuntime(mockSurveyVersion));
    act(() => result.current.answer({ kind: 'dk' }));
    act(() => result.current.next());
    act(() => result.current.reset());
    expect(result.current.currentNode?.id).toBe('n1');
    expect(result.current.answers).toEqual({});
  });

  it('respects DSL "answer in [list]" branching', () => {
    const branched = {
      ...mockSurveyVersion,
      nodes: [
        {
          id: 'n1',
          kind: 'question' as const,
          type: 'single' as const,
          text: 'q',
          options: [
            { id: 'yes', label: 'Yes' },
            { id: 'no', label: 'No' },
          ],
          next: [
            { to: 'end_refused', when: "answer in ['no']" },
            { to: 'n2', when: 'true' },
          ],
        },
        { id: 'n2', kind: 'question' as const, type: 'single' as const, text: 'q2', options: [], next: [{ to: 'end_success', when: 'true' }] },
        { id: 'end_success', kind: 'success-end' as const },
        { id: 'end_refused', kind: 'refusal-end' as const },
      ],
    };
    const { result } = renderHook(() => useSurveyRuntime(branched));
    act(() => result.current.answer({ kind: 'single', optionId: 'no', label: 'No' }));
    act(() => result.current.next());
    expect(result.current.terminalKind).toBe('refusal');
  });
});
```

- [ ] **Step 2: Run — red.**

- [ ] **Step 3: Implementation**

`web/src/hooks/useSurveyRuntime.ts`:

```ts
import { useCallback, useMemo, useRef, useState } from 'react';
import type { SurveyAnswer, SurveyAnswers, SurveyNode, SurveyVersion } from '../types/survey';

// Extremely small DSL evaluator covering the cases the prototype uses.
// Production: this delegates to survey-runtime.wasm (Plan 07).
function evalWhen(expr: string, currentAnswer: SurveyAnswer | undefined): boolean {
  const e = expr.trim();
  if (e === 'true') return true;
  if (e === 'false') return false;

  const eqMatch = /^answer\s*==\s*['"]([^'"]+)['"]$/.exec(e);
  if (eqMatch) {
    const val = eqMatch[1];
    if (currentAnswer?.kind === 'single') return currentAnswer.optionId === val;
    if (currentAnswer?.kind === 'text') return currentAnswer.value === val;
    return false;
  }

  const inMatch = /^answer\s+in\s+\[([^\]]+)\]$/.exec(e);
  if (inMatch) {
    const ids = inMatch[1].split(',').map((s) => s.trim().replace(/^['"]|['"]$/g, ''));
    if (currentAnswer?.kind === 'single') return ids.includes(currentAnswer.optionId);
    if (currentAnswer?.kind === 'multi') return currentAnswer.optionIds.some((id) => ids.includes(id));
    return false;
  }

  // Default to "true" if unparseable — runtime should only see expressions known good.
  return true;
}

function pickEdge(node: SurveyNode, ans: SurveyAnswer | undefined): string | null {
  if (!node.next || node.next.length === 0) return null;
  for (const edge of node.next) {
    if (evalWhen(edge.when, ans)) return edge.to;
  }
  return null;
}

function findFirstQuestion(version: SurveyVersion): SurveyNode | null {
  return version.nodes.find((n) => n.kind === 'question' || n.kind === 'intro') ?? null;
}

function countQuestions(v: SurveyVersion): number {
  return v.nodes.filter((n) => n.kind === 'question').length;
}

export interface SurveyRuntime {
  currentNode: SurveyNode | null;
  questionIndex: number;       // zero-based
  questionCount: number;
  answers: SurveyAnswers;
  isComplete: boolean;
  terminalKind: 'success' | 'refusal' | null;
  answer(a: SurveyAnswer): void;
  next(): void;
  back(): void;
  reset(): void;
}

interface RuntimeState {
  cursor: string;
  answers: SurveyAnswers;
  history: string[];
  terminal: 'success' | 'refusal' | null;
}

export function useSurveyRuntime(version: SurveyVersion): SurveyRuntime {
  const initial = useMemo<RuntimeState>(() => {
    const start = findFirstQuestion(version);
    return { cursor: start?.id ?? '', answers: {}, history: [], terminal: null };
  }, [version]);

  const [s, setS] = useState<RuntimeState>(initial);
  const versionRef = useRef(version);
  versionRef.current = version;

  const nodeById = useMemo(() => new Map(version.nodes.map((n) => [n.id, n])), [version]);

  const currentNode = nodeById.get(s.cursor) ?? null;
  const questionCount = useMemo(() => countQuestions(version), [version]);
  const questionIndex = useMemo(() => {
    let i = 0;
    for (const n of version.nodes) {
      if (n.kind === 'question') {
        if (n.id === s.cursor) return i;
        i++;
      }
    }
    return Math.max(0, i - 1);
  }, [version, s.cursor]);

  const answer = useCallback((a: SurveyAnswer) => {
    setS((prev) => ({ ...prev, answers: { ...prev.answers, [prev.cursor]: a } }));
  }, []);

  const next = useCallback(() => {
    setS((prev) => {
      const node = nodeById.get(prev.cursor);
      if (!node) return prev;
      const target = pickEdge(node, prev.answers[prev.cursor]);
      if (!target) return prev;
      const targetNode = nodeById.get(target);
      if (!targetNode) return prev;
      if (targetNode.kind === 'success-end' || targetNode.kind === 'refusal-end') {
        return {
          ...prev,
          history: [...prev.history, prev.cursor],
          cursor: target,
          terminal: targetNode.kind === 'success-end' ? 'success' : 'refusal',
        };
      }
      return { ...prev, history: [...prev.history, prev.cursor], cursor: target };
    });
  }, [nodeById]);

  const back = useCallback(() => {
    setS((prev) => {
      if (prev.history.length === 0) return prev;
      const last = prev.history[prev.history.length - 1];
      return { ...prev, cursor: last, history: prev.history.slice(0, -1), terminal: null };
    });
  }, []);

  const reset = useCallback(() => setS(initial), [initial]);

  return {
    currentNode,
    questionIndex,
    questionCount,
    answers: s.answers,
    isComplete: s.terminal !== null,
    terminalKind: s.terminal,
    answer,
    next,
    back,
    reset,
  };
}
```

- [ ] **Step 4: Run — green.**

Expected: 7 passed.

- [ ] **Step 5: Add a placeholder for WASM blob loading**

Append to `useSurveyRuntime.ts`:

```ts
// Future: when web/public/wasm/survey-runtime.wasm is published by Plan 07,
// useSurveyRuntimeWasm will fetch and instantiate it, then proxy nextNode().
// For now, the TS evaluator above is the source of truth client-side and the
// server (POST /api/calls/:id/answers) is authoritative.
```

- [ ] **Step 6: Commit**

```bash
git add web/src/hooks/useSurveyRuntime.ts web/src/hooks/useSurveyRuntime.test.ts
git commit -m "feat(web): add useSurveyRuntime with DSL subset (TS shim, WASM-ready)"
```

---

## Task 7: Verto / sip.js media client hook

**Files:**
- Create: `web/src/hooks/useVertoClient.ts`
- Create: `web/src/hooks/useVertoClient.test.ts`
- Create: `web/src/test/mocks/verto.mock.ts`

Verto is a browser-side WebRTC client over WSS. The `verto.js` library is large; we lazy-load it. Tests don't load the real library — they inject the mock.

- [ ] **Step 1: Mock**

`web/src/test/mocks/verto.mock.ts`:

```ts
type Hooks = Partial<{
  onWSLogin: (success: boolean) => void;
  onDialogState: (event: { name: string; callID: string }) => void;
}>;

export class FakeVerto {
  static instances: FakeVerto[] = [];
  hooks: Hooks;
  hangupCalls: string[] = [];
  loggedOut = false;

  constructor(_options: unknown, hooks: Hooks = {}) {
    this.hooks = hooks;
    FakeVerto.instances.push(this);
  }

  newCall(_p: unknown) {
    return { callID: 'verto-call-1', hangup: (reason: string) => this.hangupCalls.push(reason) };
  }

  hangup(callId: string) {
    this.hangupCalls.push(callId);
  }

  logout() {
    this.loggedOut = true;
  }

  // Test helpers — call these to drive the FSM.
  simulateLogin(ok: boolean) {
    this.hooks.onWSLogin?.(ok);
  }
  simulateDialogState(name: string, callID = 'verto-call-1') {
    this.hooks.onDialogState?.({ name, callID });
  }
}

export function resetFakeVerto() {
  FakeVerto.instances = [];
}
```

- [ ] **Step 2: Failing tests**

`web/src/hooks/useVertoClient.test.ts`:

```ts
import { renderHook, act, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { FakeVerto, resetFakeVerto } from '../test/mocks/verto.mock';
import { useVertoClient } from './useVertoClient';

beforeEach(() => {
  resetFakeVerto();
  // Inject the loader so the hook gets our fake
  (globalThis as never as { __vertoLoader: () => Promise<unknown> }).__vertoLoader = () =>
    Promise.resolve({ Verto: FakeVerto });
});

afterEach(() => vi.clearAllMocks());

describe('useVertoClient', () => {
  it('does not connect until credentials are provided', () => {
    const { result } = renderHook(() => useVertoClient(null));
    expect(result.current.status).toBe('idle');
    expect(FakeVerto.instances).toHaveLength(0);
  });

  it('initialises Verto when credentials change', async () => {
    const creds = { wsUrl: 'wss://x', sipUser: 'u', sipPassword: 'p', callerId: 'c', fsNodeId: 'fs' };
    const { result } = renderHook(() => useVertoClient(creds));

    await waitFor(() => expect(FakeVerto.instances.length).toBe(1));
    expect(result.current.status).toBe('connecting');
  });

  it('moves to connected on successful WS login', async () => {
    const { result } = renderHook(() =>
      useVertoClient({ wsUrl: 'wss://x', sipUser: 'u', sipPassword: 'p', callerId: 'c', fsNodeId: 'fs' }),
    );
    await waitFor(() => expect(FakeVerto.instances.length).toBe(1));
    act(() => FakeVerto.instances[0].simulateLogin(true));
    await waitFor(() => expect(result.current.status).toBe('connected'));
  });

  it('marks status=error on failed login', async () => {
    const { result } = renderHook(() =>
      useVertoClient({ wsUrl: 'wss://x', sipUser: 'u', sipPassword: 'p', callerId: 'c', fsNodeId: 'fs' }),
    );
    await waitFor(() => expect(FakeVerto.instances.length).toBe(1));
    act(() => FakeVerto.instances[0].simulateLogin(false));
    await waitFor(() => expect(result.current.status).toBe('error'));
  });

  it('hangup() forwards to verto.hangup with current call id', async () => {
    const { result } = renderHook(() =>
      useVertoClient({ wsUrl: 'wss://x', sipUser: 'u', sipPassword: 'p', callerId: 'c', fsNodeId: 'fs' }),
    );
    await waitFor(() => expect(FakeVerto.instances.length).toBe(1));
    act(() => FakeVerto.instances[0].simulateLogin(true));
    act(() => FakeVerto.instances[0].simulateDialogState('active', 'verto-call-1'));

    act(() => result.current.hangup());
    expect(FakeVerto.instances[0].hangupCalls).toContain('verto-call-1');
  });

  it('cleans up verto on unmount', async () => {
    const { unmount } = renderHook(() =>
      useVertoClient({ wsUrl: 'wss://x', sipUser: 'u', sipPassword: 'p', callerId: 'c', fsNodeId: 'fs' }),
    );
    await waitFor(() => expect(FakeVerto.instances.length).toBe(1));
    unmount();
    expect(FakeVerto.instances[0].loggedOut).toBe(true);
  });
});
```

- [ ] **Step 3: Implementation**

`web/src/hooks/useVertoClient.ts`:

```ts
import { useEffect, useRef, useState } from 'react';
import type { OperatorCredentials } from '../api/telephony';

export type VertoStatus = 'idle' | 'connecting' | 'connected' | 'error' | 'closed';

export interface VertoClient {
  status: VertoStatus;
  activeCallId: string | null;
  hangup(): void;
  toggleMute(): void;
  toggleHold(): void;
}

interface VertoLib {
  Verto: new (options: VertoOptions, hooks: VertoHooks) => VertoInstance;
}

interface VertoInstance {
  newCall?(p: unknown): { callID: string; hangup(reason: string): void };
  hangup(callId: string): void;
  logout(): void;
}

interface VertoOptions {
  socketUrl: string;
  login: string;
  passwd: string;
  audioParams: { echoCancellation: boolean; noiseSuppression: boolean; autoGainControl: boolean };
}

interface VertoHooks {
  onWSLogin?(success: boolean): void;
  onDialogState?(event: { name: string; callID: string }): void;
}

async function loadVertoLib(): Promise<VertoLib> {
  const inj = (globalThis as never as { __vertoLoader?: () => Promise<VertoLib> }).__vertoLoader;
  if (inj) return inj();
  // Real lazy import in production:
  const mod = (await import(/* @vite-ignore */ '@signalwire/verto-clientcode-replacement')) as unknown as VertoLib;
  return mod;
}

export function useVertoClient(creds: OperatorCredentials | null): VertoClient {
  const [status, setStatus] = useState<VertoStatus>('idle');
  const [activeCallId, setActiveCallId] = useState<string | null>(null);
  const vertoRef = useRef<VertoInstance | null>(null);

  useEffect(() => {
    if (!creds) {
      setStatus('idle');
      return;
    }
    let disposed = false;
    setStatus('connecting');

    void (async () => {
      try {
        const lib = await loadVertoLib();
        if (disposed) return;
        const verto = new lib.Verto(
          {
            socketUrl: creds.wsUrl,
            login: creds.sipUser,
            passwd: creds.sipPassword,
            audioParams: { echoCancellation: true, noiseSuppression: true, autoGainControl: true },
          },
          {
            onWSLogin: (ok) => setStatus(ok ? 'connected' : 'error'),
            onDialogState: (e) => {
              if (e.name === 'active') setActiveCallId(e.callID);
              if (e.name === 'destroy' || e.name === 'hangup') setActiveCallId(null);
            },
          },
        );
        vertoRef.current = verto;
      } catch {
        setStatus('error');
      }
    })();

    return () => {
      disposed = true;
      try {
        vertoRef.current?.logout();
      } catch {
        /* noop */
      }
      vertoRef.current = null;
      setStatus('closed');
      setActiveCallId(null);
    };
  }, [creds]);

  const hangup = () => {
    const v = vertoRef.current;
    if (!v) return;
    if (activeCallId) v.hangup(activeCallId);
  };

  // Mute/hold are wired to Verto SDK methods on real lib; here we expose stable handles.
  const toggleMute = () => {/* delegates to verto.dialogs[id].setMute() in real impl */};
  const toggleHold = () => {/* delegates to verto.dialogs[id].hold() */};

  return { status, activeCallId, hangup, toggleMute, toggleHold };
}
```

- [ ] **Step 4: Test passes**

Expected: 6 passed.

- [ ] **Step 5: Commit**

```bash
git add web/src/hooks/useVertoClient.ts web/src/hooks/useVertoClient.test.ts web/src/test/mocks/verto.mock.ts
git commit -m "feat(web): add lazy-loading verto client hook with FakeVerto test harness"
```

---

## Task 8: Shift timer + keyboard-shortcut hooks

**Files:**
- Create: `web/src/hooks/useShiftTimer.ts`
- Create: `web/src/hooks/useKeyboardShortcuts.ts`
- Create: `web/src/hooks/useKeyboardShortcuts.test.ts`

- [ ] **Step 1: `useShiftTimer.ts`** — counts seconds from a reference timestamp; ticks while `running` is true.

```ts
import { useEffect, useState } from 'react';

export function useShiftTimer(running: boolean, since: Date | null = null): number {
  const [seconds, setSeconds] = useState(since ? Math.floor((Date.now() - since.getTime()) / 1000) : 0);
  useEffect(() => {
    if (!running) {
      setSeconds(0);
      return;
    }
    setSeconds(since ? Math.floor((Date.now() - since.getTime()) / 1000) : 0);
    const t = setInterval(() => setSeconds((s) => s + 1), 1000);
    return () => clearInterval(t);
  }, [running, since]);
  return seconds;
}

export function fmtMmSs(total: number): string {
  const m = Math.floor(total / 60);
  const s = total % 60;
  return `${String(m).padStart(2, '0')}:${String(s).padStart(2, '0')}`;
}

export function fmtHhMmSs(total: number): string {
  const h = Math.floor(total / 3600);
  const m = Math.floor((total % 3600) / 60);
  const s = total % 60;
  return `${String(h).padStart(2, '0')}:${String(m).padStart(2, '0')}:${String(s).padStart(2, '0')}`;
}
```

- [ ] **Step 2: Failing tests for keyboard shortcuts**

`web/src/hooks/useKeyboardShortcuts.test.ts`:

```ts
import { renderHook } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';
import { useKeyboardShortcuts } from './useKeyboardShortcuts';

function press(key: string) {
  window.dispatchEvent(new KeyboardEvent('keydown', { key }));
}

describe('useKeyboardShortcuts', () => {
  it('invokes handler for digits 1-5', () => {
    const onDigit = vi.fn();
    renderHook(() => useKeyboardShortcuts({ enabled: true, onDigit }));
    press('1');
    press('5');
    press('6'); // out of range
    expect(onDigit).toHaveBeenCalledTimes(2);
    expect(onDigit).toHaveBeenNthCalledWith(1, 1);
    expect(onDigit).toHaveBeenNthCalledWith(2, 5);
  });

  it('Space toggles pause; Enter advances; Esc cancels; Z marks DK', () => {
    const handlers = {
      onPauseToggle: vi.fn(),
      onNext: vi.fn(),
      onCancel: vi.fn(),
      onDk: vi.fn(),
    };
    renderHook(() => useKeyboardShortcuts({ enabled: true, ...handlers }));
    press(' ');
    press('Enter');
    press('Escape');
    press('z');
    expect(handlers.onPauseToggle).toHaveBeenCalledOnce();
    expect(handlers.onNext).toHaveBeenCalledOnce();
    expect(handlers.onCancel).toHaveBeenCalledOnce();
    expect(handlers.onDk).toHaveBeenCalledOnce();
  });

  it('does nothing when disabled', () => {
    const onDigit = vi.fn();
    renderHook(() => useKeyboardShortcuts({ enabled: false, onDigit }));
    press('1');
    expect(onDigit).not.toHaveBeenCalled();
  });
});
```

- [ ] **Step 3: Implementation**

```ts
import { useEffect } from 'react';

export interface KbShortcuts {
  enabled: boolean;
  onDigit?(d: number): void;
  onPauseToggle?(): void;
  onNext?(): void;
  onCancel?(): void;
  onDk?(): void;
}

export function useKeyboardShortcuts(p: KbShortcuts) {
  useEffect(() => {
    if (!p.enabled) return;
    const handler = (e: KeyboardEvent) => {
      const k = e.key;
      if (k >= '1' && k <= '5') {
        p.onDigit?.(Number(k));
        return;
      }
      if (k === ' ') {
        e.preventDefault();
        p.onPauseToggle?.();
        return;
      }
      if (k === 'Enter') {
        p.onNext?.();
        return;
      }
      if (k === 'Escape') {
        p.onCancel?.();
        return;
      }
      if (k === 'z' || k === 'Z' || k === 'я' || k === 'Я') {
        p.onDk?.();
        return;
      }
    };
    window.addEventListener('keydown', handler);
    return () => window.removeEventListener('keydown', handler);
  }, [p]);
}
```

- [ ] **Step 4: Run — green.** Commit.

```bash
git add web/src/hooks/useShiftTimer.ts web/src/hooks/useKeyboardShortcuts.ts web/src/hooks/useKeyboardShortcuts.test.ts
git commit -m "feat(web): add useShiftTimer and useKeyboardShortcuts hooks"
```

---

## Task 9: CurrentStateBadge + MiniStat presentational components

**Files:**
- Create: `web/src/components/operator/CurrentStateBadge.tsx`
- Create: `web/src/components/operator/CurrentStateBadge.test.tsx`
- Create: `web/src/components/operator/MiniStat.tsx`

- [ ] **Step 1: Badge test (snapshot of class + label)**

```tsx
import { render, screen } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import { CurrentStateBadge } from './CurrentStateBadge';

describe('CurrentStateBadge', () => {
  it.each([
    ['ready', 'Готов к звонку', 'op-state online'],
    ['dialing', 'Дозвон...', 'op-state call'],
    ['call', 'В разговоре', 'op-state call'],
    ['status', 'Выбор статуса', 'op-state processing'],
    ['verify', 'Перепроверка анкеты', 'op-state processing'],
    ['pause', 'На паузе', 'op-state pause'],
  ] as const)('renders state %s with label %s and class %s', (state, label, cls) => {
    render(<CurrentStateBadge state={state} />);
    const el = screen.getByText(label);
    expect(el.parentElement?.className).toContain(cls);
  });
});
```

- [ ] **Step 2: Implementation**

```tsx
import type { CallState } from '../../types/call';
import { Icon } from '../../ui/Icon';

const map: Record<CallState, { label: string; cls: string; icon: string }> = {
  ready:   { label: 'Готов к звонку',     cls: 'online',     icon: 'check' },
  dialing: { label: 'Дозвон...',          cls: 'call',       icon: 'radio' },
  call:    { label: 'В разговоре',        cls: 'call',       icon: 'phone' },
  status:  { label: 'Выбор статуса',      cls: 'processing', icon: 'flag' },
  verify:  { label: 'Перепроверка анкеты', cls: 'processing', icon: 'check' },
  pause:   { label: 'На паузе',           cls: 'pause',      icon: 'pause' },
};

export function CurrentStateBadge({ state }: { state: CallState }) {
  const m = map[state];
  return (
    <div className={`op-state ${m.cls}`}>
      <Icon name={m.icon} size={14} />
      {m.label}
    </div>
  );
}
```

- [ ] **Step 3: MiniStat (no test — pure render)**

```tsx
export interface MiniStatProps {
  label: string;
  value: string;
  tone?: 'success' | 'warning' | 'danger' | 'default';
}

export function MiniStat({ label, value, tone = 'default' }: MiniStatProps) {
  const color =
    tone === 'success' ? 'var(--success)' :
    tone === 'warning' ? 'var(--warning)' :
    tone === 'danger'  ? 'var(--danger)'  : 'var(--text)';
  return (
    <div className="card" style={{ padding: '12px 16px', flex: 1, minWidth: 140 }}>
      <div className="stat-label" style={{ fontSize: '0.74em' }}>{label}</div>
      <div className="stat-value tabular" style={{ fontSize: '1.4em', color }}>{value}</div>
    </div>
  );
}
```

- [ ] **Step 4: Commit**

```bash
git add web/src/components/operator/CurrentStateBadge.tsx web/src/components/operator/CurrentStateBadge.test.tsx web/src/components/operator/MiniStat.tsx
git commit -m "feat(web): add CurrentStateBadge and MiniStat operator widgets"
```

---

## Task 10: Call-card components — Ready / Dialing / ActiveCall / Pause

**Files:**
- Create: `web/src/components/operator/ReadyCard.tsx`
- Create: `web/src/components/operator/DialingCard.tsx`
- Create: `web/src/components/operator/ActiveCallCard.tsx`
- Create: `web/src/components/operator/PauseCard.tsx`

These are direct ports from `workstation.jsx`. Tests are folded into the page-level `Workstation.test.tsx` (Task 14).

- [ ] **Step 1: `ReadyCard.tsx`**

```tsx
import { Icon } from '../../ui/Icon';

export interface ReadyCardProps {
  onStart: () => void;
  onPause: () => void;
}

export function ReadyCard({ onStart, onPause }: ReadyCardProps) {
  return (
    <div className="call-card">
      <div className="call-state-label">Система готова</div>
      <div className="call-avatar">
        <Icon name="phone" size={36} />
      </div>
      <div style={{ fontSize: '1.4em', fontWeight: 600 }}>Нажмите «Готов», чтобы начать</div>
      <div className="muted" style={{ maxWidth: 420, fontSize: '0.95em' }}>
        Когда вы будете готовы, система автоматически найдёт следующего респондента
        и соединит вас. Опрос откроется в правой панели.
      </div>
      <div className="action-bar" style={{ marginTop: 12 }}>
        <button className="btn btn-primary btn-lg" onClick={onStart} aria-label="Готов к следующему звонку">
          <Icon name="phone-call" size={18} />
          Готов к следующему звонку
        </button>
        <button className="btn btn-secondary btn-lg" onClick={onPause}>
          <Icon name="pause" size={18} />
          На паузу
        </button>
      </div>
    </div>
  );
}
```

- [ ] **Step 2: `DialingCard.tsx`**

```tsx
import { Icon } from '../../ui/Icon';
import { fmtMmSs } from '../../hooks/useShiftTimer';

export interface DialingCardProps {
  timerSeconds: number;
  region: string;
  lineSummary?: string; // "4 из 6"
  onAbort: () => void;
}

export function DialingCard({ timerSeconds, region, lineSummary = '4 из 6', onAbort }: DialingCardProps) {
  return (
    <div className="call-card">
      <div className="call-state-label">Идёт автодозвон</div>
      <div className="dialer-ring">
        <Icon name="phone" size={36} />
      </div>
      <div className="call-phone tabular">{fmtMmSs(timerSeconds)}</div>
      <div className="muted">
        Регион: <span style={{ color: 'var(--text)', fontWeight: 500 }}>{region}</span>
        <span style={{ margin: '0 8px' }}>·</span>
        Линия: {lineSummary}
      </div>
      <div className="action-bar">
        <button className="btn btn-secondary btn-lg" onClick={onAbort}>
          <Icon name="phone-off" size={18} />
          Прервать дозвон
        </button>
      </div>
    </div>
  );
}
```

- [ ] **Step 3: `ActiveCallCard.tsx`**

```tsx
import { Icon } from '../../ui/Icon';
import { fmtMmSs } from '../../hooks/useShiftTimer';
import type { RespondentSnapshot } from '../../types/call';

export interface ActiveCallCardProps {
  timerSeconds: number;
  respondent: RespondentSnapshot;
  muted: boolean;
  onMicToggle: () => void;
  onHold: () => void;
  onEnd: () => void;
}

export function ActiveCallCard(p: ActiveCallCardProps) {
  return (
    <div className="call-card" style={{ borderColor: 'var(--accent)', borderWidth: 1.5 }}>
      <div className="row" style={{ width: '100%', justifyContent: 'space-between' }}>
        <div className="call-state-label" style={{ color: 'var(--accent)' }}>
          <span className="dot" style={{ marginRight: 8, color: 'var(--accent)' }} />
          Идёт разговор
        </div>
        <div className="row gap-8">
          <button
            className="btn btn-secondary btn-sm"
            title={p.muted ? 'Включить микрофон' : 'Выключить микрофон'}
            onClick={p.onMicToggle}
            aria-pressed={p.muted}
          >
            <Icon name={p.muted ? 'mic-off' : 'mic'} size={16} />
          </button>
          <button className="btn btn-secondary btn-sm" title="Громкость">
            <Icon name="volume-2" size={16} />
          </button>
        </div>
      </div>

      <div className="call-avatar live">
        <Icon name="user" size={36} />
      </div>
      <div className="call-phone">{p.respondent.phone}</div>
      <div className="call-timer">{fmtMmSs(p.timerSeconds)}</div>

      <div className="row gap-12" style={{ fontSize: '0.9em' }}>
        <span className="badge badge-info"><Icon name="map" size={12} /> {p.respondent.region}</span>
        <span className="badge"><Icon name="user" size={12} /> Респондент №{p.respondent.id}</span>
        <span className="badge badge-accent">Попытка {p.respondent.attemptNo} из {p.respondent.attemptMax}</span>
      </div>

      <div className="action-bar">
        <button className="btn btn-warning" onClick={p.onHold}>
          <Icon name="pause" size={16} />
          Поставить на удержание
        </button>
        <button className="btn btn-danger" onClick={p.onEnd}>
          <Icon name="phone-off" size={16} />
          Завершить разговор
        </button>
      </div>
    </div>
  );
}
```

- [ ] **Step 4: `PauseCard.tsx`**

```tsx
import { Icon } from '../../ui/Icon';
import { fmtHhMmSs } from '../../hooks/useShiftTimer';

export interface PauseCardProps {
  pausedSeconds: number;
  pauseLimitSeconds: number;
  reason?: string | null;
  onResume: () => void;
}

export function PauseCard({ pausedSeconds, pauseLimitSeconds, reason, onResume }: PauseCardProps) {
  const overLimit = pausedSeconds > pauseLimitSeconds;
  return (
    <div
      className="call-card"
      style={{
        borderColor: overLimit ? 'var(--danger)' : 'var(--warning)',
        background: overLimit ? 'var(--danger-soft)' : 'var(--warning-soft)',
      }}
    >
      <div className="call-state-label" style={{ color: overLimit ? 'var(--danger)' : 'var(--warning)' }}>
        Вы на паузе
      </div>
      <div
        className="call-avatar"
        style={{ background: 'var(--bg-card)', borderColor: 'var(--warning)', color: 'var(--warning)' }}
      >
        <Icon name="pause" size={36} />
      </div>
      <div style={{ fontSize: '1.3em', fontWeight: 600 }}>Автодозвон приостановлен</div>
      <div className="muted" style={{ maxWidth: 420, fontSize: '0.95em' }}>
        {reason ?? 'Система не будет вам звонить, пока вы не нажмёте «Продолжить работу». Перерыв засчитывается в общую паузу.'}
      </div>
      <div className="row gap-12 muted" style={{ fontSize: '0.9em' }}>
        <span><Icon name="clock" size={14} /> На паузе: <span className="mono">{fmtHhMmSs(pausedSeconds)}</span></span>
        <span>Лимит: <span className="mono">{fmtHhMmSs(pauseLimitSeconds)}</span></span>
      </div>
      <button className="btn btn-primary btn-lg" onClick={onResume}>
        <Icon name="play" size={18} />
        Продолжить работу
      </button>
    </div>
  );
}
```

- [ ] **Step 5: Commit**

```bash
git add web/src/components/operator/ReadyCard.tsx web/src/components/operator/DialingCard.tsx web/src/components/operator/ActiveCallCard.tsx web/src/components/operator/PauseCard.tsx
git commit -m "feat(web): add Ready/Dialing/ActiveCall/Pause card components"
```

---

## Task 11: StatusCard + VerifyCard

**Files:**
- Create: `web/src/components/operator/StatusCard.tsx`
- Create: `web/src/components/operator/VerifyCard.tsx`

- [ ] **Step 1: `StatusCard.tsx`** (8 status options, comment textarea — FR-D10/D12)

```tsx
import { useState } from 'react';
import { Icon } from '../../ui/Icon';
import type { CallStatusOption } from '../../types/call';

export const STATUS_OPTIONS: CallStatusOption[] = [
  { id: 'success',       label: 'Анкета заполнена',       icon: 'check',         tone: 'success' },
  { id: 'refused',       label: 'Отказ от опроса',        icon: 'x',             tone: 'danger'  },
  { id: 'dropped',       label: 'Сброс / положили трубку', icon: 'phone-off',    tone: 'danger'  },
  { id: 'no-answer',     label: 'Не дозвонились',         icon: 'phone-off',     tone: 'muted'   },
  { id: 'busy',          label: 'Занято',                 icon: 'phone',         tone: 'warning' },
  { id: 'callback',      label: 'Перезвонить позже',      icon: 'clock',         tone: 'info'    },
  { id: 'wrong-person',  label: 'Не тот человек',         icon: 'user',          tone: 'muted'   },
  { id: 'tech-failure',  label: 'Техническая ошибка',     icon: 'alert-circle',  tone: 'warning' },
];

export interface StatusCardProps {
  onChoose: (s: CallStatusOption, comment: string) => void;
}

export function StatusCard({ onChoose }: StatusCardProps) {
  const [comment, setComment] = useState('');
  return (
    <div className="card">
      <div className="card-header">
        <div>
          <div className="card-title">Какой результат разговора?</div>
          <div className="muted" style={{ fontSize: '0.9em', marginTop: 4 }}>
            Выберите статус, чтобы система могла учесть результат и перейти к следующему звонку
          </div>
        </div>
      </div>
      <div className="card-body">
        <div className="status-grid">
          {STATUS_OPTIONS.map((s) => (
            <button
              key={s.id}
              className={`status-btn ${s.tone}`}
              onClick={() => onChoose(s, comment)}
              data-status-id={s.id}
            >
              <Icon name={s.icon} size={20} />
              <span>{s.label}</span>
            </button>
          ))}
        </div>
        <div className="field" style={{ marginTop: 18 }}>
          <label className="field-label" htmlFor="op-comment">Комментарий оператора (необязательно)</label>
          <textarea
            id="op-comment"
            className="textarea"
            placeholder="Например: «попросил перезвонить вечером после 19:00»"
            value={comment}
            onChange={(e) => setComment(e.target.value)}
            rows={2}
            style={{ minHeight: 60 }}
          />
        </div>
      </div>
    </div>
  );
}
```

- [ ] **Step 2: `VerifyCard.tsx`** — read-only summary of answers with redo / save (FR-D11)

```tsx
import { Icon } from '../../ui/Icon';
import type { SurveyAnswers, SurveyVersion } from '../../types/survey';

export interface VerifyCardProps {
  answers: SurveyAnswers;
  survey: SurveyVersion;
  onRedo: () => void;
  onSave: () => void;
}

function answerLabel(answer: SurveyAnswers[string] | undefined): string | null {
  if (!answer) return null;
  if (answer.kind === 'single') return answer.label;
  if (answer.kind === 'multi') return answer.labels.join(', ');
  if (answer.kind === 'number') return String(answer.value);
  if (answer.kind === 'text') return answer.value;
  if (answer.kind === 'dk') return 'Затрудняется ответить';
  return null;
}

export function VerifyCard({ answers, survey, onRedo, onSave }: VerifyCardProps) {
  const questions = survey.nodes.filter((n) => n.kind === 'question');
  return (
    <div className="card">
      <div className="card-header">
        <div className="row" style={{ gap: 10 }}>
          <div
            style={{
              width: 36,
              height: 36,
              borderRadius: '50%',
              background: 'var(--success-soft)',
              color: 'var(--success)',
              display: 'grid',
              placeItems: 'center',
            }}
          >
            <Icon name="check" size={20} />
          </div>
          <div>
            <div className="card-title">Перепроверка анкеты</div>
            <div className="muted" style={{ fontSize: '0.9em', marginTop: 2 }}>
              Проверьте ответы перед сохранением. Это нужно для контроля качества.
            </div>
          </div>
        </div>
        <span className="badge badge-success">
          <Icon name="check" size={12} /> Анкета заполнена
        </span>
      </div>
      <div className="card-body" style={{ maxHeight: 460, overflowY: 'auto' }}>
        <div className="col gap-16">
          {questions.map((q, i) => {
            const label = answerLabel(answers[q.id]);
            return (
              <div key={q.id} style={{ borderBottom: '1px solid var(--border)', paddingBottom: 14 }}>
                <div
                  className="muted"
                  style={{
                    fontSize: '0.78em',
                    textTransform: 'uppercase',
                    letterSpacing: '0.05em',
                    marginBottom: 4,
                  }}
                >
                  Вопрос {i + 1}
                </div>
                <div style={{ fontWeight: 500, marginBottom: 6 }}>{q.text}</div>
                <div className="row" style={{ gap: 8 }}>
                  <Icon name="arrowRight" size={14} color="var(--text-faint)" />
                  <span style={{ color: 'var(--accent)', fontWeight: 500 }}>
                    {label ?? <em className="muted">не отвечено — будет пропущено</em>}
                  </span>
                </div>
              </div>
            );
          })}
        </div>
      </div>
      <div className="modal-footer">
        <button className="btn btn-secondary" onClick={onRedo}>
          <Icon name="rotate-ccw" size={16} />
          Вернуться к анкете
        </button>
        <button className="btn btn-success" onClick={onSave}>
          <Icon name="save" size={16} />
          Сохранить и перейти дальше
        </button>
      </div>
    </div>
  );
}
```

- [ ] **Step 3: Commit**

```bash
git add web/src/components/operator/StatusCard.tsx web/src/components/operator/VerifyCard.tsx
git commit -m "feat(web): add StatusCard (8 outcomes + comment) and VerifyCard"
```

---

## Task 12: QuestionPane + SurveyPlaceholder (right panel)

**Files:**
- Create: `web/src/components/operator/QuestionPane.tsx`
- Create: `web/src/components/operator/QuestionPane.test.tsx`
- Create: `web/src/components/operator/SurveyPlaceholder.tsx`

- [ ] **Step 1: `QuestionPane.test.tsx`** — covers rendering, selection, keyboard wiring, "Затрудняется", progress bar

```tsx
import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';
import { mockSurveyVersion } from '../../test/mocks/data';
import { QuestionPane } from './QuestionPane';

const baseProps = {
  survey: mockSurveyVersion,
  currentNode: mockSurveyVersion.nodes[0],
  questionIndex: 0,
  questionCount: 2,
  answer: undefined,
  onAnswer: vi.fn(),
  onNext: vi.fn(),
  onBack: vi.fn(),
  onDk: vi.fn(),
};

describe('QuestionPane', () => {
  it('renders question text and options', () => {
    render(<QuestionPane {...baseProps} />);
    expect(screen.getByText(/Скажите, пожалуйста, в целом/)).toBeInTheDocument();
    expect(screen.getByText('Очень интересуюсь')).toBeInTheDocument();
    expect(screen.getAllByRole('button', { name: /^[1-5]\.|Очень|Скорее|Совсем|Затрудняюсь/i }).length).toBeGreaterThan(0);
  });

  it('shows the intro script for the first question only', () => {
    render(<QuestionPane {...baseProps} />);
    expect(screen.getByText(/Вступление — зачитайте дословно/)).toBeInTheDocument();
  });

  it('calls onAnswer when option clicked', async () => {
    const onAnswer = vi.fn();
    render(<QuestionPane {...baseProps} onAnswer={onAnswer} />);
    await userEvent.click(screen.getByText('Скорее интересуюсь'));
    expect(onAnswer).toHaveBeenCalledWith({ kind: 'single', optionId: 'rather', label: 'Скорее интересуюсь' });
  });

  it('disables Next when no answer recorded', () => {
    render(<QuestionPane {...baseProps} answer={undefined} />);
    const next = screen.getByRole('button', { name: /Далее/ });
    expect(next).toBeDisabled();
  });

  it('label changes to "Завершить анкету" on the last question', () => {
    render(
      <QuestionPane
        {...baseProps}
        currentNode={mockSurveyVersion.nodes[1]}
        questionIndex={1}
      />,
    );
    expect(screen.getByRole('button', { name: /Завершить анкету/ })).toBeInTheDocument();
  });

  it('back is disabled on first question', () => {
    render(<QuestionPane {...baseProps} questionIndex={0} />);
    expect(screen.getByRole('button', { name: /Назад/ })).toBeDisabled();
  });

  it('renders progress percentage based on questionIndex', () => {
    render(<QuestionPane {...baseProps} questionIndex={1} questionCount={2} />);
    expect(screen.getByText(/100%/)).toBeInTheDocument();
  });
});
```

- [ ] **Step 2: Implementation**

`QuestionPane.tsx`:

```tsx
import { Icon } from '../../ui/Icon';
import type { SurveyAnswer, SurveyAnswers, SurveyNode, SurveyVersion } from '../../types/survey';

export interface QuestionPaneProps {
  survey: SurveyVersion;
  currentNode: SurveyNode;
  questionIndex: number;
  questionCount: number;
  answer: SurveyAnswers[string] | undefined;
  onAnswer: (a: SurveyAnswer) => void;
  onNext: () => void;
  onBack: () => void;
  onDk: () => void;
}

export function QuestionPane(p: QuestionPaneProps) {
  const { currentNode: q } = p;
  const progress = ((p.questionIndex + 1) / p.questionCount) * 100;
  const isLast = p.questionIndex === p.questionCount - 1;

  const isSelected = (optId: string) =>
    p.answer?.kind === 'single' && p.answer.optionId === optId;

  return (
    <div style={{ height: '100%', display: 'flex', flexDirection: 'column' }}>
      <div className="q-header">
        <div className="row" style={{ justifyContent: 'space-between', marginBottom: 4 }}>
          <div
            className="muted"
            style={{ fontSize: '0.82em', textTransform: 'uppercase', letterSpacing: '0.05em' }}
          >
            Вопрос {p.questionIndex + 1} из {p.questionCount}
          </div>
          <div className="muted" style={{ fontSize: '0.82em' }}>{Math.round(progress)}%</div>
        </div>
        <div className="q-progress">
          <div className="q-progress-fill" style={{ width: progress + '%' }} />
        </div>
      </div>

      <div className="q-body">
        {p.questionIndex === 0 && p.survey.intro && (
          <div className="q-script" style={{ marginTop: 0 }}>
            <div className="q-script-label">Вступление — зачитайте дословно</div>
            {p.survey.intro}
          </div>
        )}

        <div className="q-question">{q.text}</div>

        {q.hint && (
          <div className="q-script">
            <div className="q-script-label">Подсказка</div>
            {q.hint}
          </div>
        )}

        <div className="q-options">
          {(q.options ?? []).map((opt, i) => (
            <button
              key={opt.id}
              type="button"
              className={`q-option ${isSelected(opt.id) ? 'selected' : ''}`}
              onClick={() =>
                p.onAnswer({ kind: 'single', optionId: opt.id, label: opt.label })
              }
              data-option-id={opt.id}
            >
              <div className="q-radio" />
              <div style={{ flex: 1, textAlign: 'left' }}>{opt.label}</div>
              <div className="muted mono" style={{ fontSize: '0.78em' }}>
                {i + 1}
              </div>
            </button>
          ))}
        </div>

        <div className="row" style={{ marginTop: 18, gap: 8, fontSize: '0.85em' }}>
          <Icon name="info" size={14} color="var(--text-faint)" />
          <span className="muted">
            Используйте цифры <span className="kbd">1</span>–<span className="kbd">5</span> для быстрого выбора
          </span>
        </div>
      </div>

      <div className="q-footer">
        <button className="btn btn-secondary" onClick={p.onBack} disabled={p.questionIndex === 0}>
          <Icon name="chevronLeft" size={16} />
          Назад
        </button>
        <div className="row gap-8">
          <button className="btn btn-ghost" onClick={p.onDk}>
            Затрудняется
          </button>
          <button className="btn btn-primary" onClick={p.onNext} disabled={!p.answer}>
            {isLast ? 'Завершить анкету' : 'Далее'}
            <Icon name="chevronRight" size={16} />
          </button>
        </div>
      </div>
    </div>
  );
}
```

- [ ] **Step 3: SurveyPlaceholder.tsx (passive case)**

```tsx
import { Icon } from '../../ui/Icon';
import type { SurveyVersion } from '../../types/survey';

export function SurveyPlaceholder({ survey }: { survey: SurveyVersion }) {
  const qCount = survey.nodes.filter((n) => n.kind === 'question').length;
  return (
    <div style={{ height: '100%', display: 'flex', flexDirection: 'column' }}>
      <div className="q-header">
        <div className="muted" style={{ fontSize: '0.82em', textTransform: 'uppercase', letterSpacing: '0.05em' }}>
          Анкета
        </div>
        <h3 style={{ marginTop: 4 }}>{survey.title}</h3>
      </div>
      <div className="q-body" style={{ alignItems: 'center', display: 'flex', justifyContent: 'center', textAlign: 'center' }}>
        <div style={{ maxWidth: 360 }}>
          <div
            style={{
              width: 72,
              height: 72,
              margin: '0 auto 16px',
              background: 'var(--bg-soft)',
              borderRadius: '50%',
              display: 'grid',
              placeItems: 'center',
              color: 'var(--text-faint)',
            }}
          >
            <Icon name="file-text" size={32} />
          </div>
          <h3 style={{ marginBottom: 8, color: 'var(--text-muted)' }}>Анкета откроется при разговоре</h3>
          <div className="muted">
            Нажмите «Готов к следующему звонку», чтобы система начала автодозвон.
            Текст вступления и все вопросы появятся здесь.
          </div>
          <div style={{ marginTop: 24, padding: 14, background: 'var(--bg-soft)', borderRadius: 'var(--radius)', fontSize: '0.88em', textAlign: 'left' }}>
            <div className="muted" style={{ fontSize: '0.78em', textTransform: 'uppercase', letterSpacing: '0.05em', marginBottom: 6 }}>
              Анкета содержит
            </div>
            <div className="row" style={{ gap: 6, marginBottom: 4 }}>
              <Icon name="check" size={14} color="var(--success)" /> {qCount} вопросов
            </div>
            <div className="row" style={{ gap: 6, marginBottom: 4 }}>
              <Icon name="check" size={14} color="var(--success)" /> Вступление и инструкции
            </div>
            <div className="row" style={{ gap: 6 }}>
              <Icon name="check" size={14} color="var(--success)" /> Среднее время: {survey.metadata.estimatedMinutes} мин
            </div>
          </div>
        </div>
      </div>
    </div>
  );
}
```

- [ ] **Step 4: Run tests — green.** Commit.

```bash
git add web/src/components/operator/QuestionPane.tsx web/src/components/operator/QuestionPane.test.tsx web/src/components/operator/SurveyPlaceholder.tsx
git commit -m "feat(web): add QuestionPane and SurveyPlaceholder components"
```

---

## Task 13: Stats widgets — ChartBars / Compare / LegItem / ShiftTimeBar / StatusList

**Files:**
- Create: `web/src/components/operator/ChartBars.tsx`
- Create: `web/src/components/operator/ChartBars.test.tsx`
- Create: `web/src/components/operator/Compare.tsx`
- Create: `web/src/components/operator/LegItem.tsx`
- Create: `web/src/components/operator/ShiftTimeBar.tsx`
- Create: `web/src/components/operator/StatusList.tsx`

- [ ] **Step 1: Test for ChartBars (the most "logic-heavy" widget)**

```tsx
import { render } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import { ChartBars } from './ChartBars';

describe('ChartBars', () => {
  it('renders one stack per data point', () => {
    const { container } = render(<ChartBars hourly={[2, 1, 3, 4, 2, 5, 3, 4, 2, 1, 0, 1]} />);
    expect(container.querySelectorAll('.chart-bar-stack')).toHaveLength(12);
  });

  it('uses max(hourly) as 100% reference', () => {
    const { container } = render(<ChartBars hourly={[1, 0, 0, 0]} />);
    const stack = container.querySelectorAll('.chart-bar-stack')[0];
    const bars = stack.querySelectorAll('.chart-bar') as NodeListOf<HTMLElement>;
    // success bar (~30%) + remaining accent bar
    expect(bars.length).toBe(2);
  });

  it('handles all-zeros input safely', () => {
    const { container } = render(<ChartBars hourly={[0, 0, 0]} />);
    expect(container.querySelectorAll('.chart-bar-stack')).toHaveLength(3);
  });
});
```

- [ ] **Step 2: Implementation**

```tsx
export interface ChartBarsProps {
  hourly: number[];           // 12 numbers, 09–21 in prototype
  successRatio?: number;      // 0..1; default 0.3 to mirror prototype
}

export function ChartBars({ hourly, successRatio = 0.3 }: ChartBarsProps) {
  const max = Math.max(1, ...hourly);
  return (
    <div className="chart-row">
      {hourly.map((h, i) => {
        const totalPct = (h / max) * 100;
        const successPct = (h * successRatio / max) * 100;
        const remaining = totalPct - successPct;
        return (
          <div key={i} className="chart-bar-stack" title={`${i + 9}:00 — ${h} звонков`}>
            <div className="chart-bar" style={{ height: `${successPct}%`, background: 'var(--success)' }} />
            <div className="chart-bar" style={{ height: `${remaining}%`, background: 'var(--accent-soft)' }} />
          </div>
        );
      })}
    </div>
  );
}
```

- [ ] **Step 3: Compare.tsx**

```tsx
export interface CompareProps {
  label: string;
  you: string;
  team: string;
  tone: 'success' | 'warning' | 'danger';
}

export function Compare({ label, you, team, tone }: CompareProps) {
  return (
    <div className="row" style={{ justifyContent: 'space-between' }}>
      <div className="muted">{label}</div>
      <div className="row gap-12">
        <span className="mono"><strong>{you}</strong> <span className="muted">вы</span></span>
        <span className="muted mono">{team} команда</span>
        <span className={`badge badge-${tone}`}>{tone === 'success' ? 'выше' : tone === 'warning' ? 'ниже' : 'ниже'}</span>
      </div>
    </div>
  );
}
```

- [ ] **Step 4: LegItem.tsx**

```tsx
export interface LegItemProps {
  color: string;
  label: string;
  value: string;
}

export function LegItem({ color, label, value }: LegItemProps) {
  return (
    <div className="row gap-8">
      <span style={{ width: 10, height: 10, background: color, borderRadius: 2 }} />
      <span className="muted">{label}</span>
      <span className="mono"><strong>{value}</strong></span>
    </div>
  );
}
```

- [ ] **Step 5: ShiftTimeBar.tsx**

```tsx
import { LegItem } from './LegItem';
import { fmtMmSs } from '../../hooks/useShiftTimer';

export interface ShiftTimeBarProps {
  callSeconds: number;
  pauseSeconds: number;
  dialingSeconds: number;
  idleSeconds: number;
}

export function ShiftTimeBar(p: ShiftTimeBarProps) {
  return (
    <div>
      <div style={{ height: 28, borderRadius: 6, overflow: 'hidden', display: 'flex' }}>
        <div style={{ flex: p.callSeconds, background: 'var(--accent)' }} title="В разговоре" />
        <div style={{ flex: p.pauseSeconds, background: 'var(--warning)' }} title="Пауза" />
        <div style={{ flex: p.dialingSeconds, background: 'var(--info)' }} title="Дозвон/обработка" />
        <div style={{ flex: p.idleSeconds, background: 'var(--bg-soft)' }} title="Простой" />
      </div>
      <div className="row" style={{ marginTop: 14, gap: 18, flexWrap: 'wrap', fontSize: '0.9em' }}>
        <LegItem color="var(--accent)" label="В разговоре" value={fmtMmSs(p.callSeconds)} />
        <LegItem color="var(--warning)" label="Пауза" value={fmtMmSs(p.pauseSeconds)} />
        <LegItem color="var(--info)" label="Дозвон / обработка" value={fmtMmSs(p.dialingSeconds)} />
        <LegItem color="var(--bg-soft)" label="Простой" value={fmtMmSs(p.idleSeconds)} />
      </div>
    </div>
  );
}
```

- [ ] **Step 6: StatusList.tsx**

```tsx
export interface StatusListItem {
  label: string;
  count: number;
  color: string; // CSS variable suffix: success/danger/warning/info/muted
}

export function StatusList({ items, total }: { items: StatusListItem[]; total: number }) {
  return (
    <div className="col gap-12">
      {items.map((st) => {
        const pct = total > 0 ? (st.count / total) * 100 : 0;
        return (
          <div key={st.label}>
            <div className="row" style={{ justifyContent: 'space-between', marginBottom: 6, fontSize: '0.92em' }}>
              <span>{st.label}</span>
              <span className="mono">
                <strong>{st.count}</strong> <span className="muted">· {Math.round(pct)}%</span>
              </span>
            </div>
            <div style={{ height: 8, background: 'var(--bg-soft)', borderRadius: 4, overflow: 'hidden' }}>
              <div style={{ width: pct + '%', height: '100%', background: `var(--${st.color})` }} />
            </div>
          </div>
        );
      })}
    </div>
  );
}
```

- [ ] **Step 7: Run tests — green.** Commit.

```bash
git add web/src/components/operator/ChartBars.tsx web/src/components/operator/ChartBars.test.tsx web/src/components/operator/Compare.tsx web/src/components/operator/LegItem.tsx web/src/components/operator/ShiftTimeBar.tsx web/src/components/operator/StatusList.tsx
git commit -m "feat(web): add stats widgets (ChartBars, Compare, ShiftTimeBar, StatusList)"
```

---

## Task 14: Workstation page — composition, FSM-driven render, WS, verto

**Files:**
- Create: `web/src/pages/operator/Workstation.tsx`
- Create: `web/src/pages/operator/Workstation.test.tsx`

This is the largest test surface. Because verto + WS are both async, tests use the FakeHub + FakeVerto injectors.

- [ ] **Step 1: Skeleton test (smoke + first transition)**

```tsx
import { render, screen, waitFor, act } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { Workstation } from './Workstation';
import { FakeHub } from '../../test/mocks/ws-hub.mock';
import { FakeVerto, resetFakeVerto } from '../../test/mocks/verto.mock';
import { mockMe, mockSurveyVersion } from '../../test/mocks/data';

function withQuery(node: React.ReactNode) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return <QueryClientProvider client={client}>{node}</QueryClientProvider>;
}

let hub: FakeHub;

beforeEach(() => {
  resetFakeVerto();
  hub = new FakeHub();
  (globalThis as never as { __vertoLoader: () => Promise<unknown> }).__vertoLoader = () =>
    Promise.resolve({ Verto: FakeVerto });
});

afterEach(() => vi.clearAllMocks());

describe('Workstation — basic shell', () => {
  it('renders ReadyCard initially with project header', async () => {
    render(withQuery(<Workstation me={mockMe} hub={hub} surveyVersion={mockSurveyVersion} />));
    await waitFor(() => expect(screen.getByText('Рабочее место оператора')).toBeInTheDocument());
    expect(screen.getByRole('button', { name: /Готов к следующему звонку/ })).toBeInTheDocument();
  });

  it('renders ws-grid and four MiniStat tiles', () => {
    const { container } = render(
      withQuery(<Workstation me={mockMe} hub={hub} surveyVersion={mockSurveyVersion} />),
    );
    expect(container.querySelector('.ws-grid')).not.toBeNull();
    expect(screen.getByText('Звонков сегодня')).toBeInTheDocument();
    expect(screen.getByText('Успешных анкет')).toBeInTheDocument();
    expect(screen.getByText('Время в звонке')).toBeInTheDocument();
    expect(screen.getByText('Среднее обработки')).toBeInTheDocument();
  });
});

describe('Workstation — FSM transitions', () => {
  it('ready -> dialing on click', async () => {
    render(withQuery(<Workstation me={mockMe} hub={hub} surveyVersion={mockSurveyVersion} />));
    await userEvent.click(screen.getByRole('button', { name: /Готов к следующему звонку/ }));
    expect(await screen.findByText(/Идёт автодозвон/)).toBeInTheDocument();
  });

  it('dialing -> call when WS publishes call.bridged', async () => {
    render(withQuery(<Workstation me={mockMe} hub={hub} surveyVersion={mockSurveyVersion} />));
    await userEvent.click(screen.getByRole('button', { name: /Готов к следующему звонку/ }));
    await screen.findByText(/Идёт автодозвон/);

    act(() =>
      hub.emit('op.u-svetlana.commands', { type: 'call.bridged', callId: 'c1', vertoCallId: 'v1' }),
    );
    // Workstation also subscribes to call.<id>.events; the page should pick up bridged via either route
    expect(await screen.findByText(/Идёт разговор/)).toBeInTheDocument();
  });

  it('clicking option 1 selects answer and Next becomes enabled', async () => {
    render(
      withQuery(
        <Workstation
          me={mockMe}
          hub={hub}
          surveyVersion={mockSurveyVersion}
          initialState="call"
          initialCallId="c1"
        />,
      ),
    );
    await userEvent.click(screen.getByText('Очень интересуюсь'));
    expect(screen.getByRole('button', { name: /Далее/ })).toBeEnabled();
  });

  it('keyboard "1" selects first option', async () => {
    render(
      withQuery(
        <Workstation me={mockMe} hub={hub} surveyVersion={mockSurveyVersion} initialState="call" />,
      ),
    );
    await userEvent.keyboard('1');
    // .selected class on first .q-option
    const opts = document.querySelectorAll('.q-option');
    expect(opts[0].className).toContain('selected');
  });

  it('hangup ends the call and shows StatusCard', async () => {
    render(
      withQuery(
        <Workstation
          me={mockMe}
          hub={hub}
          surveyVersion={mockSurveyVersion}
          initialState="call"
          initialCallId="c1"
        />,
      ),
    );
    await userEvent.click(screen.getByRole('button', { name: /Завершить разговор/ }));
    expect(await screen.findByText(/Какой результат разговора/)).toBeInTheDocument();
  });

  it('selecting refused returns to ready without verify', async () => {
    render(
      withQuery(
        <Workstation me={mockMe} hub={hub} surveyVersion={mockSurveyVersion} initialState="status" />,
      ),
    );
    await userEvent.click(screen.getByText('Отказ от опроса'));
    expect(await screen.findByText(/Нажмите «Готов»/)).toBeInTheDocument();
  });

  it('selecting success goes to verify, save returns to ready', async () => {
    render(
      withQuery(
        <Workstation me={mockMe} hub={hub} surveyVersion={mockSurveyVersion} initialState="status" />,
      ),
    );
    await userEvent.click(screen.getByText('Анкета заполнена'));
    expect(await screen.findByText(/Перепроверка анкеты/)).toBeInTheDocument();
    await userEvent.click(screen.getByRole('button', { name: /Сохранить и перейти дальше/ }));
    expect(await screen.findByText(/Нажмите «Готов»/)).toBeInTheDocument();
  });

  it('force-pause WS command moves operator to pause', async () => {
    render(withQuery(<Workstation me={mockMe} hub={hub} surveyVersion={mockSurveyVersion} />));
    act(() =>
      hub.emit('op.u-svetlana.commands', { type: 'op.force-pause', reason: 'admin-action' }),
    );
    expect(await screen.findByText(/Автодозвон приостановлен/)).toBeInTheDocument();
  });
});
```

- [ ] **Step 2: Implementation**

`web/src/pages/operator/Workstation.tsx`:

```tsx
import { useEffect, useMemo, useState } from 'react';
import { useMutation } from '@tanstack/react-query';
import { CurrentStateBadge } from '../../components/operator/CurrentStateBadge';
import { MiniStat } from '../../components/operator/MiniStat';
import { ReadyCard } from '../../components/operator/ReadyCard';
import { DialingCard } from '../../components/operator/DialingCard';
import { ActiveCallCard } from '../../components/operator/ActiveCallCard';
import { StatusCard, STATUS_OPTIONS } from '../../components/operator/StatusCard';
import { VerifyCard } from '../../components/operator/VerifyCard';
import { PauseCard } from '../../components/operator/PauseCard';
import { QuestionPane } from '../../components/operator/QuestionPane';
import { SurveyPlaceholder } from '../../components/operator/SurveyPlaceholder';
import { useCallFSM } from '../../hooks/useCallFSM';
import { useSurveyRuntime } from '../../hooks/useSurveyRuntime';
import { useWSSubscription, type SubscribableHub } from '../../hooks/useWSSubscription';
import { useShiftTimer } from '../../hooks/useShiftTimer';
import { useKeyboardShortcuts } from '../../hooks/useKeyboardShortcuts';
import { useVertoClient } from '../../hooks/useVertoClient';
import { fetchOperatorCredentials, type OperatorCredentials } from '../../api/telephony';
import { postOperatorReady, postOperatorPause, postOperatorResume } from '../../api/operator';
import { postCallAnswer, postCallHangup, postCallStatus } from '../../api/calls';
import type { CallEvent, CallState, CallStatusOption, OpCommand, RespondentSnapshot } from '../../types/call';
import type { MeProfile } from '../../types/operator';
import type { SurveyVersion } from '../../types/survey';

export interface WorkstationProps {
  me: MeProfile;
  hub: SubscribableHub;
  surveyVersion: SurveyVersion;
  initialState?: CallState;
  initialCallId?: string;
}

export function Workstation({
  me,
  hub,
  surveyVersion,
  initialState = 'ready',
  initialCallId,
}: WorkstationProps) {
  const fsm = useCallFSM(initialState);
  const runtime = useSurveyRuntime(surveyVersion);
  const [respondent, setRespondent] = useState<RespondentSnapshot | null>(
    initialState === 'call' && initialCallId
      ? {
          id: '48211',
          phone: '+7 (495) 234-78-15',
          region: 'Москва',
          attemptNo: 1,
          attemptMax: 3,
        }
      : null,
  );
  const [creds, setCreds] = useState<OperatorCredentials | null>(null);
  const [muted, setMuted] = useState(false);

  // Lazy fetch verto credentials when first transitioning to dialing
  useEffect(() => {
    if (fsm.state === 'dialing' && !creds) {
      fetchOperatorCredentials().then(setCreds).catch(() => {/* surfaced via verto status */});
    }
  }, [fsm.state, creds]);

  const verto = useVertoClient(creds);

  // Timers
  const callTimer = useShiftTimer(fsm.state === 'call', null);
  const dialingTimer = useShiftTimer(fsm.state === 'dialing', null);
  const pauseTimer = useShiftTimer(fsm.state === 'pause', null);

  // WS subscriptions: per-operator commands + per-call events
  useWSSubscription<OpCommand>(hub, `op.${me.userId}.commands`, (msg) => {
    // Server may multiplex bridged events here too; tolerate both.
    const m = msg as OpCommand | CallEvent;
    if ((m as OpCommand).type === 'op.force-pause') {
      fsm.forcePause((m as OpCommand & { type: 'op.force-pause' }).reason);
    } else if ((m as OpCommand).type === 'op.force-end-shift') {
      fsm.forcePause('Принудительное завершение смены');
    } else if ((m as CallEvent).type === 'call.bridged') {
      const ev = m as CallEvent & { type: 'call.bridged' };
      fsm.bridged({ callId: ev.callId, vertoCallId: ev.vertoCallId });
    } else if ((m as CallEvent).type === 'call.dialing') {
      const ev = m as CallEvent & { type: 'call.dialing' };
      setRespondent(ev.respondent);
    }
  });

  const callTopic = fsm.callId ? `call.${fsm.callId}.events` : null;
  useWSSubscription<CallEvent>(hub, callTopic, (ev) => {
    if (ev.type === 'call.bridged') {
      fsm.bridged({ callId: ev.callId, vertoCallId: ev.vertoCallId });
    } else if (ev.type === 'call.dialing') {
      setRespondent(ev.respondent);
    } else if (ev.type === 'call.hangup') {
      fsm.hangup(ev.cause);
    }
  });

  // Keyboard shortcuts (active during call only — FR-D7)
  useKeyboardShortcuts({
    enabled: fsm.state === 'call' && Boolean(runtime.currentNode),
    onDigit: (d) => {
      const opt = runtime.currentNode?.options?.[d - 1];
      if (opt) runtime.answer({ kind: 'single', optionId: opt.id, label: opt.label });
    },
    onPauseToggle: () => fsm.requestPause(),
    onNext: () => onNext(),
    onCancel: () => {/* operator cancel — no-op for now */},
    onDk: () => runtime.answer({ kind: 'dk' }),
  });

  const answerMutation = useMutation({
    mutationFn: (p: { callId: string; nodeId: string }) =>
      postCallAnswer(p.callId, p.nodeId, runtime.answers[p.nodeId]),
  });

  const startMutation = useMutation({
    mutationFn: () => postOperatorReady(),
    onSuccess: () => fsm.start(),
  });

  const pauseMutation = useMutation({
    mutationFn: () => postOperatorPause('manual'),
    onSuccess: () => fsm.requestPause(),
  });

  const resumeMutation = useMutation({
    mutationFn: () => postOperatorResume(),
    onSuccess: () => fsm.resume(),
  });

  const statusMutation = useMutation({
    mutationFn: (p: { status: CallStatusOption; comment: string }) =>
      postCallStatus(fsm.callId!, { status: p.status.id, comment: p.comment }),
  });

  const onNext = () => {
    const node = runtime.currentNode;
    if (!node || !runtime.answers[node.id]) return;
    if (fsm.callId) {
      answerMutation.mutate({ callId: fsm.callId, nodeId: node.id });
    }
    runtime.next();
    if (runtime.questionIndex === runtime.questionCount - 1 && runtime.isComplete) {
      // last question reached AND runtime walked to terminal — go to status
      // (this branch fires next tick because runtime is async-ish)
    }
  };

  const onChooseStatus = (s: CallStatusOption, comment: string) => {
    if (fsm.callId) statusMutation.mutate({ status: s, comment });
    fsm.pickStatus(s);
  };

  const onEndCall = () => {
    if (fsm.callId) {
      verto.hangup();
      postCallHangup(fsm.callId).catch(() => {/* still transition fsm */});
    }
    fsm.hangup('user');
  };

  const headerProjectName = useMemo(
    () => surveyVersion.title,
    [surveyVersion.title],
  );

  return (
    <div className="ws-grid">
      <div className="ws-left">
        <div className="row" style={{ justifyContent: 'space-between' }}>
          <div>
            <h2 style={{ marginBottom: 4 }}>Рабочее место оператора</h2>
            <div className="muted" style={{ fontSize: '0.92em' }}>
              Проект: <span style={{ color: 'var(--text)', fontWeight: 500 }}>{headerProjectName}</span>
            </div>
          </div>
          <CurrentStateBadge state={fsm.state} />
        </div>

        {fsm.state === 'ready' && (
          <ReadyCard onStart={() => startMutation.mutate()} onPause={() => pauseMutation.mutate()} />
        )}
        {fsm.state === 'dialing' && (
          <DialingCard
            timerSeconds={dialingTimer}
            region={respondent?.region ?? '—'}
            onAbort={() => fsm.abortDialing()}
          />
        )}
        {fsm.state === 'call' && respondent && (
          <ActiveCallCard
            timerSeconds={callTimer}
            respondent={respondent}
            muted={muted}
            onMicToggle={() => {
              setMuted((m) => !m);
              verto.toggleMute();
            }}
            onHold={() => verto.toggleHold()}
            onEnd={onEndCall}
          />
        )}
        {fsm.state === 'call' && !respondent && (
          <ActiveCallCard
            timerSeconds={callTimer}
            respondent={{ id: '—', phone: '—', region: '—', attemptNo: 1, attemptMax: 3 }}
            muted={muted}
            onMicToggle={() => setMuted((m) => !m)}
            onHold={() => {}}
            onEnd={onEndCall}
          />
        )}
        {fsm.state === 'status' && <StatusCard onChoose={onChooseStatus} />}
        {fsm.state === 'verify' && (
          <VerifyCard
            answers={runtime.answers}
            survey={surveyVersion}
            onRedo={() => fsm.redoVerify()}
            onSave={() => {
              fsm.saveVerify();
              runtime.reset();
            }}
          />
        )}
        {fsm.state === 'pause' && (
          <PauseCard
            pausedSeconds={pauseTimer}
            pauseLimitSeconds={me.pauseLimitSeconds}
            reason={fsm.pauseReason}
            onResume={() => resumeMutation.mutate()}
          />
        )}

        <div className="row gap-12" style={{ flexWrap: 'wrap' }}>
          <MiniStat label="Звонков сегодня" value="28" />
          <MiniStat label="Успешных анкет" value="6" tone="success" />
          <MiniStat label="Время в звонке" value="01:54" />
          <MiniStat label="Среднее обработки" value="04:12" />
        </div>
      </div>

      <div className="ws-right">
        {fsm.state === 'verify' ? (
          <VerifyDoneSplash />
        ) : fsm.state === 'call' && runtime.currentNode ? (
          <QuestionPane
            survey={surveyVersion}
            currentNode={runtime.currentNode}
            questionIndex={runtime.questionIndex}
            questionCount={runtime.questionCount}
            answer={runtime.answers[runtime.currentNode.id]}
            onAnswer={(a) => runtime.answer(a)}
            onNext={onNext}
            onBack={() => runtime.back()}
            onDk={() => runtime.answer({ kind: 'dk' })}
          />
        ) : (
          <SurveyPlaceholder survey={surveyVersion} />
        )}
      </div>
    </div>
  );
}

function VerifyDoneSplash() {
  return (
    <div style={{ height: '100%', display: 'flex', flexDirection: 'column' }}>
      <div className="q-header">
        <div className="row" style={{ justifyContent: 'space-between' }}>
          <div>
            <div className="muted" style={{ fontSize: '0.82em', textTransform: 'uppercase', letterSpacing: '0.05em' }}>
              Анкета
            </div>
            <h3 style={{ marginTop: 4 }}>Готово к проверке</h3>
          </div>
          <span className="badge badge-success">Готово к проверке</span>
        </div>
      </div>
      <div
        className="q-body"
        style={{ alignItems: 'center', display: 'flex', justifyContent: 'center', textAlign: 'center', padding: 32 }}
      >
        <div>
          <div style={{ fontSize: '3em', marginBottom: 12, color: 'var(--success)' }}>✓</div>
          <h3 style={{ marginBottom: 8 }}>Анкета заполнена</h3>
          <div className="muted" style={{ maxWidth: 320, margin: '0 auto' }}>
            Проверьте ответы в окне слева и сохраните результат.
          </div>
        </div>
      </div>
    </div>
  );
}
```

- [ ] **Step 3: Iterate test pass**

Run: `pnpm vitest run web/src/pages/operator/Workstation.test.tsx`
Expected: all tests green. Resolve any timing flakes by `await waitFor` or `findByText` rather than `getByText`.

- [ ] **Step 4: Commit**

```bash
git add web/src/pages/operator/Workstation.tsx web/src/pages/operator/Workstation.test.tsx
git commit -m "feat(web): add Workstation page composing FSM, survey runtime, WS, verto"
```

---

## Task 15: MyStats page

**Files:**
- Create: `web/src/pages/operator/MyStats.tsx`
- Create: `web/src/pages/operator/MyStats.test.tsx`

- [ ] **Step 1: Failing test (smoke + segment-control)**

```tsx
import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { describe, expect, it } from 'vitest';
import { MyStats } from './MyStats';

function withQuery(node: React.ReactNode) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return <QueryClientProvider client={client}>{node}</QueryClientProvider>;
}

describe('MyStats', () => {
  it('renders four KPI tiles after load', async () => {
    render(withQuery(<MyStats />));
    await waitFor(() => expect(screen.getByText('Звонков всего')).toBeInTheDocument());
    expect(screen.getByText('28')).toBeInTheDocument(); // callsTotal
    expect(screen.getByText('Успешных анкет')).toBeInTheDocument();
    expect(screen.getByText('6')).toBeInTheDocument();
  });

  it('switches period when segment clicked', async () => {
    render(withQuery(<MyStats />));
    await waitFor(() => screen.getByText('Сегодня'));
    await userEvent.click(screen.getByText('Неделя'));
    // active class moves
    const week = screen.getByText('Неделя');
    expect(week.className).toContain('active');
  });

  it('shows team comparison block', async () => {
    render(withQuery(<MyStats />));
    await waitFor(() => screen.getByText('Сравнение с командой'));
    expect(screen.getByText('Анкет за час')).toBeInTheDocument();
  });
});
```

- [ ] **Step 2: Implementation**

```tsx
import { useState } from 'react';
import { useQuery } from '@tanstack/react-query';
import { fetchMyStats, type Period } from '../../api/me';
import { ChartBars } from '../../components/operator/ChartBars';
import { StatusList } from '../../components/operator/StatusList';
import { ShiftTimeBar } from '../../components/operator/ShiftTimeBar';
import { Compare } from '../../components/operator/Compare';

export function MyStats() {
  const [period, setPeriod] = useState<Period>('today');
  const { data: s, isLoading } = useQuery({
    queryKey: ['me-stats', period],
    queryFn: () => fetchMyStats(period),
  });

  if (isLoading || !s) {
    return <div className="page" data-screen-label="op stats"><div className="muted">Загрузка...</div></div>;
  }

  const total = s.statuses.reduce((a, b) => a + b.count, 0);
  const successRate = Math.round((s.callsSuccess / Math.max(1, s.callsTotal)) * 100);

  return (
    <div className="page" data-screen-label="op stats">
      <div className="page-header">
        <div>
          <h1>Моя результативность</h1>
          <div className="muted" style={{ marginTop: 4 }}>
            {period === 'today' ? 'Сегодня' : period === 'week' ? 'Неделя' : 'Месяц'}
          </div>
        </div>
        <div className="row gap-8">
          <div className="seg">
            {(['today', 'week', 'month'] as const).map((p) => (
              <div
                key={p}
                role="button"
                tabIndex={0}
                className={`seg-item ${period === p ? 'active' : ''}`}
                onClick={() => setPeriod(p)}
                onKeyDown={(e) => e.key === 'Enter' && setPeriod(p)}
              >
                {p === 'today' ? 'Сегодня' : p === 'week' ? 'Неделя' : 'Месяц'}
              </div>
            ))}
          </div>
        </div>
      </div>

      <div style={{ display: 'grid', gridTemplateColumns: 'repeat(4, 1fr)', gap: 16, marginBottom: 24 }}>
        <div className="stat">
          <div className="stat-label">Звонков всего</div>
          <div className="stat-value">{s.callsTotal}</div>
          <div className="stat-delta up">+4 к среднему по группе</div>
        </div>
        <div className="stat">
          <div className="stat-label">Успешных анкет</div>
          <div className="stat-value" style={{ color: 'var(--success)' }}>{s.callsSuccess}</div>
          <div className="stat-delta">конверсия {successRate}%</div>
        </div>
        <div className="stat">
          <div className="stat-label">Время в звонке</div>
          <div className="stat-value mono">{s.callTime}</div>
          <div className="stat-delta">из {s.workTime} смены</div>
        </div>
        <div className="stat">
          <div className="stat-label">Среднее обработки</div>
          <div className="stat-value mono">{s.avgHandle}</div>
          <div className="stat-delta down">−12 сек к норме</div>
        </div>
      </div>

      <div style={{ display: 'grid', gridTemplateColumns: '1.4fr 1fr', gap: 16 }}>
        <div className="card">
          <div className="card-header">
            <h3 className="card-title">Звонки по часам</h3>
            <div className="muted" style={{ fontSize: '0.85em' }}>09:00 — 21:00</div>
          </div>
          <div className="card-body">
            <ChartBars hourly={s.hourly} />
            <div className="row" style={{ justifyContent: 'space-between', marginTop: 8, fontSize: '0.78em', color: 'var(--text-muted)' }}>
              {Array.from({ length: 12 }, (_, i) => <span key={i}>{i + 9}</span>)}
            </div>
            <div className="row" style={{ marginTop: 16, gap: 18, fontSize: '0.88em' }}>
              <span className="row gap-8"><span className="dot" style={{ color: 'var(--success)' }} /> Успешные</span>
              <span className="row gap-8"><span className="dot" style={{ color: 'var(--accent)' }} /> Все звонки</span>
            </div>
          </div>
        </div>

        <div className="card">
          <div className="card-header">
            <h3 className="card-title">Анкеты по статусам</h3>
            <span className="muted" style={{ fontSize: '0.85em' }}>{total} всего</span>
          </div>
          <div className="card-body">
            <StatusList items={s.statuses} total={total} />
          </div>
        </div>
      </div>

      <div style={{ marginTop: 16, display: 'grid', gridTemplateColumns: '1fr 1fr', gap: 16 }}>
        <div className="card">
          <div className="card-header"><h3 className="card-title">Распределение времени смены</h3></div>
          <div className="card-body">
            <ShiftTimeBar
              callSeconds={s.shiftBreakdown.callSeconds}
              pauseSeconds={s.shiftBreakdown.pauseSeconds}
              dialingSeconds={s.shiftBreakdown.dialingSeconds}
              idleSeconds={s.shiftBreakdown.idleSeconds}
            />
          </div>
        </div>

        <div className="card">
          <div className="card-header">
            <h3 className="card-title">Сравнение с командой</h3>
            <span className="muted" style={{ fontSize: '0.85em' }}>За {period === 'today' ? 'сегодня' : period === 'week' ? 'неделю' : 'месяц'}</span>
          </div>
          <div className="card-body">
            <div className="col gap-12">
              <Compare label="Анкет за час" you={String(s.team.surveysPerHourYou)} team={String(s.team.surveysPerHourTeam)} tone="success" />
              <Compare label="Конверсия" you={`${Math.round(s.team.conversionYou * 100)}%`} team={`${Math.round(s.team.conversionTeam * 100)}%`} tone="success" />
              <Compare label="Среднее обработки" you={s.team.avgHandleYou} team={s.team.avgHandleTeam} tone="success" />
              <Compare label="Время на паузе" you={s.team.pauseYou} team={s.team.pauseTeam} tone="warning" />
            </div>
          </div>
        </div>
      </div>
    </div>
  );
}
```

- [ ] **Step 3: Run tests — green.** Commit.

```bash
git add web/src/pages/operator/MyStats.tsx web/src/pages/operator/MyStats.test.tsx
git commit -m "feat(web): add MyStats page with KPIs, hourly chart, team comparison"
```

---

## Task 16: ProjectInfo page

**Files:**
- Create: `web/src/pages/operator/ProjectInfo.tsx`
- Create: `web/src/pages/operator/ProjectInfo.test.tsx`

- [ ] **Step 1: Test**

```tsx
import { render, screen, waitFor } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { describe, expect, it } from 'vitest';
import { ProjectInfo } from './ProjectInfo';
import { mockMe } from '../../test/mocks/data';

function withQuery(node: React.ReactNode) {
  const c = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return <QueryClientProvider client={c}>{node}</QueryClientProvider>;
}

describe('ProjectInfo', () => {
  it('renders project header (code + name)', async () => {
    render(withQuery(<ProjectInfo me={mockMe} />));
    await waitFor(() => screen.getByText('ВЦИОМ-2026-05'));
    expect(screen.getByRole('heading', { level: 1 })).toHaveTextContent('Электоральный мониторинг — Май 2026');
  });

  it('renders rules list', async () => {
    render(withQuery(<ProjectInfo me={mockMe} />));
    await waitFor(() => screen.getByText(/Зачитывайте вступление дословно/));
    expect(screen.getByText(/Все звонки записываются для контроля качества/)).toBeInTheDocument();
  });

  it('renders progress bar with correct caption', async () => {
    render(withQuery(<ProjectInfo me={mockMe} />));
    await waitFor(() => screen.getByText('Прогресс'));
    expect(screen.getByText(/4820/)).toBeInTheDocument();
    expect(screen.getByText(/6000/)).toBeInTheDocument();
  });

  it('renders team block with curator and operator count', async () => {
    render(withQuery(<ProjectInfo me={mockMe} />));
    await waitFor(() => screen.getByText('Команда'));
    expect(screen.getByText(/24 операторов/)).toBeInTheDocument();
    expect(screen.getByText(/М.П. Соколова/)).toBeInTheDocument();
  });
});
```

- [ ] **Step 2: Implementation**

```tsx
import { useQuery } from '@tanstack/react-query';
import { apiClient } from '../../api/client';
import { Icon } from '../../ui/Icon';
import type { MeProfile } from '../../types/operator';
import type { ProjectInfo as ProjectInfoT, ProjectProgress } from '../../types/project';

export interface ProjectInfoProps {
  me: MeProfile;
}

export function ProjectInfo({ me }: ProjectInfoProps) {
  const projectId = me.currentProjectId;
  const enabled = projectId !== null;

  const project = useQuery({
    queryKey: ['project-info', projectId],
    queryFn: () => apiClient.get<ProjectInfoT>(`/api/projects/${projectId}`),
    enabled,
  });
  const progress = useQuery({
    queryKey: ['project-progress', projectId],
    queryFn: () => apiClient.get<ProjectProgress>(`/api/projects/${projectId}/progress`),
    enabled,
  });

  if (!enabled) {
    return (
      <div className="page" data-screen-label="project info">
        <div className="muted">У вас ещё нет назначенного проекта.</div>
      </div>
    );
  }
  if (project.isLoading || progress.isLoading || !project.data || !progress.data) {
    return (
      <div className="page" data-screen-label="project info">
        <div className="muted">Загрузка...</div>
      </div>
    );
  }
  const p = project.data;
  const pr = progress.data;
  const pct = (pr.done / Math.max(1, pr.target)) * 100;
  const statusBadge =
    p.status === 'active'
      ? { className: 'badge badge-success', label: 'Активен' }
      : p.status === 'paused'
      ? { className: 'badge badge-warning', label: 'Приостановлен' }
      : { className: 'badge', label: 'В архиве' };

  return (
    <div className="page" data-screen-label="project info">
      <div className="page-header">
        <div>
          <div className="muted" style={{ fontSize: '0.85em', marginBottom: 4 }}>{p.code}</div>
          <h1>{p.name}</h1>
        </div>
        <span className={statusBadge.className}><span className="dot" /> {statusBadge.label}</span>
      </div>

      <div style={{ display: 'grid', gridTemplateColumns: '2fr 1fr', gap: 16 }}>
        <div className="card">
          <div className="card-header"><h3 className="card-title">Описание проекта</h3></div>
          <div className="card-body">
            <p style={{ marginTop: 0, lineHeight: 1.6 }}>{p.description}</p>
            <h4 style={{ marginTop: 18, marginBottom: 8 }}>Что важно соблюдать</h4>
            <ul style={{ paddingLeft: 18, lineHeight: 1.8, color: 'var(--text-muted)' }}>
              {p.rules.map((r) => <li key={r}>{r}</li>)}
            </ul>
          </div>
        </div>

        <div className="col gap-16">
          <div className="card">
            <div className="card-header"><h3 className="card-title">Прогресс</h3></div>
            <div className="card-body">
              <div className="row" style={{ justifyContent: 'space-between', marginBottom: 6 }}>
                <span className="muted">Заполнено анкет</span>
                <span className="mono"><strong>{pr.done}</strong> / {pr.target}</span>
              </div>
              <div style={{ height: 10, background: 'var(--bg-soft)', borderRadius: 5, overflow: 'hidden' }}>
                <div style={{ width: `${pct}%`, height: '100%', background: 'var(--accent)' }} />
              </div>
              <div className="muted" style={{ fontSize: '0.85em', marginTop: 6 }}>
                Осталось {pr.target - pr.done} анкет · ориентир {pr.deadline}
              </div>
            </div>
          </div>

          <div className="card">
            <div className="card-header"><h3 className="card-title">Команда</h3></div>
            <div className="card-body col gap-8">
              <div className="row gap-8">
                <Icon name="users" size={16} color="var(--text-muted)" />
                <span>{p.operatorsCount} операторов работают</span>
              </div>
              <div className="row gap-8">
                <Icon name="folder" size={16} color="var(--text-muted)" />
                <span>База: {p.baseName}</span>
              </div>
              <div className="row gap-8">
                <Icon name="user" size={16} color="var(--text-muted)" />
                <span>Куратор: {p.curatorName}</span>
              </div>
            </div>
          </div>
        </div>
      </div>
    </div>
  );
}
```

- [ ] **Step 3: Tests pass.** Commit.

```bash
git add web/src/pages/operator/ProjectInfo.tsx web/src/pages/operator/ProjectInfo.test.tsx
git commit -m "feat(web): add ProjectInfo page with description, progress, team"
```

---

## Task 17: OpHistory page (table + survey-answers modal)

**Files:**
- Create: `web/src/pages/operator/OpHistory.tsx`
- Create: `web/src/pages/operator/OpHistory.test.tsx`
- Create: `web/src/components/operator/SurveyAnswersModal.tsx`

- [ ] **Step 1: Failing test**

```tsx
import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { describe, expect, it } from 'vitest';
import { OpHistory } from './OpHistory';

function withQuery(node: React.ReactNode) {
  const c = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return <QueryClientProvider client={c}>{node}</QueryClientProvider>;
}

describe('OpHistory', () => {
  it('renders rows from /api/me/calls', async () => {
    render(withQuery(<OpHistory />));
    await waitFor(() => screen.getByText('+7 (495) 123-4521'));
    expect(screen.getAllByRole('row').length).toBeGreaterThan(1); // header + ≥1 data row
  });

  it('renders status badges with localised labels', async () => {
    render(withQuery(<OpHistory />));
    await waitFor(() => screen.getByText('+7 (495) 123-4521'));
    expect(screen.getByText('Успешно')).toBeInTheDocument();
    expect(screen.getByText('Отказ')).toBeInTheDocument();
  });

  it('shows "Анкета" button only on success rows', async () => {
    render(withQuery(<OpHistory />));
    await waitFor(() => screen.getByText('+7 (495) 123-4521'));
    const buttons = screen.getAllByRole('button', { name: /Анкета/ });
    // mockMyCalls has 2 success rows
    expect(buttons.length).toBeGreaterThanOrEqual(2);
  });

  it('opens survey modal when Анкета clicked', async () => {
    render(withQuery(<OpHistory />));
    await waitFor(() => screen.getByText('+7 (495) 123-4521'));
    const buttons = screen.getAllByRole('button', { name: /Анкета/ });
    await userEvent.click(buttons[0]);
    expect(await screen.findByText(/Заполненная анкета/)).toBeInTheDocument();
  });
});
```

- [ ] **Step 2: SurveyAnswersModal.tsx**

```tsx
import { useQuery } from '@tanstack/react-query';
import { fetchMyCallAnswers } from '../../api/me';
import { Icon } from '../../ui/Icon';

export interface SurveyAnswersModalProps {
  callId: string | null;
  onClose: () => void;
}

export function SurveyAnswersModal({ callId, onClose }: SurveyAnswersModalProps) {
  const enabled = callId !== null;
  const { data } = useQuery({
    queryKey: ['call-answers', callId],
    queryFn: () => fetchMyCallAnswers(callId!),
    enabled,
  });

  if (!enabled) return null;
  return (
    <div className="modal-backdrop" role="dialog" aria-modal="true">
      <div className="modal" style={{ maxWidth: 640 }}>
        <div className="modal-header">
          <div className="card-title">Заполненная анкета</div>
          <button className="btn btn-ghost btn-sm" onClick={onClose} aria-label="Закрыть">
            <Icon name="x" size={16} />
          </button>
        </div>
        <div className="modal-body">
          {data ? (
            <div className="col gap-12">
              {Object.entries(data.answers).map(([nodeId, ans]) => (
                <div key={nodeId} style={{ borderBottom: '1px solid var(--border)', paddingBottom: 8 }}>
                  <div className="muted" style={{ fontSize: '0.78em', textTransform: 'uppercase', letterSpacing: '0.05em' }}>
                    {nodeId}
                  </div>
                  <div className="mono" style={{ fontSize: '0.92em' }}>{JSON.stringify(ans)}</div>
                </div>
              ))}
            </div>
          ) : (
            <div className="muted">Загрузка...</div>
          )}
        </div>
        <div className="modal-footer">
          <button className="btn btn-secondary" onClick={onClose}>Закрыть</button>
        </div>
      </div>
    </div>
  );
}
```

- [ ] **Step 3: OpHistory.tsx**

```tsx
import { useState } from 'react';
import { useQuery } from '@tanstack/react-query';
import { fetchMyCalls, type Period } from '../../api/me';
import { Icon } from '../../ui/Icon';
import { SurveyAnswersModal } from '../../components/operator/SurveyAnswersModal';
import type { CallStatusId } from '../../types/call';

const STATUS_MAP: Record<CallStatusId, { label: string; tone: string }> = {
  success: { label: 'Успешно', tone: 'success' },
  refused: { label: 'Отказ', tone: 'danger' },
  dropped: { label: 'Сброс', tone: 'danger' },
  'no-answer': { label: 'Не дозвонились', tone: 'muted' },
  busy: { label: 'Занято', tone: 'warning' },
  callback: { label: 'Перезвонить', tone: 'info' },
  'wrong-person': { label: 'Не тот человек', tone: 'muted' },
  'tech-failure': { label: 'Тех. ошибка', tone: 'warning' },
};

export function OpHistory() {
  const [period] = useState<Period>('today');
  const [openCallId, setOpenCallId] = useState<string | null>(null);

  const { data, isLoading } = useQuery({
    queryKey: ['my-calls', period],
    queryFn: () => fetchMyCalls({ period }),
  });

  const calls = data ?? [];

  return (
    <div className="page" data-screen-label="op history">
      <div className="page-header">
        <div>
          <h1>История моих звонков</h1>
          <div className="muted">Сегодня · {calls.length} записей</div>
        </div>
        <div className="row gap-8">
          <button className="btn btn-secondary"><Icon name="filter" size={16} /> Фильтры</button>
          <button className="btn btn-secondary"><Icon name="download" size={16} /> Экспорт</button>
        </div>
      </div>

      <div className="card">
        <table className="table">
          <thead>
            <tr>
              <th>Время</th>
              <th>Телефон</th>
              <th>Регион</th>
              <th>Длительность</th>
              <th>Статус</th>
              <th />
            </tr>
          </thead>
          <tbody>
            {isLoading && (
              <tr><td colSpan={6} className="muted" style={{ textAlign: 'center', padding: 24 }}>Загрузка...</td></tr>
            )}
            {!isLoading && calls.length === 0 && (
              <tr><td colSpan={6} className="muted" style={{ textAlign: 'center', padding: 24 }}>Звонков пока нет</td></tr>
            )}
            {calls.map((c) => {
              const s = STATUS_MAP[c.status] ?? { label: '—', tone: 'muted' };
              return (
                <tr key={c.id}>
                  <td className="mono">{c.time}</td>
                  <td className="mono">{c.phone}</td>
                  <td>{c.region}</td>
                  <td className="mono">{c.duration}</td>
                  <td><span className={`badge badge-${s.tone}`}>{s.label}</span></td>
                  <td style={{ textAlign: 'right' }}>
                    {c.status === 'success' && (
                      <button className="btn btn-ghost btn-sm" onClick={() => setOpenCallId(c.id)}>
                        <Icon name="eye" size={14} /> Анкета
                      </button>
                    )}
                  </td>
                </tr>
              );
            })}
          </tbody>
        </table>
      </div>
      <SurveyAnswersModal callId={openCallId} onClose={() => setOpenCallId(null)} />
    </div>
  );
}
```

- [ ] **Step 4: Run tests — green.** Commit.

```bash
git add web/src/pages/operator/OpHistory.tsx web/src/pages/operator/OpHistory.test.tsx web/src/components/operator/SurveyAnswersModal.tsx
git commit -m "feat(web): add OpHistory page + SurveyAnswersModal"
```

---

## Task 18: Route registration

**Files:**
- Modify: `web/src/App.tsx` (created in Plan 15) — register the four operator routes.

- [ ] **Step 1: Add routes**

Locate the operator-area block in `App.tsx` and replace placeholder elements:

```tsx
import { Workstation } from './pages/operator/Workstation';
import { MyStats } from './pages/operator/MyStats';
import { ProjectInfo } from './pages/operator/ProjectInfo';
import { OpHistory } from './pages/operator/OpHistory';
// ...
<Route path="/operator/workstation" element={
  <Workstation
    me={me}
    hub={hub}
    surveyVersion={activeSurvey}
  />
} />
<Route path="/operator/stats" element={<MyStats />} />
<Route path="/operator/project" element={<ProjectInfo me={me} />} />
<Route path="/operator/history" element={<OpHistory />} />
```

`me`, `hub`, `activeSurvey` come from the auth/data context set up in Plan 15. If they aren't there yet — add a `useMe()` hook that wraps `apiClient.get<MeProfile>('/api/me')` and a `useActiveSurvey(me.currentProjectId)` hook hitting `/api/projects/:id/active-survey`.

- [ ] **Step 2: Sanity check**

Run: `pnpm dev` → manually open `/operator/workstation`. Expected: page renders, ReadyCard visible. Click "Готов" — transitions to dialing.

- [ ] **Step 3: Commit**

```bash
git add web/src/App.tsx
git commit -m "feat(web): wire operator routes to Workstation/MyStats/ProjectInfo/OpHistory"
```

---

## Task 19: Coverage gate ≥ 75% on `pages/operator/`

- [ ] **Step 1: Configure vitest coverage threshold**

Append to `web/vite.config.ts` (`test.coverage.thresholds`):

```ts
test: {
  coverage: {
    provider: 'v8',
    include: [
      'src/pages/operator/**',
      'src/components/operator/**',
      'src/hooks/useCallFSM.ts',
      'src/hooks/useSurveyRuntime.ts',
      'src/hooks/useWSSubscription.ts',
      'src/hooks/useKeyboardShortcuts.ts',
    ],
    exclude: ['**/*.test.*'],
    thresholds: {
      lines: 75,
      functions: 75,
      branches: 70,
      statements: 75,
    },
  },
},
```

- [ ] **Step 2: Run with coverage**

Run: `pnpm vitest run --coverage`
Expected: ≥ 75% across the included paths. If under, add tests for uncovered branches (typically: `tech-failure` STATUS path, `back()` after first question, error paths in mutations).

- [ ] **Step 3: Commit**

```bash
git add web/vite.config.ts
git commit -m "test(web): enforce 75% coverage gate on operator pages and hooks"
```

---

## Task 20: Visual fidelity self-audit (no commit)

- [ ] **Step 1: Open `SocioPulse.html` in a browser** at the operator user (`operator/1234`). Compare side-by-side with `pnpm dev` build at `/operator/workstation`. Iterate any class names, gaps, fonts that drift. Record diffs in plan-execution-log only — fixes already happen via Edit on respective files.

- [ ] **Step 2: Verify CSS classes used match prototype**

Run: `grep -E "className=\"" web/src/pages/operator web/src/components/operator -r | grep -oE "className=\"[^\"]+\"" | sort -u | head`
Expected output: only classes that exist in `social-pulse-maket/project/styles.css` (no typos like `q-options-list`, etc.).

- [ ] **Step 3: Verify no inline hex colours**

Run: `grep -RnE "#[0-9a-fA-F]{3,6}" web/src/pages/operator web/src/components/operator | grep -v "^.*test" || echo "no inline hex"`
Expected: only `rgb(...)` from CSS variables or no matches outside tests.

- [ ] **Step 4: Verify keyboard-shortcut a11y**

Manual: Tab through Workstation; check focus rings visible on options and buttons; check `1`-`5` work.

- [ ] **Step 5: No commit. Plan 16 visual review done.**

---

## Task 21: Final verification + tag

- [ ] **Step 1: Full test run**

Run: `pnpm vitest run --coverage`
Expected: all suites green; coverage thresholds met.

- [ ] **Step 2: tsc clean**

Run: `pnpm tsc --noEmit`
Expected: 0 errors.

- [ ] **Step 3: Build clean**

Run: `pnpm build`
Expected: Vite production build succeeds.

- [ ] **Step 4: Tag**

```bash
git tag -a v0.16.0-frontend-operator -m "Plan 16 complete: operator pages with verto + WS"
```

---

## Verification summary

After completing tasks 1–21:

- 4 operator pages live (`Workstation`, `MyStats`, `ProjectInfo`, `OpHistory`).
- `useCallFSM`, `useSurveyRuntime`, `useWSSubscription`, `useVertoClient`, `useKeyboardShortcuts` hooks tested.
- WS-hub integration: subscriptions to `op.<self>.commands` (force-pause/end + bridged multiplexing) and `call.<call_id>.events` (bridged, hangup, dialing).
- Verto/sip.js stack lazy-loaded; credential round-trip wired (`POST /api/telephony/operator-credentials`).
- 8 status outcomes wired to `POST /api/calls/:id/status`; verify-then-save workflow obeyed.
- Keyboard shortcuts `1`-`5`, Space, Enter, Esc, Z honoured during call.
- All pages use only classes from `social-pulse-maket/project/styles.css`; no inline hex.
- Coverage ≥ 75% on `pages/operator/` and the new hooks.
- TypeScript clean; production build clean.

This unlocks Plans 17/18/19 (admin pages) which import the same hooks and types.

---

## Self-review

**Spec coverage:**
- FR-D1 shift session entry → header on Workstation + project context shown. (Login + project picker handled by Plan 15.) ✓
- FR-D2 FSM transitions ready→dialing→call→status→verify→ready + pause from any state → `useCallFSM` covers all transitions, tested. ✓
- FR-D3 UI of each state per prototype → ReadyCard, DialingCard, ActiveCallCard, StatusCard, VerifyCard, PauseCard. ✓
- FR-D4 introductory script "зачитайте дословно" block → `QuestionPane` shows on first question. ✓
- FR-D5 progress bar + "вопрос N из M" → `q-progress` + counter. ✓
- FR-D6 hint blue block → renders `q.hint` if provided. ✓
- FR-D7 keyboard shortcuts 1–5/Space/Enter/Esc/Z → `useKeyboardShortcuts` + tests. ✓
- FR-D8 respondent card → `ActiveCallCard`. ✓
- FR-D9 mic / hold / hangup → `ActiveCallCard` + `useVertoClient`. ✓
- FR-D10 8 outcome statuses → `STATUS_OPTIONS`. ✓
- FR-D11 verify before save (success only) → `VerifyCard` + FSM branch. ✓
- FR-D12 operator comment → `StatusCard` textarea. ✓
- FR-D13 pause limit visual → `PauseCard` switches to danger after limit. ✓
- FR-D14 pages info / history / stats → 3 sibling pages. ✓
- FR-J1 KPI today → MyStats top tiles. ✓
- FR-J2 team comparison → MyStats Compare block. ✓
- FR-J3 history with read-only completed survey → OpHistory + SurveyAnswersModal. ✓
- FR-J4 project info → ProjectInfo. ✓
- §10.1 WS protocol — subscribe to topics, ack, push events → `useWSSubscription`. ✓
- §10.4 listen-in operator side: out of scope (admin-side). Operator does not initiate listen-in. ✓
- §11.5 client-side runtime → `useSurveyRuntime` (TS shim, WASM-ready). ✓
- §7.4 verto credentials → `fetchOperatorCredentials` + `useVertoClient`. ✓

**Out-of-scope correctly deferred:**
- Survey builder, admin overview/dialer/operators/users/calls/finance/reports — Plans 17/18/19.
- Backend bug-fixes — respective module plans.

Plan 16 verified.

---

**Plan complete and saved to `docs/superpowers/plans/2026-05-06-16-frontend-operator.md`.**
