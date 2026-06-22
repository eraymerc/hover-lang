package elaborator

import (
	"fmt"
	"hover/Interpreter/token"
	mna "hover/MNA"
	"strings"
)

func (e *Elaborator) Elaborate() (*ElaboratedProgram, *mna.System, error) {
	main, ok := e.modules["main"]
	if !ok {
		return nil, nil, fmt.Errorf("module 'main' not found")
	}

	for _, mod := range e.modules {
		if mod.Token.Type == token.ANALOG {
			e.processAnalogIdt(mod)
			e.processAnalogDdt(mod)
		}
	}

	e.flattenModule(main, "main", make(map[string]float64), make(map[string]string), main.Token.Type)

	if len(e.errors) > 0 {
		return nil, nil, fmt.Errorf("elaboration failed:\n%s", strings.Join(e.errors, "\n"))
	}

	nodeMap := make(map[string]int)
	var nodeNames []string
	branchCount := 0
	for _, p := range e.output.Physicals {
		for _, n := range p.Nodes {
			if n != "gnd" && n != "0" {
				if _, ok := nodeMap[n]; !ok {
					nodeMap[n] = len(nodeNames)
					nodeNames = append(nodeNames, n)
				}
			}
		}
		if isBranch(p.Type) {
			branchCount++
		}
	}

	dt := 1e-6
	for _, d := range e.output.Directives {
		if d.Name == "tran" && len(d.Args) >= 3 {
			dt = ParseEngineering(d.Args[2].String())
		}
	}

	sys := mna.NewSystem(len(nodeNames), branchCount, nodeMap, nodeNames, dt)
	nextBranch := len(nodeNames)
	for _, p := range e.output.Physicals {
		nID := func(i int) int {
			if i >= len(p.Nodes) || p.Nodes[i] == "gnd" {
				return -1
			}
			return nodeMap[p.Nodes[i]]
		}
		if isBranch(p.Type) {
			sys.BranchNameToIdx[p.Name] = nextBranch
			nextBranch++
		}
		val := p.Parameters["param0"]
		switch strings.ToLower(p.Type) {
		case "r", "resistor":
			sys.StampResistor(nID(0), nID(1), val)
		case "c", "capacitor":
			sys.StampCapacitor(nID(0), nID(1), val)
		case "l", "inductor":
			sys.StampInductor(nID(0), nID(1), val, sys.BranchNameToIdx[p.Name])
		case "voltage_source":
			sys.StampIdealVoltageSource(nID(0), nID(1), val, sys.BranchNameToIdx[p.Name])
		case "vcvs", "e":
			// [n+, n-, nc+, nc-]
			sys.StampVCVS(nID(0), nID(1), nID(2), nID(3), val, sys.BranchNameToIdx[p.Name])
		case "vccs", "g":
			// [n+, n-, nc+, nc-] -> no branch var needed
			sys.StampVCCS(nID(0), nID(1), nID(2), nID(3), val)

		case "current_source", "i":
			sys.StampCurrentSource(nID(0), nID(1), val, p.Name)

		}
	}

	return e.output, sys, nil
}
