package mna

import (
	"encoding/csv"
	"fmt"
	"log"
	"os"
)

// Logger tracks specified node voltages and runtime signal values over time.
type Logger struct {
	NodesToSave  []string // MNA node names to track from solution vector
	ExtraSignals []string // VM signal names to track (logged externally)
	TimeSteps    []float64
	Results      map[string][]float64 // Maps column name to values over time
}

// InitLogger sets up the memory for the simulation tracking.
// nodesToSave are MNA electrical nodes; extraSignals are VM runtime variables.
func InitLogger(nodesToSave []string, extraSignals []string) *Logger {
	results := make(map[string][]float64)
	for _, node := range nodesToSave {
		results[node] = make([]float64, 0)
	}
	for _, sig := range extraSignals {
		results[sig] = make([]float64, 0)
	}

	return &Logger{
		NodesToSave:  nodesToSave,
		ExtraSignals: extraSignals,
		TimeSteps:    make([]float64, 0),
		Results:      results,
	}
}

// LogStep grabs the current voltages from the math solution and saves them.
// Call LogSignals separately for VM runtime variables.
func (l *Logger) LogStep(t float64, sys *System, solution []float64) {
	l.TimeSteps = append(l.TimeSteps, t)

	for _, node := range l.NodesToSave {
		// If they ask for ground, it's always 0V
		if node == "0" || node == "gnd" {
			l.Results[node] = append(l.Results[node], 0.0)
			continue
		}

		// Otherwise, look up the node index and grab the voltage
		if idx, exists := sys.NodeMap[node]; exists {
			l.Results[node] = append(l.Results[node], solution[idx])
		} else {
			// If node wasn't found, save 0.0 to prevent crashing
			l.Results[node] = append(l.Results[node], 0.0)
		}
	}
}

// LogSignals records externally-provided runtime signal values for the current timestep.
// Called by the co-simulation loop after Phase A (VM execution).
func (l *Logger) LogSignals(values map[string]float64) {
	for _, sig := range l.ExtraSignals {
		if val, ok := values[sig]; ok {
			l.Results[sig] = append(l.Results[sig], val)
		} else {
			l.Results[sig] = append(l.Results[sig], 0.0)
		}
	}
}

// ExportCSV writes the logged data to a file.
// Columns are: Time, [MNA nodes...], [VM signals...]
func (l *Logger) ExportCSV(filename string) {
	file, err := os.Create(filename)
	if err != nil {
		log.Fatalf("Failed to create CSV: %v", err)
	}
	defer file.Close()

	writer := csv.NewWriter(file)
	defer writer.Flush()

	// Build ordered column list
	allColumns := make([]string, 0, len(l.NodesToSave)+len(l.ExtraSignals))
	allColumns = append(allColumns, l.NodesToSave...)
	allColumns = append(allColumns, l.ExtraSignals...)

	// 1. Write the Header (Time, node1, node2, ..., signal1, signal2, ...)
	header := []string{"Time"}
	header = append(header, allColumns...)
	writer.Write(header)

	// 2. Write the Data Rows
	for i, t := range l.TimeSteps {
		row := []string{fmt.Sprintf("%.6e", t)} // Scientific notation for small times
		for _, col := range allColumns {
			val := l.Results[col][i]
			row = append(row, fmt.Sprintf("%.6e", val))
		}
		writer.Write(row)
	}
	fmt.Printf("--- Simulation results saved to %s (%d columns, %d timesteps) ---\n",
		filename, len(allColumns)+1, len(l.TimeSteps))
}
