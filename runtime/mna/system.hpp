#pragma once

#include <Eigen/Dense>
#include <string>
#include <unordered_map>
#include <vector>

// ─────────────────────────────────────────────────────────────────────────────
// CURRENT SOURCE RECORD
// Tracks each current source so SetCurrentSource can unstamp before restamping.
// Mirrors Go: type CurrentSourceRecord struct { N1, N2 int; LastValue float64 }
// ─────────────────────────────────────────────────────────────────────────────

struct CurrentSourceRecord {
    int    n1;           // positive node index (-1 = ground)
    int    n2;           // negative node index (-1 = ground)
    double last_value;
};

// ─────────────────────────────────────────────────────────────────────────────
// SYSTEM
// Central MNA data structure. Everything the engine needs at runtime.
//
// Mirrors Go:
//   type System struct {
//       G, C            *sparse.DOK
//       B_static        []float64
//       B_dynamic       []float64
//       NodeMap         map[string]int
//       BranchNameToIdx map[string]int
//       CurrentSources  map[string]*CurrentSourceRecord
//       Size, NumNodes  int
//       Dt              float64
//   }
//
// Key difference: G and C are dense Eigen matrices instead of sparse DOK.
// At our circuit sizes (≤256 nodes) dense stamping with operator+= is faster
// than a sparse intermediate — no conversion step before the LU solve.
// ─────────────────────────────────────────────────────────────────────────────

struct System {
    // ── Matrices ─────────────────────────────────────────────────────────────
    Eigen::MatrixXd G;          // static conductance matrix
    Eigen::MatrixXd C;          // dynamic matrix: capacitors and inductors (-L)

    // ── RHS vectors ──────────────────────────────────────────────────────────
    Eigen::VectorXd B_static;   // permanent forces (DC sources, driven values)
    Eigen::VectorXd B_dynamic;  // scratch RHS rebuilt every timestep solve

    // ── Dimensions ───────────────────────────────────────────────────────────
    int num_nodes;      // N   — KCL rows
    int num_branches;   // M   — branch current rows (V, L, E, H)
    int size;           // N+M — total matrix dimension

    // ── Node map ─────────────────────────────────────────────────────────────
    // net name → row index in G/C/B
    std::unordered_map<std::string, int> node_map;
    std::vector<std::string>             node_names;  // index → name (for debug)

    // ── Branch map ───────────────────────────────────────────────────────────
    // element name → branch row index (rows num_nodes … size-1)
    std::unordered_map<std::string, int> branch_map;

    // ── Current sources ──────────────────────────────────────────────────────
    std::unordered_map<std::string, CurrentSourceRecord> current_sources;

    // ── Timestep ─────────────────────────────────────────────────────────────
    double dt;

    // ─────────────────────────────────────────────────────────────────────────
    // CONSTRUCTOR
    // Allocates and zero-initialises all Eigen objects to (size x size).
    // ─────────────────────────────────────────────────────────────────────────
    // Default constructor — use system_init_empty() + system_finalize()
    // when node/branch counts are not known upfront (generated sim.cpp path).
    System()
        : num_nodes(0), num_branches(0), size(0), dt(1e-6)
    {}

    System(int num_nodes_, int num_branches_, double dt_)
        : num_nodes(num_nodes_)
        , num_branches(num_branches_)
        , size(num_nodes_ + num_branches_)
        , dt(dt_)
    {
        G        = Eigen::MatrixXd::Zero(size, size);
        C        = Eigen::MatrixXd::Zero(size, size);
        B_static = Eigen::VectorXd::Zero(size);
        B_dynamic= Eigen::VectorXd::Zero(size);
        node_names.reserve(num_nodes_);
    }

    // ─────────────────────────────────────────────────────────────────────────
    // NODE MAP HELPERS
    // ─────────────────────────────────────────────────────────────────────────

    // Register a node and return its index. Idempotent.
    // Ground ("gnd" / "0") always returns -1 and is never stored.
    int register_node(const std::string &name) {
        if (name == "gnd" || name == "0") return -1;
        auto it = node_map.find(name);
        if (it != node_map.end()) return it->second;
        int idx = (int)node_names.size();
        node_map[name] = idx;
        node_names.push_back(name);
        return idx;
    }

    // Resolve a node name to its index.
    // Returns -1 for ground, -2 if not found.
    int resolve_node(const std::string &name) const {
        if (name == "gnd" || name == "0") return -1;
        auto it = node_map.find(name);
        return (it != node_map.end()) ? it->second : -2;
    }

    // ─────────────────────────────────────────────────────────────────────────
    // BRANCH MAP HELPERS
    // ─────────────────────────────────────────────────────────────────────────

    // Register a branch element (V, L, E, H) and return its FINAL row index.
    //
    // IMPORTANT: branch_map stores a LOCAL 0-based index (0, 1, 2, ...) —
    // not the final matrix row. Node registration and branch registration
    // are interleaved during the netlist build (register_branch is often
    // called right after register_node for the same physical primitive),
    // so node_names.size() at registration time is NOT the final node count.
    // The actual row = num_nodes (final, set by system_finalize) + local index.
    //
    // This function returns a PROVISIONAL index for codegen's use at
    // registration time, but the only index that matters is what
    // resolve_branch() returns AFTER system_finalize() — codegen never
    // uses this return value directly (it calls resolve_branch separately).
    int register_branch(const std::string &name) {
        auto it = branch_map.find(name);
        if (it != branch_map.end()) return it->second; // already registered, local idx
        int local_idx = (int)branch_map.size();
        branch_map[name] = local_idx;
        return local_idx;
    }

    // Resolve a branch name to its FINAL row index (num_nodes + local index).
    // Only valid after system_finalize() has set num_nodes correctly.
    // Returns -1 if not found.
    int resolve_branch(const std::string &name) const {
        auto it = branch_map.find(name);
        if (it == branch_map.end()) return -1;
        return num_nodes + it->second;
    }

    // ─────────────────────────────────────────────────────────────────────────
    // CURRENT SOURCE HELPERS
    // ─────────────────────────────────────────────────────────────────────────

    void register_current_source(const std::string &name, int n1, int n2, double value) {
        current_sources[name] = CurrentSourceRecord{n1, n2, value};
    }

    CurrentSourceRecord *find_current_source(const std::string &name) {
        auto it = current_sources.find(name);
        return (it != current_sources.end()) ? &it->second : nullptr;
    }

    // ─────────────────────────────────────────────────────────────────────────
    // DEBUG
    // ─────────────────────────────────────────────────────────────────────────

    void print() const;
};

// Initialise a System without knowing node/branch counts upfront.
// Nodes and branches are registered dynamically via register_node/register_branch.
// Called by generated sim.cpp before build_netlist().
inline void system_init_empty(System *sys, double dt_) {
    *sys = System(0, 0, dt_);
    // Resize matrices lazily after netlist is built — call system_finalize() after build_netlist.
}

// Must be called after all register_node / register_branch calls,
// before any stamping or solving.
inline void system_finalize(System *sys) {
    int n = (int)sys->node_names.size();
    int b = (int)sys->branch_map.size();
    sys->num_nodes    = n;
    sys->num_branches = b;
    sys->size         = n + b;
    sys->G        = Eigen::MatrixXd::Zero(sys->size, sys->size);
    sys->C        = Eigen::MatrixXd::Zero(sys->size, sys->size);
    sys->B_static = Eigen::VectorXd::Zero(sys->size);
    sys->B_dynamic= Eigen::VectorXd::Zero(sys->size);
}