<!--
Copy to docs/postmortems/YYYY-MM-DD-<short-slug>.md (the date the incident or drill started, JST).
Blameless: describe what the system and the process allowed to happen, never who "should have" done better.
Write "the deploy", "the on-call engineer", "the runbook", not a person's name, in the analysis.
Every number comes from the drill log, Prometheus or the logs, with a link. Never estimate a number and present it as
measured; write "unknown" and say why.
Delete these comments when the postmortem is final.
-->

# Postmortem: <!-- one line: what users experienced, not the cause -->

| | |
|---|---|
| Status | <!-- Draft / In review / Final --> |
| Trigger | <!-- Game day GD-0X (planned fault injection) / Real incident --> |
| Date (JST) | <!-- start – end --> |
| Environment | <!-- tasuki-prod / tasuki-dev; cluster session (aws-up date) --> |
| Services | <!-- e.g. orders, orders-db --> |
| Severity | <!-- page / ticket, and why --> |
| Authors | <!-- names --> |
| Reviewers | <!-- names, date reviewed --> |
| Drill plan and log | <!-- links, for game days --> |

## Summary

<!-- Three to five sentences: what happened, what users saw, how long, how it ended, and the one most important
     follow-up. A reader who stops here should know enough. For a game day, say plainly that the fault was injected
     on purpose, and what the hypothesis was. -->

## Impact

<!-- Who was affected and how: merchants (quotes, orders), couriers (offers, GPS), customers (tracking).
     Tasuki's users are simulated: say so, and give the simulated numbers. -->

| Measure | Value | Source |
|---|---|---|
| Duration of user-visible impact | <!-- from the first bad event to recovery --> | <!-- query or log line --> |
| Requests or events affected | <!-- e.g. failed creates, late GPS points --> | <!-- --> |
| Orders lost or duplicated | <!-- 0, or the number and IDs --> | <!-- --> |
| Client-side view (simulator) | <!-- failure ratio and reasons --> | <!-- --> |

### SLOs and error budget

<!-- One row per SLO that moved. "Remaining" is slo:period_error_budget_remaining:ratio (1 = untouched, 0 = spent).
     The policy's postmortem threshold is 20 % of the budget, measured from the remaining ratio before and after.
     In an ephemeral cluster that ratio covers only the session, so also give the 28-day equivalent
     (docs/gameday/README.md, "The error budget during drills") and show the arithmetic. -->

| SLO | Remaining before | Remaining after | Burned (policy) | 28-day equivalent | Alerts |
|---|---|---|---|---|---|
| <!-- orders · availability --> | <!-- --> | <!-- --> | <!-- --> | <!-- --> | <!-- name, severity, fired at --> |

Policy consequences: <!-- stage reached (75 % / 100 %), freeze yes/no, overrides used and who decided -->

## Timeline

<!-- JST, from the drill log (scripts/gameday/record-timeline.sh) or the incident channel. Include the trigger, the
     first symptom, detection, each decision and action, mitigation, recovery and the all-clear. Note gaps honestly
     ("no record between 21:04 and 21:11"). -->

| Time (JST) | Event | Source |
|---|---|---|
| <!-- --> | <!-- --> | <!-- log line, alert, query --> |

Key intervals: time to detect <!-- -->, time to mitigate <!-- -->, time to recover <!-- -->.

## Detection

<!-- How did we find out: which alert (with its severity and the time after the first bad event), a dashboard, a
     person? Did the alert we expected fire? Did one fire that should not have? If nothing fired, say what would
     have caught it and how much later. -->

## Response

<!-- What was done, in order, and why. Which runbook was used, and was it right: wrong commands, missing steps,
     unclear decisions? Which tools helped, which got in the way? -->

## Recovery

<!-- What ended the impact (rollback, failover, fix forward, the fault being removed), and how we confirmed that
     steady state was back (which SLIs, for how long). -->

## Root cause and contributing factors

<!-- The conditions that let a trigger become user impact. Usually several: a design gap, a missing alert, a
     default nobody revisited, an untested assumption. For a game day, the "trigger" is the injected fault; the
     interesting part is everything that made its impact bigger or longer than the design says. -->

1. <!-- -->

## What went well

- <!-- -->

## What went badly

- <!-- -->

## Where we got lucky

<!-- Things that limited the impact by chance rather than by design. Each one is a candidate action item. -->

- <!-- -->

## Action items

<!-- Every item has an owner, a due date and an issue. Types: prevent (stop it happening), detect (find out sooner),
     mitigate (smaller or shorter impact), process (runbooks, policy, drills). Mark the ones the policy calls
     critical: they decide when a freeze can be lifted early. -->

| # | Action | Type | Critical? | Owner | Due | Issue | Status |
|---|---|---|---|---|---|---|---|
| 1 | <!-- --> | <!-- --> | <!-- --> | <!-- --> | <!-- YYYY-MM-DD --> | <!-- link --> | <!-- open --> |

## Lessons

<!-- What we learned about the system or how we operate it, beyond this one incident. -->

## Links

- Drill plan / incident issue: <!-- -->
- Drill log: <!-- -->
- Dashboards or screenshots: <!-- -->
- Pull requests (fixes, runbook changes, overrides): <!-- -->
- Runbooks used: <!-- -->
