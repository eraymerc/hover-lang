# Design: native `ddt()` / `idt()`

**Status:** design only, nothing implemented. Written 2026-08-10.

## What exists today

`ddt(expr)` and `idt(expr)` are *elaborator rewrites*, not runtime operators.
`processAnalogDdt` in [`compiler/elaborator/macros.go`](../../compiler/elaborator/macros.go)
turns `ddt(expr)` into three injected statements plus a substitution:

```
double __hidden_ddt_0_M = expr;            // local decl
L<1.0>()            [__hidden_ddt_0_M, gnd];   // 1 henry to ground
current_source<>(__hidden_ddt_0_M) [gnd, __hidden_ddt_0_M];
// ddt(expr)  ->  V(__hidden_ddt_0_M)
```

KCL at the hidden node forces the inductor's current to equal `expr`, so its
terminal voltage is `L·d(expr)/dt`, and with `L = 1` the hidden node voltage *is*
the derivative. `idt()` is the exact dual with a 1 farad capacitor.

This is genuinely elegant. It performs no numerical differentiation, and it
inherits whatever order and stability the active solver already has for
inductors — for free, with no per-solver work.

## Why change it

Each `ddt` adds **two MNA unknowns**, and neither carries the units the solver
assumes for its row class:

| row | holds | tested against |
|---|---|---|
| `V(__hidden_ddt_N)` | `d(expr)/dt` — for `ddt(q_d)`, **amps** | `atol`, documented as volts |
| inductor branch current | `expr` — for `ddt(q_d)`, **coulombs** | `abstol`, documented as amps |

[`stdlib/semiconductors/diode.hvr`](../../stdlib/semiconductors/diode.hvr) line 274
is `ddt(q_d)` with `q_d` a charge, so both rows above are live in every circuit
containing a diode. The consequence is not a wrong answer but an *unmeasured*
one: a 1 µV floor sits on a capacitive current that is nanoamps at commutation,
and a 1 pA floor sits on a junction charge that is itself around 1e-12 C. Those
rows are effectively outside the convergence test.

Beyond the tolerance question, the synthetic rows are visible everywhere a real
node is: the convergence test, the trust region's scale vector (whose floor is a
flat `1e-6` for volts and amps alike), the Jacobian's perturbation sweep, and
equilibration. And a 1 H inductor to ground sitting next to a junction
conductance of 3e-7 S is a large contributor to the row-magnitude spread that
equilibration exists to fight — the failure dumps show these rows at `2^40` while
neighbours sit at `2^1`.

There is also a structural amplification worth naming. The hidden node's only
conductive tie to the circuit is the inductor's companion term, `dt/L`. As `dt`
shrinks that tie vanishes and `V(hidden) = expr·L/dt` grows without bound. So
when the step controller responds to trouble by halving `dt`, it directly
amplifies the very rows that are failing. This was visible in the rectifier
before the Jacobian fix: `V(__hidden_ddt_0_Diode) = 2.716271e8` against
`I(L_14)/dt = 4.047560e-4 / 1.490116e-12`, matching to all printed digits.

## What SPICE and Verilog-AMS do instead

Neither introduces an unknown. `ddt` is an operator on the integration formula:
the device evaluates its charge `Q(x)` and stamps

- `alpha · dQ/dV` into the Jacobian, and
- `alpha · (Q(x) − Q_history)` into the RHS,

where `alpha` is the same integration coefficient the solver already computes
(`1/dt` for BE, `1.5/dt` for constant-step BDF2). The derivative never becomes a
solved-for quantity; it is a term in the equations of the node the device is
already attached to.

## Design work required

1. **Charge history storage.** Today the history is implicit in the inductor's
   own state, which the snapshot/restore machinery already handles. Natively it
   needs a per-instance slot in the VM, keyed so that a module instantiated
   twice gets two slots — the same mangling problem `mangleNode` already solves
   for nodes.

2. **Getting `alpha` to the model.** Every solver computes it; no analog block
   can currently see it. This is a new piece of surface between the solver and
   the analog runtime, and it must be the *same* `alpha` the caller's
   `solver_solve_rhs` is using that iteration, or the companion model and the
   residual disagree.

3. **Step rejection and the BDF2 primer.** `alpha` changes from `1.5/dt` to
   `1/dt` on any priming step, and BDF2 primes after every rejection
   (`step_count = 0`). Charge history must be checkpointed and restored in step
   with `vm_save_state` / `vm_restore_state`, or a rejected step leaves the
   device remembering a charge from a timeline that was abandoned.

4. **Order.** The current rewrite genuinely inherits BDF2's second order,
   because the inductor is integrated by the same formula as everything else. A
   native stamp must use the same multistep blend (`x_blend`, not just
   `x_prev1`) rather than quietly degrading to backward Euler. This is the
   easiest part to get subtly wrong and the hardest to notice, since first order
   at a small step still looks plausible.

5. **`idt()`'s dual**, with the same four questions, plus the initial-condition
   convention that the capacitor form currently gets from the solver's cold
   start.

6. **Keep the numerical Jacobian honest.** `dQ/dV` would now be needed at the
   device. Today it falls out of `vm_compute_jacobian`'s perturbation sweep
   because the charge is a node quantity; natively the sweep must still see it,
   or the term is missing from the matrix entirely.

## Recommendation

Do this as its own piece of work, not folded into solver debugging. It touches
the elaborator, the analog runtime, the solver interface and every device model
that uses charge storage, and the current rewrite — for all its costs — is
*correct*. The tolerance mis-assignment it causes is real but has not yet been
shown to break a circuit on its own; the one failure that named a `ddt` row
turned out to be a symptom of the stale-Jacobian bug, and disappeared when that
was fixed.

An intermediate step, if the full rework is not wanted soon: have the elaborator
record which branches and nodes it synthesized and expose that to `System`, so
the convergence test can give those rows a relative-only tolerance instead of an
absolute floor in the wrong unit. That is a contained change and it removes the
dimensional error without touching how derivatives are computed.
