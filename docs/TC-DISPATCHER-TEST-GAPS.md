# TC Dispatcher E2E Test Gaps

Gaps in e2e coverage for the TC dispatcher, ranked by likelihood of
catching a real bug. Each entry identifies the untested code path and
why it matters.

## 1. Egress direction

All TC dispatcher tests use `TCDirectionIngress`. Egress uses a
different netlink parent handle (`HANDLE_MIN_EGRESS`), a different
dispatcher type (`DispatcherTypeTCEgress`), and different bpffs paths.
A wiring bug in any of those would be invisible today.

**Test:** Load a program, attach to egress, send outbound traffic
through a veth pair, verify the program's stats map shows non-zero
packet counts.

## 2. Slot reuse after detach -- DONE

Covered by `TestTC_SlotReusedAfterDetach` (single slot reclaim) and
`TestTC_DispatcherSineWave` (repeated fill/drain across shifting slot
boundaries with traffic verification).

## 3. Dispatcher lifecycle after last extension removed -- DONE

Covered by `TestTC_DispatcherLifecycleAfterLastDetach`, which verifies
the full teardown path (store deletion, TC filter removal, pin
cleanup) and confirms a subsequent attach creates a fresh dispatcher.

## 4. Ingress and egress on the same interface

Two dispatchers coexist keyed by `(nsid, ifindex, direction)`. No
test confirms they are independent -- that detaching all egress links
does not disturb the ingress dispatcher.

**Test:** Attach to both ingress and egress on the same interface.
Detach all egress links. Verify the ingress dispatcher config is
unchanged.

## 5. Multiple interfaces with the same program

Dispatchers are keyed by ifindex. No test attaches the same program
to two interfaces and verifies independent dispatcher state.

**Test:** Create two dummy interfaces, attach the same program to
ingress on both, detach from one, verify the other is unaffected.

## 6. Priority ties

The config sorts slots by `(priority, program_name)` as a secondary
key. No test exercises the secondary sort or verifies deterministic
ordering when priorities collide.

**Test:** Load two distinctly-named programs, attach both at the same
priority, read the runtime config, assert the run_order matches
alphabetical program-name ordering.

## 7. Detach from the middle of a full dispatcher -- DONE

Covered by `TestTC_DispatcherSineWave`, which drains from both low
and high ends of the slot range across three oscillations, verifying
run_order and chain_call_actions after each drain.

## 8. Config map double-buffer flip -- partially covered

`TestTC_DispatcherSineWave` reads and verifies the runtime config
(including the active buffer contents) after every fill and drain
cycle, exercising the double-buffer across many transitions. A
dedicated test that explicitly asserts the active index toggles on
each operation would still add value.
