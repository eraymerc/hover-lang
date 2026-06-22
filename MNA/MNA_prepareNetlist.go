package mna

import (
	"bufio"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
)

// Component holds a parsed netlist line.
// Different types use different subsets of these fields:
//
//	R, C, I, L, V  → Name, Type, Node1, Node2, Value
//	G (VCCS)        → + CtrlNode1, CtrlNode2
//	E (VCVS)        → + CtrlNode1, CtrlNode2
//	F (CCCS)        → + SenseElement
//	H (CCVS)        → + SenseElement
type Component struct {
	Type         string
	Name         string
	Node1, Node2 string
	Value        float64
	CtrlNode1    string // VCCS, VCVS: control + node
	CtrlNode2    string // VCCS, VCVS: control - node
	SenseElement string // CCCS, CCVS: name of the branch to sense current through
}

func PrepareNetlist(fileName string, dt float64) *System {
	file, err := os.Open(fileName)
	if err != nil {
		log.Fatalf("Failed to open file: %v", err)
	}
	defer file.Close()

	var components []Component
	nodeMap := make(map[string]int)
	var nodeNames []string

	numBranches := 0

	registerNode := func(n string) {
		if n == "0" || n == "gnd" {
			return
		}
		if _, exists := nodeMap[n]; !exists {
			nodeMap[n] = len(nodeNames)
			nodeNames = append(nodeNames, n)
		}
	}

	// ── Pass 1: parse lines, register nodes, count branches ──────────────────

	lineNo := 0
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "//") {
			continue
		}

		parts := strings.Fields(line)
		if len(parts) < 4 {
			log.Printf("line %d: skipping malformed line %q (need at least 4 fields)", lineNo, line)
			continue
		}

		name := parts[0]
		compType := strings.ToUpper(string(name[0]))
		value, err := strconv.ParseFloat(parts[1], 64)
		if err != nil {
			log.Printf("line %d: bad value %q for %s — skipping", lineNo, parts[1], name)
			continue
		}

		var c Component
		c.Name = name
		c.Type = compType
		c.Value = value

		switch compType {
		case "R", "C", "I", "L", "V":
			// format: name value n1 n2
			if len(parts) < 4 {
				log.Printf("line %d: %s needs 4 fields", lineNo, name)
				continue
			}
			c.Node1, c.Node2 = parts[2], parts[3]
			registerNode(c.Node1)
			registerNode(c.Node2)
			if compType == "L" || compType == "V" {
				numBranches++
			}

		case "G", "E": // VCCS, VCVS — format: name value n1 n2 nc1 nc2
			if len(parts) < 6 {
				log.Fatalf("line %d: %s (VCCS/VCVS) needs 6 fields: name value n+ n- nc+ nc-", lineNo, name)
			}
			c.Node1, c.Node2 = parts[2], parts[3]
			c.CtrlNode1, c.CtrlNode2 = parts[4], parts[5]
			registerNode(c.Node1)
			registerNode(c.Node2)
			registerNode(c.CtrlNode1)
			registerNode(c.CtrlNode2)
			if compType == "E" { // VCVS needs a branch for its output current
				numBranches++
			}

		case "F", "H": // CCCS, CCVS — format: name value n1 n2 senseElement
			if len(parts) < 5 {
				log.Fatalf("line %d: %s (CCCS/CCVS) needs 5 fields: name value n+ n- senseElement", lineNo, name)
			}
			c.Node1, c.Node2 = parts[2], parts[3]
			c.SenseElement = parts[4]
			registerNode(c.Node1)
			registerNode(c.Node2)
			if compType == "H" { // CCVS needs a branch for its output current
				numBranches++
			}

		default:
			log.Printf("line %d: unknown component type %q — skipping", lineNo, compType)
			continue
		}

		components = append(components, c)
	}

	// Create the system now that node and branch counts are final.
	numNodes := len(nodeNames)
	sys := NewSystem(numNodes, numBranches, nodeMap, nodeNames, dt)

	// Assign branch indices into sys.BranchNameToIdx.
	// Walk components in declaration order — same order stamps will use.
	nextBranchIdx := numNodes // MNA branches start right after node rows
	for _, c := range components {
		switch c.Type {
		case "L", "V", "E", "H":
			sys.BranchNameToIdx[c.Name] = nextBranchIdx
			nextBranchIdx++
		}
	}

	// ── Pass 2: stamp every component ────────────────────────────────────────

	resolveNode := func(n string) int {
		if n == "0" || n == "gnd" {
			return -1
		}
		return nodeMap[n]
	}

	for _, c := range components {
		n1 := resolveNode(c.Node1)
		n2 := resolveNode(c.Node2)

		switch c.Type {
		case "R":
			sys.StampResistor(n1, n2, c.Value)

		case "C":
			sys.StampCapacitor(n1, n2, c.Value)

		case "I":
			sys.StampCurrentSource(n1, n2, c.Value, c.Name)

		case "L":
			sys.StampInductor(n1, n2, c.Value, sys.BranchNameToIdx[c.Name])

		case "V":
			sys.StampIdealVoltageSource(n1, n2, c.Value, sys.BranchNameToIdx[c.Name])

		case "G": // VCCS
			nc1 := resolveNode(c.CtrlNode1)
			nc2 := resolveNode(c.CtrlNode2)
			sys.StampVCCS(n1, n2, nc1, nc2, c.Value)

		case "E": // VCVS
			nc1 := resolveNode(c.CtrlNode1)
			nc2 := resolveNode(c.CtrlNode2)
			sys.StampVCVS(n1, n2, nc1, nc2, c.Value, sys.BranchNameToIdx[c.Name])

		case "F": // CCCS
			sensBr, ok := sys.BranchNameToIdx[c.SenseElement]
			if !ok {
				log.Fatalf("CCCS %s: sense element %q not found or has no branch — declare it before %s in the netlist",
					c.Name, c.SenseElement, c.Name)
			}
			sys.StampCCCS(n1, n2, sensBr, c.Value)

		case "H": // CCVS
			sensBr, ok := sys.BranchNameToIdx[c.SenseElement]
			if !ok {
				log.Fatalf("CCVS %s: sense element %q not found or has no branch — declare it before %s in the netlist",
					c.Name, c.SenseElement, c.Name)
			}
			sys.StampCCVS(n1, n2, sensBr, c.Value, sys.BranchNameToIdx[c.Name])
		}
	}

	fmt.Printf("Netlist loaded: %d nodes, %d branches, matrix size %dx%d\n",
		numNodes, numBranches, sys.Size, sys.Size)
	return sys
}
