# M7 proof — agents file work

Run on 2026-11-21 against a real manager (`bin/nsm manager serve`) and real HTTP. The model is not
involved: this proves the ROUTE and the board's rules. The `file_item` TOOL has its own tests, since a
scripted provider would only re-run the same three lines through a fake model.

The script is `/tmp/m7proof/proof2.sh` and its output below is verbatim.

## The rules, one call each

```
== five filings: each becomes a real queued item ==
  filing 1 -> 201 3998f0478e1a
  filing 2 -> 201 7dcd57f2456e
  filing 3 -> 201 789d91829296
  filing 4 -> 201 65fd6201f753
  filing 5 -> 201 c321ab290019

== the retry: same client_ref, no second item ==
{"id":"3998f0478e1ac0b90d1d2c35578616552ca1bea81ea6ff842831a8db682ac21e","created":false}
  -> 201

== a capability this node does not have: refused, and named ==
{"error":{"code":"filing","message":"this node cannot provide \"sandbox:seatbelt\", so it cannot ask for it"}}
  -> 400

== the sixth: the run is PARKED, not refused ==
  sixth -> 202 {"created":false,"parked":true,"reason":"this item has already filed 5 children, which is the limit"}
```

The retry returns **the id of the first filing** and `created:false`: the second call spent nothing.
The capability refusal names the capability, because a model can act on "this node cannot provide
sandbox:seatbelt" and cannot act on "forbidden". The sixth filing is 202, not 403: the run is parked
and a human is being asked.

## The human's half

```
== the operator's view of the parked parent ==
  status: needs_approval
  why: item de51ea1f… is waiting on a human: the action "file_item" has to be approved or denied
  parked: de51ea1f4c79 action: file_item

== approve: one filing is allowed, and the grant survives the claim that follows ==
  -> 303
  run2=0373800eb80cfb3bf85820b99eaf102bb46f03279d5c3fe4ed85ca37a54fff1c
{"id":"2216ea3db7ce876cc55d9508a69aedc178f405272819388eded5c864fbee4bc0","created":true}
  -> 201

== and the next one parks again: the grant was for ONE ==
  beyond -> 202 {"created":false,"parked":true,"reason":"this item has already filed 6 children, which is the limit"}
```

`run2` is a **different run id** — the claim that resumed the item. The grant survived it and was
spent by the filing (which is the correction M7's build forced; M3's rule is that a claim eats its
grant, and for `file_item` that would have spent the answer on resuming). The next filing parks at
six.

## What the board holds afterwards

```
children: 6
created events for children: 6
escalated events: 2
granted events: 1
denied events: 0
runs: 2
distinct runs that filed: 2
```

Six children, six `created` events, across **two runs** — which is the fan-out rule working: the cap
counts the parent's children, not one run's filings, so the re-claim that resumed the item did not
reset it. That was the plan's first error, found while building.

```
    child 1 | depth 1 | agent:7a74f535… | run 84444b8c
    …
    the granted child | depth 1 | agent:7a74f535… | run 0373800e
  parent status: needs_approval
```

Every child carries `created_by = agent:<node>`, `depth = 1` and the run that filed it, so "who asked
for this and from where" is a row rather than an inference.

## Three findings, recorded rather than smoothed over

1. **The decision route takes a form, not JSON, unless the Content-Type says otherwise.** `curl -d`
   defaults to `application/x-www-form-urlencoded`, so the JSON body was read as a form and the
   decision arrived empty — `decision "" is neither approve nor deny`. This is the same caller-side
   trap M5's proof recorded; it is a property of the route (the approvals *page* posts a form), not a
   bug, but it costs every scripted operator ten minutes.
2. **A client_ref is scoped to a run.** Reusing `ref-1` from a later run answers `409 client_ref_taken`,
   by design: a key identifies one run's request, and returning another run's item would be a silent
   cross-run join. The first script reusing refs across runs looked like a product bug and was not.
3. **A node at concurrency 1 that holds a lease cannot claim.** The first script's second claim
   returned an empty list because the earlier partial run still held the item. The proof now registers
   its own node, which is also what makes it re-runnable.

## What this proof does not show

- No model called `file_item`. The tool is covered by `TestTheFilingToolCarriesTheRunsOwnIdentity`,
  `TestTheFilingToolSaysWhenTheRunParked`, `TestTheFilingToolSaysWhenItAlreadyExisted`,
  `TestTheFilingToolRefusesBadArgumentsBeforeTheBoard`, `TestTheFilingToolPassesTheBoardsRefusalThrough`
  and `TestTheFilingToolIsOfferedOnlyWhenItCanWork` — the last of which exists because a mutation
  showed the tool-set condition was previously untested.
- Nothing was placed. Children are `queued`; whether placement picks them up is M1–M2's feature and
  is covered there.
- The tree is not rendered on a page yet (M7 T4). The board's answer is `/admin/why` and the
  approvals list, as shown above.
