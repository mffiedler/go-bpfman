# TC Dispatcher E2E Test Gaps

Gaps in e2e coverage for the TC dispatcher, ranked by likelihood of
catching a real bug. Each entry identifies the untested code path and
why it matters.

## 1. Egress direction -- partially covered

`TestTC_IngressEgressIndependence` attaches programs to egress and
verifies the egress dispatcher is created, its config is correct, and
it tears down independently of ingress. A dedicated test that sends
outbound traffic through a veth pair and verifies the egress
program's stats map shows non-zero packet counts would still add
value.

## 2. Slot reuse after detach -- DONE

Covered by `TestTC_SlotReusedAfterDetach` (single slot reclaim) and
`TestTC_DispatcherSineWave` (repeated fill/drain across shifting slot
boundaries with traffic verification).

## 3. Dispatcher lifecycle after last extension removed -- DONE

Covered by `TestTC_DispatcherLifecycleAfterLastDetach`, which verifies
the full teardown path (store deletion, TC filter removal, pin
cleanup) and confirms a subsequent attach creates a fresh dispatcher.

## 4. Ingress and egress on the same interface -- DONE

Covered by `TestTC_IngressEgressIndependence`, which attaches
programs to both ingress and egress on the same interface, detaches
all egress links, and verifies the ingress dispatcher config
(program count, run_order, chain_call_actions) is unchanged.

## 5. Multiple interfaces with the same program -- DONE

Covered by `TestTC_MultipleInterfacesIndependent`, which attaches the
same program to ingress on two interfaces, detaches all links from
one, and verifies the other's dispatcher config is unchanged.

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
