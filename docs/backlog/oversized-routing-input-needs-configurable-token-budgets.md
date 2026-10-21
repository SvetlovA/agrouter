---
worth: yes
where: pkg/prompt/budget.go:13
added: 2026-10-21
---
# Oversized routing input needs configurable token budgets

Add configuration-driven token management for Jev requests. The maintainer intends to design this feature later; retain the investigation below as its motivating failure and regression evidence. Configuration names, defaults, and the exact controls remain design decisions for that work.

Today, request budgets and the byte-to-token estimate are hardcoded. A growing Ralphex progress log produces chunks that agrouter considers within budget but Jev rejects as oversized. Agrouter then abandons model and effort selection because its size-recovery path recognizes HTTP 422, while the observed Jev response is HTTP 400 with `max_tokens_exceeded`.

This concerns the input sent to Jev for routing and project-complexity scoring, not the child coding model's output-token allowance. No production fix was made during this investigation.

## Future feature scope

- Decide which request budgets, estimation margins, and chunk/anchor sizes should be configurable. Validate their relationships and preserve enough room for questions, catalog descriptions, envelopes, and repeated context.
- Apply the resulting policy consistently to whole-state requests, routing chunks, and project-document requests. Preserve the original prompt and stdin passed to the child; routing-size controls must not silently truncate the child's task.
- Recognize the structured token-overflow error and use bounded size recovery. Do not classify every HTTP 400 as a size error or retry invalid requests indefinitely. Keep recovery within the routing deadline.
- Expose safe, actionable diagnostics for size rejection. Report the status/error type and relevant size estimates without logging prompts, file contents, raw argv, or credentials.
- Document config layering and defaults, and update the hardcoded-budget assumptions in tests and project guidance as needed.

## Attached investigation: Ralphex digest run, 2026-10-21

### Environment and observed behavior

- Caller: Ralphex `v1.7.0-windows.3`; installed agrouter: `v0.2.0`.
- Checkout: `/Users/svtlv/Projects/workspaces/svtlv/Digest-staged-clustering`.
- Plan: `docs/plans/20261020-digest-staged-clustering.md`.
- Progress log: `.ralphex/progress/progress-20261020-digest-staged-clustering.txt`.
- Ralphex uses `agrouter exec claude`, supplies `AGENTS.md`, `README.md`, and the architecture docs through `--doc`, and leaves model and effort unpinned.
- Routing succeeded at 03:54:11 on October 21. The first logged model-stage failure was at 08:33:59, on the retry of task 14, and failures continued through task iteration 19. These are the timestamps printed in the local progress log.
- Failed selections report `cli: "claude"`, `model: null`, `effort: null`, reason `jev_api_error`, stage `model`. The child runs with the verified defaults `claude-opus-5-5` and `high`; those defaults are not a successful Jev selection.
- Subsequent Claude subscription-quota exhaustion and Ralphex's 15-minute waits are separate from the Jev request-size failure.

### Root-cause chain

1. Ralphex's task template mentions the plan and progress-log paths. `pkg/prompt/mentions.go:14` reads referenced text files into the routing state, with no file-size cap. At investigation time, the plan was 78,072 bytes and the progress log was 101,709 bytes. The growing log therefore expands routing input on later calls.
2. `pkg/prompt/budget.go:13` estimates tokens as UTF-8 bytes divided by three, rounded up. Its fixed limits include 30,000 estimated tokens for state plus the longest question and 64,000 for state plus all questions. Splitting does run, but the resulting requests can still exceed the server's actual token limit.
3. A decision-only reproduction with `AGROUTER_DEBUG=1` exposed the underlying response:

   ```text
   model stage: chunk 3 of 4: chunk request: jev: HTTP 400: {"detail":{"error_type":"max_tokens_exceeded"}}
   ```

4. `pkg/jev/client.go:83` maps only HTTP 422 to `ErrUnprocessable`. Both the whole-request recovery in `pkg/router/stage.go:167` and chunk recovery in `pkg/router/fanout.go:82` depend on that sentinel. The HTTP 400 token overflow instead cancels the fan-out and fails the model stage.
5. `pkg/router/stage.go:148` exits on the model-stage error before reaching effort selection. Since the CLI is fixed, `pkg/router/router.go:130` falls back to that CLI with only caller-fixed values. `cmd/agrouter/reason.go:87` normalizes the status error to `jev_api_error`, hiding the specific size diagnosis in ordinary output.

The measurements establish that the estimator/margin is insufficient for this input. They do not establish Jev's exact tokenizer, actual token count, or whether its server-side limit changed between historical attempts.

### Controlled reproductions

All ordinary probes used decision mode, so they did not start Claude or alter the running Ralphex session.

| Routing input | Result |
| --- | --- |
| Short C# review task | Model and effort selected successfully |
| Short task with project docs | Complexity scoring, model selection, and effort selection succeeded |
| Plan reference only | Model and effort selected successfully |
| Progress-log reference only | Model stage failed with HTTP 400 `max_tokens_exceeded` |
| Reconstructed Ralphex task prompt, with the configured doc set | Complexity scoring succeeded; model stage failed on chunk 3 of 4 with the same HTTP 400 |

Minimal reproduction, from the original checkout while that progress log exists, with the configured Jev credentials:

```sh
AGROUTER_DEBUG=1 agrouter --cli claude \
  -p 'Continue implementation. Read .ralphex/progress/progress-20261020-digest-staged-clustering.txt for previous progress.' \
  </dev/null
```

The live log is local and mutable, so the command is historical reproduction context, not a portable test fixture.

### Recovery experiment

A temporary Go harness used the real config, prompt capture, router, and Jev client. It mapped only HTTP 400 responses whose body contained `max_tokens_exceeded` to a synthetic HTTP 422 error, allowing the existing halving path to run. This was a diagnostic adapter, not a production error-classification design or a repository change.

For the progress-log-only input:

| Request | State bytes | Question bytes | Estimated total tokens | Response |
| --- | ---: | ---: | ---: | --- |
| Model chunk 1/2 | 84,390 | 4,344 | 29,578 | HTTP 400 token overflow |
| Model chunk 2/2 | 43,882 | 4,344 | 16,076 | Success |
| Smaller model piece | 48,155 | 4,344 | 17,500 | Success |
| Smaller model piece | 48,209 | 4,344 | 17,518 | Success |
| Effort chunk 1/2 | 84,390 | 3,003 | 29,131 | HTTP 400 token overflow |
| Smaller effort piece | 48,155 | 3,003 | 17,053 | Success |
| Smaller effort piece | 48,209 | 3,003 | 17,071 | Success |

The run completed with model `claude-opus-5-5`, effort `high`, and no undecided error. This confirms size recovery can restore selection. It also shows that effort requests can independently overflow once model selection succeeds; the original Ralphex failures stop before reaching that stage. These are observed outcomes, not model choices to pin in regression tests.

### Regression coverage to add with the feature

- Replay the exact HTTP 400 error shape through an `httptest` server, using synthetic input and catalog entries; verify bounded reduction and successful routing without real API calls.
- Cover whole-state and chunked routing, independent effort-stage overflow, and the shared project-document recovery path.
- Verify unrelated HTTP 400 responses remain errors, and repeated overflow terminates under the chosen retry/minimum-size policy and deadline.
- Test configurable-budget validation and precedence, repeated anchor/question overhead, unchanged child prompt/stdin, and diagnostic redaction.
- Existing recovery fixtures in `pkg/router/pool_test.go`, `pkg/router/stage_test.go`, and `pkg/router/complexity_test.go` exercise HTTP 422. Extend that coverage rather than relying on the incorrect status assumption.

Related: [Routing outcome is not visible in every mode](exec-routing-outcome-not-always-visible.md) covers delivery of routing outcomes to callers. This item covers request sizing and recovery; coordinate any diagnostic changes without duplicating that work.
