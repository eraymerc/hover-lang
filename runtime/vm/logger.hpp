#pragma once

#include <string>
#include <vector>
#include <unordered_map>
#include "../mna/system.hpp"

// ─────────────────────────────────────────────────────────────────────────────
// LOGGER
//
// Tracks node voltages and VM signal values over time, exports to CSV.
// Mirrors Go:
//   type Logger struct {
//       NodesToSave, ExtraSignals []string
//       TimeSteps                 []float64
//       Results                   map[string][]float64
//   }
// ─────────────────────────────────────────────────────────────────────────────

struct Logger {
    std::vector<std::string>                         nodes_to_save;   // MNA node names
    std::vector<std::string>                         extra_signals;   // VM signal names
    std::vector<double>                              time_steps;
    std::unordered_map<std::string, std::vector<double>> results;
};

// Initialise logger with the lists of signals to track.
// Mirrors Go: func InitLogger(nodesToSave, extraSignals []string) *Logger
void logger_init(Logger *log,
                 const std::vector<std::string> &nodes_to_save,
                 const std::vector<std::string> &extra_signals);

// Record MNA node voltages for the current timestep.
// Mirrors Go: func (l *Logger) LogStep(t float64, sys *System, solution []float64)
void logger_log_step(Logger *log, double t,
                     const System *sys,
                     const Eigen::VectorXd &solution);

// Record VM signal values for the current timestep.
// Mirrors Go: func (l *Logger) LogSignals(values map[string]float64)
void logger_log_signals(Logger *log,
                        const std::unordered_map<std::string, double> &values);

// Write all logged data to a CSV file.
// Mirrors Go: func (l *Logger) ExportCSV(filename string)
void logger_export_csv(const Logger *log, const char *filename);