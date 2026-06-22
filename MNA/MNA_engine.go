package mna

import "gonum.org/v1/gonum/mat"

type Solver struct {
	Sys          *System
	LU           mat.LU
	LastSolution []float64
	gEffData     []float64
	lastAlpha    float64

	// HIGH-PERFORMANCE SCRATCHPADS: Eliminates heap allocation spam entirely
	bDense    *mat.Dense
	xDense    *mat.Dense
	xData     []float64
	xResult   []float64
	gxScratch []float64
	dScratch  []float64
}

func NewSolver(sys *System) *Solver {
	n := sys.Size
	xData := make([]float64, n)
	return &Solver{
		Sys:          sys,
		LastSolution: make([]float64, n),
		gEffData:     make([]float64, n*n),

		// Map dense structures directly onto existing memory slices once at initialization
		bDense:    mat.NewDense(n, 1, sys.B_dynamic),
		xDense:    mat.NewDense(n, 1, xData),
		xData:     xData,
		xResult:   make([]float64, n),
		gxScratch: make([]float64, n),
		dScratch:  make([]float64, n),
	}
}

// ── PRIMITIVE 1 ──────────────────────────────────────────────────────────────

func (s *Solver) Factorize(alpha float64, jacobian []float64) {
	n := s.Sys.Size
	s.lastAlpha = alpha
	for i := range s.gEffData {
		s.gEffData[i] = 0.0
	}

	s.Sys.G.DoNonZero(func(i, j int, v float64) {
		s.gEffData[i*n+j] = v
	})
	s.Sys.C.DoNonZero(func(i, j int, v float64) {
		s.gEffData[i*n+j] += alpha * v
	})

	if jacobian != nil {
		for i := 0; i < n*n; i++ {
			s.gEffData[i] += jacobian[i]
		}
	}

	gEff := mat.NewDense(n, n, s.gEffData)
	s.LU.Factorize(gEff)
}

// ── PRIMITIVE 2 ──────────────────────────────────────────────────────────────

func (s *Solver) SolveRHS(api *API, bScale float64, xHistory []float64, alpha float64, correction []float64, jacobian []float64) []float64 {

	// Refactorize ONLY if topology altered, timestep shifted, OR Jacobian values changed
	// After — O(1)
	if api.GDirty || s.lastAlpha != alpha {
		s.Factorize(alpha, jacobian)
		api.GDirty = false
	}

	// Build RHS directly into pre-allocated memory structures
	for i, b := range s.Sys.B_static {
		s.Sys.B_dynamic[i] = bScale * b
	}
	s.Sys.C.DoNonZero(func(i, j int, v float64) {
		s.Sys.B_dynamic[i] += alpha * v * xHistory[j]
	})
	if correction != nil {
		for i, c := range correction {
			s.Sys.B_dynamic[i] += c
		}
	}

	// Zero-allocation solve using pre-allocated matrix handles
	if err := s.LU.SolveTo(s.xDense, false, s.bDense); err != nil {
		panic("MNA Solver: singular matrix")
	}

	// Return data via internal reusable slice to avoid GC allocation thrashing
	copy(s.xResult, s.xData)
	return s.xResult
}

// ── PRIMITIVE 3 ──────────────────────────────────────────────────────────────

func (s *Solver) ComputeGx(x []float64) []float64 {
	for i := range s.gxScratch {
		s.gxScratch[i] = 0.0
	}
	s.Sys.G.DoNonZero(func(i, j int, v float64) {
		s.gxScratch[i] += v * x[j]
	})
	return s.gxScratch
}

// ── COMMIT ───────────────────────────────────────────────────────────────────

func (s *Solver) AdvanceTime(solution []float64, api *API) {
	copy(s.LastSolution, solution)
	api.UpdateSolution(solution)
}

// ── CONVENIENCE WRAPPERS ─────────────────────────────────────────────────────

func (s *Solver) Step(api *API) {
	alpha := 1.0 / s.Sys.Dt
	result := s.SolveRHS(api, 1.0, s.LastSolution, alpha, nil, nil)
	s.AdvanceTime(result, api)
}

func (s *Solver) ComputeDerivatives(x []float64) []float64 {
	gx := s.ComputeGx(x)
	for i := range s.dScratch {
		s.dScratch[i] = 0.0 // clear instead of allocate
	}
	s.Sys.C.DoNonZero(func(i, j int, v float64) {
		if i == j && v > 0 {
			s.dScratch[i] = (s.Sys.B_static[i] - gx[i]) / v
		}
	})
	return s.dScratch
}
