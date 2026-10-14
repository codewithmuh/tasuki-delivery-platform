# Postmortems

Blameless write-ups of incidents and of game days that taught us something. Start each one from
[TEMPLATE.md](TEMPLATE.md) and name it `YYYY-MM-DD-<short-slug>.md` (the date it started, JST).

**There are no postmortems yet.** No incident has happened and no game day has been run. Postmortems are written
after the event, from the drill log or the incident record, never in advance.

## When one is required

| Situation | Required? | Source |
|---|---|---|
| One incident burned **more than 20 %** of an SLO's error budget (remaining ratio before minus after) | **Yes**, within 5 working days | [Error-budget policy](../slo/error-budget-policy.md) |
| A page fired (fast burn: 2 % of the budget in an hour) | Check the 20 % rule once it is over. A page is how such incidents usually announce themselves | Policy |
| A budget was exhausted, or a freeze was lifted early, or a break-glass override was used | Yes: the decision and its reason are recorded in the postmortem | Policy, "Who decides" |
| Any order lost or duplicated, or the ledger out of balance (P3) | Yes, whatever the budget says | Design G1, G4 |
| A game day whose hypothesis was wrong, whose rollback failed, or whose alert or runbook did not work | Yes: that is the finding | [docs/gameday](../gameday/README.md) |
| A game day that went exactly as predicted and burned less than 20 % | No. The results section of the plan is enough | |

Game days count against the budget like any other incident. In this project's ephemeral cluster the budget covers
only the current session, so most prod drills will cross 20 %. Write those postmortems: they are P1's deliverable
(design §10: "two of them become written postmortems"). Give the 28-day-equivalent figure next to the policy's figure,
so readers see both. A postmortem that comes from a game day says so in its first line and in the `Trigger` field.

## Process

1. **Draft** (the incident's on-call engineer or the game day's driver), from the drill log or the incident timeline,
   within 5 working days. Status `Draft`.
2. **Review**: at least one reviewer who was not involved checks the timeline against the evidence, challenges every
   number without a source, and looks for blame in the wording. For a solo project, review it yourself after a day's
   gap and say so in the Reviewers field. Status `In review`.
3. **Action items**: each gets an owner, a due date and a GitHub issue (label `postmortem`, plus `gameday` for drills).
   The policy's freeze can be lifted early only when the **critical** items have shipped and the service owner
   records that decision here.
4. **Final**: status `Final`, linked from the [runbooks](../runbooks/README.md) it changed and from the drill plan's
   Results section.
5. **Follow-up**: the weekly SLO review checks open action items. When the fixes ship, re-run the game day that found
   the problem. The postmortem links that run's results.

## Index

| Date | Postmortem | Trigger | Services | Budget burned |
|---|---|---|---|---|
| <!-- none yet --> | | | | |
