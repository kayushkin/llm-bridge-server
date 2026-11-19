# Plans that cannot be scored until the suite can run a React effect

These two plans are complete and correct. They are kept **out of the plan directory
proper** because `sabotage-failure-value.py` globs `*.json` one level deep, and a plan
whose CONTROL-POSITIVE cannot be caught turns every future run red for a reason that has
nothing to do with the code being scored. A control that cries wolf is one the next reader
turns off, so this is a subdirectory rather than an exemption.

## What was measured

Both were run on 2026-08-21 against a green 523-test suite. In both, **CONTROL-POSITIVE
was NOT caught**:

    session-names   4 mutations, 4 UNNOTICED, CONTROL-POSITIVE not caught
    raise-ceiling   2 mutations, 2 UNNOTICED, CONTROL-POSITIVE not caught

Those eight UNNOTICED verdicts are **not** a claim that eight producers are unpinned. They
are the instrument reporting that it never ran: nothing in the suite executes
`useSessionNames` (`src/react/sessionNames.ts`) or `useBudgetHalt`
(`src/react/hooks.ts`). A probe that could not run is not a negative result.

⚠️ `test/sessionNames.test.ts` makes this hard to see. It shares a name with
`src/react/sessionNames.ts` and imports **`ApiClient` only** — it tests the shape of the
`GET /sessions/summary` request, never the hook. A matching filename is not coverage.

## Why it is blocked

The suite drives React through `renderToStaticMarkup` (`react-dom/server`), which is how
`sessionSignals.test.ts` and `drafts.test.ts` render components. Server rendering **never
fires `useEffect`**, and the effect body is where both producers live. There is no
`jsdom` / `happy-dom` in `devDependencies` and no `environment` set for vitest, so there is
no path today that runs a React effect.

Clearing this needs one of two answers, and both are choices about the package rather than
about these plans:

1. **Add a DOM environment** (`jsdom` or `happy-dom` plus `environment: 'jsdom'`) and drive
   the hooks with `react-dom/client` + `act`. A new devDependency and a slower suite.
2. **Extract the pure predicates** out of the effects and test those directly, which is
   what the rest of this package already looks like — `selectors.ts`, `otelDedup.ts` and
   `terminalState.ts` are all pure and all thoroughly pinned. ⚠️ Extraction alone would
   leave the call site unheld; the extracted predicate would need a mutation proving the
   effect still calls it.

Once either lands, move the plan back up one directory and re-run it. The mutations
themselves need no changes.
