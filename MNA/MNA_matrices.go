package mna

import (
	"fmt"

	"github.com/james-bowman/sparse"
)

// CurrentSourceRecord stores the connection and last stamped value of a current
// source so the API can unstamp the old value before stamping the new one.
type CurrentSourceRecord struct {
	N1, N2    int
	LastValue float64
}

type System struct {
	G         *sparse.DOK // Static conductances and MNA branch routing
	C         *sparse.DOK // Physical Capacitors and Inductors (-L)
	B_static  []float64   // Permanent forces (DC sources)
	B_dynamic []float64   // Temporary forces for the loop

	NodeMap   map[string]int
	NodeNames []string
	NumNodes  int // N
	Size      int // N + M (Total matrix size)
	Dt        float64

	// BranchNameToIdx maps element name → MNA branch row index.
	// Populated by PrepareNetlist for every V, L, E, H element.
	BranchNameToIdx map[string]int

	// CurrentSources tracks every stamped current source by name so the API
	// can unstamp the old value before applying a new one.
	CurrentSources map[string]*CurrentSourceRecord
}

func NewSystem(numNodes, numBranches int, nodeMap map[string]int, nodeNames []string, dt float64) *System {
	size := numNodes + numBranches

	return &System{
		G:               sparse.NewDOK(size, size),
		C:               sparse.NewDOK(size, size),
		B_static:        make([]float64, size),
		B_dynamic:       make([]float64, size),
		NodeMap:         nodeMap,
		NodeNames:       nodeNames,
		NumNodes:        numNodes,
		Size:            size,
		Dt:              dt,
		BranchNameToIdx: make(map[string]int),
		CurrentSources:  make(map[string]*CurrentSourceRecord),
	}
}

// --- HELPER ---
// dokAdd accumulates a value into a DOK cell (DOK.Set overwrites, so we read first)
func dokAdd(m *sparse.DOK, i, j int, val float64) {
	m.Set(i, j, m.At(i, j)+val)
}

// --- STAMPING FUNCTIONS ---

func (sys *System) StampResistor(n1, n2 int, resistance float64) {
	g := 1.0 / resistance
	if n1 >= 0 {
		dokAdd(sys.G, n1, n1, g)
	}
	if n2 >= 0 {
		dokAdd(sys.G, n2, n2, g)
	}
	if n1 >= 0 && n2 >= 0 {
		dokAdd(sys.G, n1, n2, -g)
		dokAdd(sys.G, n2, n1, -g)
	}
}

func (sys *System) StampCapacitor(n1, n2 int, capacitance float64) {
	if n1 >= 0 {
		dokAdd(sys.C, n1, n1, capacitance)
	}
	if n2 >= 0 {
		dokAdd(sys.C, n2, n2, capacitance)
	}
	if n1 >= 0 && n2 >= 0 {
		dokAdd(sys.C, n1, n2, -capacitance)
		dokAdd(sys.C, n2, n1, -capacitance)
	}
}

// Ideal voltage source
func (sys *System) StampIdealVoltageSource(n1, n2 int, voltage float64, branchIdx int) {
	if n1 >= 0 {
		dokAdd(sys.G, n1, branchIdx, 1.0)
		dokAdd(sys.G, branchIdx, n1, 1.0)
	}
	if n2 >= 0 {
		dokAdd(sys.G, n2, branchIdx, -1.0)
		dokAdd(sys.G, branchIdx, n2, -1.0)
	}
	// Small resistor on branch diagonal to prevent singularities
	dokAdd(sys.G, branchIdx, branchIdx, -1e-6)

	sys.B_static[branchIdx] = voltage
}

// True MNA Inductor (treated as a branch with -L in the C matrix)
func (sys *System) StampInductor(n1, n2 int, inductance float64, branchIdx int) {
	if n1 >= 0 {
		dokAdd(sys.G, n1, branchIdx, 1.0)
		dokAdd(sys.G, branchIdx, n1, 1.0)
	}
	if n2 >= 0 {
		dokAdd(sys.G, n2, branchIdx, -1.0)
		dokAdd(sys.G, branchIdx, n2, -1.0)
	}
	// Stamp the physics into C (-L * di/dt)
	dokAdd(sys.C, branchIdx, branchIdx, -inductance)
}

func (sys *System) StampCurrentSource(n1, n2 int, current float64, name string) {
	if n1 >= 0 {
		sys.B_static[n1] -= current
	}
	if n2 >= 0 {
		sys.B_static[n2] += current
	}
	sys.CurrentSources[name] = &CurrentSourceRecord{N1: n1, N2: n2, LastValue: current}
}

// --- CONTROLLED SOURCE STAMPS ---
//
// Convention for all four:
//   n1, n2       = output + and - nodes (-1 means ground)
//   nc1, nc2     = control + and - nodes (-1 means ground)
//   sensBranchIdx = index of the sensing branch (for CC types)
//   branchIdx    = new MNA branch index allocated for this source (for VS types)

// VCCS — Voltage-Controlled Current Source
// I_out = gm * (V_nc1 - V_nc2), current flows into n1 and out of n2.
// Pure G stamp — no new branch variable needed.
//
//   G[n1][nc1] += gm    G[n1][nc2] -= gm
//   G[n2][nc1] -= gm    G[n2][nc2] += gm
func (sys *System) StampVCCS(n1, n2, nc1, nc2 int, gm float64) {
	if n1 >= 0 {
		if nc1 >= 0 {
			dokAdd(sys.G, n1, nc1, gm)
		}
		if nc2 >= 0 {
			dokAdd(sys.G, n1, nc2, -gm)
		}
	}
	if n2 >= 0 {
		if nc1 >= 0 {
			dokAdd(sys.G, n2, nc1, -gm)
		}
		if nc2 >= 0 {
			dokAdd(sys.G, n2, nc2, gm)
		}
	}
}

// VCVS — Voltage-Controlled Voltage Source
// V_n1 - V_n2 = k * (V_nc1 - V_nc2).
// Needs one new branch variable (branchIdx) for the output current.
//
// KCL rows:    G[n1][br] += 1    G[n2][br] -= 1
// Branch eq:   G[br][n1] += 1    G[br][n2] -= 1
//              G[br][nc1] -= k   G[br][nc2] += k
//              B[br] = 0  (already zero)
func (sys *System) StampVCVS(n1, n2, nc1, nc2 int, k float64, branchIdx int) {
	if n1 >= 0 {
		dokAdd(sys.G, n1, branchIdx, 1.0)
		dokAdd(sys.G, branchIdx, n1, 1.0)
	}
	if n2 >= 0 {
		dokAdd(sys.G, n2, branchIdx, -1.0)
		dokAdd(sys.G, branchIdx, n2, -1.0)
	}
	if nc1 >= 0 {
		dokAdd(sys.G, branchIdx, nc1, -k)
	}
	if nc2 >= 0 {
		dokAdd(sys.G, branchIdx, nc2, k)
	}
	// B_static[branchIdx] stays 0 — the control voltage drives it, not a fixed value
}

// CCCS — Current-Controlled Current Source
// I_out = beta * I_sense, where I_sense is the current through sensBranchIdx.
// sensBranchIdx must already exist (stamp a 0 V source on the sensing wire first).
// No new branch variable needed for the output.
//
//   G[n1][sensBr] += beta
//   G[n2][sensBr] -= beta
func (sys *System) StampCCCS(n1, n2, sensBranchIdx int, beta float64) {
	if n1 >= 0 {
		dokAdd(sys.G, n1, sensBranchIdx, beta)
	}
	if n2 >= 0 {
		dokAdd(sys.G, n2, sensBranchIdx, -beta)
	}
}

// CCVS — Current-Controlled Voltage Source
// V_n1 - V_n2 = r * I_sense, where I_sense is the current through sensBranchIdx.
// sensBranchIdx must already exist (stamp a 0 V source on the sensing wire first).
// Needs one new branch variable (branchIdx) for the output current.
//
// KCL rows:    G[n1][br] += 1    G[n2][br] -= 1
// Branch eq:   G[br][n1] += 1    G[br][n2] -= 1
//              G[br][sensBr] -= r
//              B[br] = 0  (already zero)
func (sys *System) StampCCVS(n1, n2, sensBranchIdx int, r float64, branchIdx int) {
	if n1 >= 0 {
		dokAdd(sys.G, n1, branchIdx, 1.0)
		dokAdd(sys.G, branchIdx, n1, 1.0)
	}
	if n2 >= 0 {
		dokAdd(sys.G, n2, branchIdx, -1.0)
		dokAdd(sys.G, branchIdx, n2, -1.0)
	}
	dokAdd(sys.G, branchIdx, sensBranchIdx, -r)
}

// Print outputs the G matrix with MNA branch labels
func (sys *System) Print() {
	fmt.Println("=== G MATRIX (Static, Sparse DOK) ===")
	fmt.Printf("  NNZ: %d / %d (%.1f%% sparse)\n",
		sys.G.NNZ(), sys.Size*sys.Size,
		100.0*(1.0-float64(sys.G.NNZ())/float64(sys.Size*sys.Size)))
	fmt.Print("          ")

	for i := 0; i < sys.Size; i++ {
		if i < sys.NumNodes {
			fmt.Printf("%10s", sys.NodeNames[i])
		} else {
			fmt.Printf("%10s", fmt.Sprintf("I_br%d", i-sys.NumNodes))
		}
	}
	fmt.Println()

	for i := 0; i < sys.Size; i++ {
		if i < sys.NumNodes {
			fmt.Printf("%8s |", sys.NodeNames[i])
		} else {
			fmt.Printf("%8s |", fmt.Sprintf("I_br%d", i-sys.NumNodes))
		}
		for j := 0; j < sys.Size; j++ {
			fmt.Printf("%10.3f", sys.G.At(i, j))
		}
		fmt.Println()
	}
}
