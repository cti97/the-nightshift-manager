# M6 proof — the steward

Run on 2026-11-21 against the real binaries (`bin/nsm`, `go1.27.1 darwin/arm64`), not against tests.
What tests cover is named as such; what was run by hand is shown with its output.

## The reflex executor

Covered by `internal/agent/reflex_test.go` (`TestReflexIsOneCallWithNoTools`,
`TestReflexRefusesAnEmptyAnswer`, `TestReflexFailsOverBudget`) rather than by hand: the path is one
provider request, and a manual run would need a fake provider anyway — which is what the test's
scripted provider is. The property worth stating is the negative one: the request carries **no tool
definitions**, because a model that believes it has tools spends its only turn asking for them.

## `nsm agent install`

```
$ nsm agent install --node proof --provides reflex --reach host --sandbox-root /tmp/runs --print
=== /Users/ixici/.config/nsm/agent.env (mode 0600)
NSM_NODE_TOKEN=[REDACTED]
=== /Users/ixici/.config/nsm/agent-run.sh (mode 0700)
#!/bin/sh
. "/Users/ixici/.config/nsm/agent.env"
export NSM_NODE_TOKEN
exec /Users/ixici/deepseek/the-nightshift-manager/bin/nsm agent run --board ...
=== ~/Library/LaunchAgents/com.nightshift.agent.plist (mode 0644)
```

The token appears in exactly one generated file, and that file is 0600. `--print` wrote nothing.

## Dynamic `lan:*`

`TestReachabilityIsADial` covers the dial against a real listener and a real closed port;
`TestTheHeartbeatDeclaresOnlyReachableServices` covers the second half — the service stops being
declared when its listener closes, without a restart.

## The endpoint canary

`TestTheCanaryWaitsForTheThreshold` (three failures, then a report), `TestTheCanaryReportsARecoveryOnce`,
`TestTheProbeTellsUpFromBroken` (401 reachable, 503 not, closed port not), plus the board side:
`TestASuccessfulCanaryDoesNotPromoteTheEndpoint` is the one to read — a canary may demote and never
promote.

## The release channel, run end to end

```
$ ./scripts/build-release.sh proof
released proof: 6 artifacts in dist/
$ nsm dist keygen --out /tmp/m6proof/nsm.key
wrote /tmp/m6proof/nsm.key (0600)
public key (pin this on every agent):
CWsm6O2sGaEwZVXplCMRUls/DE0ojamIxxMPB3h/zT8=
$ nsm dist sign --key /tmp/m6proof/nsm.key --dir dist
signed 6 artifacts with key CWsm6O2sGaEwZVXplCMRUls/DE0ojamIxxMPB3h/zT8=
$ nsm dist verify --pub CWsm6O2sGaEwZVXplCMRUls/DE0ojamIxxMPB3h/zT8= --dir dist
signature ok; this machine's artifact is nsm-proof-darwin-arm64
```

Served by a real manager (`nsm manager serve --dist dist`):

```
GET /dist/SHA256SUMS          with token → 200, byte-identical to dist/SHA256SUMS
GET /dist/SHA256SUMS          no token   → 401
GET /dist/files/nsm-proof-darwin-arm64 → 200, sha256 96ea09ef90cb7ea1… == the signed line
GET /dist/files/BUILD-INFO             → 404 (in the directory, not in SHA256SUMS)
```

`BUILD-INFO` is the interesting line: the artifact route serves only names the signed checksum file
lists, so a file that happens to be sitting in `dist/` is not reachable.

The self-upgrade (`nsm agent install --upgrade`) was **not** run by hand: it replaces the binary and
writes service files, and doing that on the machine writing this proof would install a daemon. It is
covered by `TestUpgradeReplacesTheBinaryAfterBothChecks`, `TestUpgradeRefusesATamperedArtifact`,
`TestUpgradeRefusesAnotherKeyAndAMissingToken` and `TestPrintAndUpgradeAreRefusedTogether`.

## The read-only replica, run end to end

A manager with one queued item, and `nsm replica serve` pointed at it:

```
=== replica board (manager UP) ===
{"cancelled":0,"claimed":0,"done":0,"failed":0,"needs_approval":0,"queued":1,"running":0}

=== write attempt on replica ===
405

$ pkill -f "nsm manager serve"        # the VPS is gone
=== replica board (manager DOWN) ===
HTTP/1.1 200 OK
{"cancelled":0,"claimed":0,"done":0,"failed":0,"needs_approval":0,"queued":1,"running":0}

$ tail -2 /tmp/m6proof/replica.log
replica: pull failed, still serving the last snapshot: Get "http://127.0.0.1:8091/admin/snapshot":
  dial tcp 127.0.0.1:8091: connect: connection refused
```

Two facts are load-bearing here. The replica kept answering **after** the manager died, which is the
only reason to have one; and the write was refused by a guard in front of the whole mux, not by a
list of safe routes — so a route added later cannot become writable on the replica by omission.

Response headers carried `X-Nsm-Replica: read-only` and a snapshot age, because a page that looks
live during an outage is worse than one that looks broken.

## What this proof does not show

- The replica has no lag banner *on the page* yet, only headers and a log line; the design memo asks
  for a visible number.
- Replication is whole-file, every interval. A board large enough for that to hurt would want
  Litestream; the memo argues the trade.
- No agent has been run on a Raspberry Pi. The install writes a systemd **user** unit that has been
  tested as text (`TestTheSystemdUnitRestartsAndIsPerUser`), not as a running service.
