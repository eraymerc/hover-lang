package mna

import "fmt"

// API is the bridge between the logical interpreter and the MNA engine.
// It holds a pointer to the last computed solution so the logical layer
// can read voltages and currents, and it exposes setters that update
// B_static so changes are picked up on the next timestep solve.
//
// One API instance is created per simulation and passed into every
// LogicalBlock that runs during Phase A of the timestep loop.
type API struct {
	sys          *System
	lastSolution []float64 // updated by the engine after every solve

	// GDirty signals to the engine that G changed and LU must be refactorized.
	// Set to true by SetResistor (and any future GDynamic setter).
	// The engine resets it to false after refactorization.
	GDirty bool
}

// NewAPI creates an API bound to a system.
// lastSolution starts as all zeros (the DC initial condition).
func NewAPI(sys *System) *API {
	return &API{
		sys:          sys,
		lastSolution: make([]float64, sys.Size),
	}
}

// UpdateSolution is called by the engine after every successful solve.
// It makes the new node voltages and branch currents available to the
// logical layer on the next timestep.
func (api *API) UpdateSolution(solution []float64) {
	copy(api.lastSolution, solution)
}

// ── READ OPERATIONS ──────────────────────────────────────────────────────────

// V returns the voltage at a named net node from the last MNA solution.
// Returns 0.0 for ground ("gnd" / "0") and for unknown net names with a warning.
//
// Language intrinsic: V(netName)
func (api *API) V(netName string) float64 {
	if netName == "gnd" || netName == "0" {
		return 0.0
	}
	idx, ok := api.sys.NodeMap[netName]
	if !ok {
		fmt.Printf("[MNA API] warning: V(%q) — net not found, returning 0\n", netName)
		return 0.0
	}
	return api.lastSolution[idx]
}

// I returns the branch current through a named element from the last MNA solution.
// Only valid for branch elements: V (voltage sources), L (inductors),
// E (VCVS), H (CCVS), and 0V sense sources.
// Returns 0.0 with a warning for unknown element names.
//
// Language intrinsic: I(elementName)
func (api *API) I(elementName string) float64 {
	idx, ok := api.sys.BranchNameToIdx[elementName]
	if !ok {
		fmt.Printf("[MNA API] warning: I(%q) — element has no branch current (is it an R, C, or G?), returning 0\n", elementName)
		return 0.0
	}
	return api.lastSolution[idx]
}

// ── WRITE OPERATIONS ─────────────────────────────────────────────────────────

// SetVoltageSource updates the output voltage of a named voltage source (V or E).
// Only touches B_static — no G change, no LU refactorization needed.
// The new value is picked up on the very next timestep solve.
//
// Language usage: voltage_source(signal)[out, gnd]
// Called by the engine's Phase B restamp for BDynamic bindings.
func (api *API) SetVoltageSource(name string, voltage float64) {
	branchIdx, ok := api.sys.BranchNameToIdx[name]
	if !ok {
		fmt.Printf("[MNA API] error: SetVoltageSource(%q) — element not found or has no branch\n", name)
		return
	}
	// B_static[branchIdx] is a direct assignment (not accumulation) for voltage sources.
	// This matches how StampIdealVoltageSource originally set it.
	api.sys.B_static[branchIdx] = voltage
	
}

// SetCurrentSource updates the output current of a named current source (I).
// Unstamps the previous value from B_static then stamps the new one.
// Only touches B_static — no G change, no LU refactorization needed.
//
// Language usage: current_source(signal)[out, gnd]
// Called by the engine's Phase B restamp for BDynamic bindings.
func (api *API) SetCurrentSource(name string, current float64) {
	rec, ok := api.sys.CurrentSources[name]
	if !ok {
		fmt.Printf("[MNA API] error: SetCurrentSource(%q) — current source not found\n", name)
		return
	}

	// Unstamp old value
	if rec.N1 >= 0 {
		api.sys.B_static[rec.N1] += rec.LastValue // reverse the original -=
	}
	if rec.N2 >= 0 {
		api.sys.B_static[rec.N2] -= rec.LastValue // reverse the original +=
	}

	// Stamp new value
	if rec.N1 >= 0 {
		api.sys.B_static[rec.N1] -= current
	}
	if rec.N2 >= 0 {
		api.sys.B_static[rec.N2] += current
	}

	// Update record so next call can unstamp correctly
	rec.LastValue = current
}

// SetResistor updates the resistance of a named resistor.
// Unstamps the old conductance from G then stamps the new one.
// Sets GDirty = true — the engine will recompute G_eff and refactorize LU
// before the next solve. This is the expensive path; use sparingly.
//
// Language usage: resistor(signal)[n1, n2]
// Called by the engine's Phase B restamp for GDynamic bindings.
func (api *API) SetResistor(name string, n1, n2 int, oldResistance, newResistance float64) {
	if newResistance == 0 {
		fmt.Printf("[MNA API] error: SetResistor(%q) — zero resistance would cause singularity\n", name)
		return
	}

	// Unstamp old conductance
	oldG := 1.0 / oldResistance
	api.sys.StampResistor(n1, n2, -1.0/oldG) // negative resistance = unstamp

	// Stamp new conductance
	api.sys.StampResistor(n1, n2, newResistance)

	// Mark G as dirty — engine must refactorize before next solve
	api.GDirty = true
}
