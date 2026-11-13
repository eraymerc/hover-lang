// ─────────────────────────────────────────────────────────────────────────────
// hvr::Dual — the arithmetic and <math.h> overloads, compiled once with the
// runtime (declarations and the rationale: hover_sim.hpp).
//
// They used to be inline in the header, and that made every generated
// sim.cpp with analog code take 11-13 s to compile at -O1 or above instead of
// 0.7 s: the optimizer inlined every Dual operation — each a merge loop over
// the sparse gradient — into phase_jacobian, one function holding the whole
// deck's analog code, and its time grows faster than linearly with function
// size. Out of line they cost a call per operation at run time.
// ─────────────────────────────────────────────────────────────────────────────

#include "../hover_sim.hpp"

#include <cmath>

namespace hvr {

bool &dual_overflow() { static bool f = false; return f; }

namespace detail {
// r.grad = ca*a.grad + cb*b.grad (merge of two sorted lists).
inline void lin(Dual &r, double ca, const Dual &a, double cb, const Dual &b) {
    int p = 0, q = 0, m = 0;
    while (p < a.n || q < b.n) {
        int idx; double val;
        if (q >= b.n || (p < a.n && a.i[p] < b.i[q])) { idx = a.i[p]; val = ca * a.d[p]; p++; }
        else if (p >= a.n || b.i[q] < a.i[p])          { idx = b.i[q]; val = cb * b.d[q]; q++; }
        else { idx = a.i[p]; val = ca * a.d[p] + cb * b.d[q]; p++; q++; }
        if (m < Dual::K) { r.i[m] = idx; r.d[m] = val; m++; }
        else dual_overflow() = true;
    }
    r.n = m;
}
// r.grad = c*a.grad
inline void scale(Dual &r, double c, const Dual &a) {
    r.n = a.n;
    for (int k = 0; k < a.n; k++) { r.i[k] = a.i[k]; r.d[k] = c * a.d[k]; }
}
// f(a) with value fv and derivative df
inline Dual unary(const Dual &a, double fv, double df) {
    Dual r(fv);
    if (a.n) scale(r, df, a);
    return r;
}
// f(a, b) with value fv and partial derivatives fa, fb
inline Dual binary(const Dual &a, const Dual &b, double fv, double fa, double fb) {
    Dual r(fv);
    if (a.n == 0)      { if (b.n) scale(r, fb, b); }
    else if (b.n == 0) { scale(r, fa, a); }
    else               { lin(r, fa, a, fb, b); }
    return r;
}
}  // namespace detail


// ── Arithmetic ───────────────────────────────────────────────────────────────
Dual operator+(const Dual &a) { return a; }
Dual operator-(const Dual &a) { Dual r(-a.v); detail::scale(r, -1.0, a); return r; }

Dual operator+(const Dual &a, const Dual &b) { return detail::binary(a, b, a.v + b.v, 1.0, 1.0); }
Dual operator+(const Dual &a, double b) { Dual r = a; r.v = a.v + b; return r; }
Dual operator+(double a, const Dual &b) { Dual r = b; r.v = a + b.v; return r; }

Dual operator-(const Dual &a, const Dual &b) { return detail::binary(a, b, a.v - b.v, 1.0, -1.0); }
Dual operator-(const Dual &a, double b) { Dual r = a; r.v = a.v - b; return r; }
Dual operator-(double a, const Dual &b) { Dual r(a - b.v); detail::scale(r, -1.0, b); return r; }

Dual operator*(const Dual &a, const Dual &b) { return detail::binary(a, b, a.v * b.v, b.v, a.v); }
Dual operator*(const Dual &a, double b) { Dual r(a.v * b); detail::scale(r, b, a); return r; }
Dual operator*(double a, const Dual &b) { Dual r(a * b.v); detail::scale(r, a, b); return r; }

Dual operator/(const Dual &a, const Dual &b) {
    return detail::binary(a, b, a.v / b.v, 1.0 / b.v, -a.v / (b.v * b.v));
}
Dual operator/(const Dual &a, double b) { Dual r(a.v / b); detail::scale(r, 1.0 / b, a); return r; }
Dual operator/(double a, const Dual &b) { Dual r(a / b.v); detail::scale(r, -a / (b.v * b.v), b); return r; }

Dual &operator+=(Dual &a, const Dual &b) { a = a + b; return a; }
Dual &operator-=(Dual &a, const Dual &b) { a = a - b; return a; }
Dual &operator*=(Dual &a, const Dual &b) { a = a * b; return a; }
Dual &operator/=(Dual &a, const Dual &b) { a = a / b; return a; }



// ── <math.h> ─────────────────────────────────────────────────────────────────
// One overload per function, on Duals only: a call with a double argument in
// generated AD code converts that argument implicitly (its gradient is
// empty), and a call with only doubles still resolves to <math.h>. The set
// here is exactly the one codegen/autodiff.go accepts; any other extern in an
// analog block makes the compiler leave the AD Jacobian out.
Dual exp(const Dual &a) { const double e = std::exp(a.v); return detail::unary(a, e, e); }
Dual exp2(const Dual &a) { const double e = std::exp2(a.v); return detail::unary(a, e, e * 0.69314718055994531); }
Dual expm1(const Dual &a) { return detail::unary(a, std::expm1(a.v), std::exp(a.v)); }
Dual log(const Dual &a) { return detail::unary(a, std::log(a.v), 1.0 / a.v); }
Dual log10(const Dual &a) { return detail::unary(a, std::log10(a.v), 1.0 / (a.v * 2.3025850929940457)); }
Dual log2(const Dual &a) { return detail::unary(a, std::log2(a.v), 1.0 / (a.v * 0.69314718055994531)); }
Dual nodiff(const Dual &a) { return Dual(a.v); }
Dual log1p(const Dual &a) { return detail::unary(a, std::log1p(a.v), 1.0 / (1.0 + a.v)); }
Dual sqrt(const Dual &a) { const double s = std::sqrt(a.v); return detail::unary(a, s, 0.5 / s); }
Dual cbrt(const Dual &a) { const double c = std::cbrt(a.v); return detail::unary(a, c, 1.0 / (3.0 * c * c)); }
Dual fabs(const Dual &a) { return detail::unary(a, std::fabs(a.v), a.v > 0.0 ? 1.0 : (a.v < 0.0 ? -1.0 : 0.0)); }

Dual sin(const Dual &a) { return detail::unary(a, std::sin(a.v), std::cos(a.v)); }
Dual cos(const Dual &a) { return detail::unary(a, std::cos(a.v), -std::sin(a.v)); }
Dual tan(const Dual &a) { const double c = std::cos(a.v); return detail::unary(a, std::tan(a.v), 1.0 / (c * c)); }
Dual asin(const Dual &a) { return detail::unary(a, std::asin(a.v), 1.0 / std::sqrt(1.0 - a.v * a.v)); }
Dual acos(const Dual &a) { return detail::unary(a, std::acos(a.v), -1.0 / std::sqrt(1.0 - a.v * a.v)); }
Dual atan(const Dual &a) { return detail::unary(a, std::atan(a.v), 1.0 / (1.0 + a.v * a.v)); }
Dual sinh(const Dual &a) { return detail::unary(a, std::sinh(a.v), std::cosh(a.v)); }
Dual cosh(const Dual &a) { return detail::unary(a, std::cosh(a.v), std::sinh(a.v)); }
Dual tanh(const Dual &a) { const double t = std::tanh(a.v); return detail::unary(a, t, 1.0 - t * t); }
Dual asinh(const Dual &a) { return detail::unary(a, std::asinh(a.v), 1.0 / std::sqrt(a.v * a.v + 1.0)); }
Dual acosh(const Dual &a) { return detail::unary(a, std::acosh(a.v), 1.0 / std::sqrt(a.v * a.v - 1.0)); }
Dual atanh(const Dual &a) { return detail::unary(a, std::atanh(a.v), 1.0 / (1.0 - a.v * a.v)); }
Dual erf(const Dual &a) { return detail::unary(a, std::erf(a.v), 1.1283791670955126 * std::exp(-a.v * a.v)); }
Dual erfc(const Dual &a) { return detail::unary(a, std::erfc(a.v), -1.1283791670955126 * std::exp(-a.v * a.v)); }

// Piecewise constant: the value, no gradient.
Dual floor(const Dual &a) { return Dual(std::floor(a.v)); }
Dual ceil(const Dual &a) { return Dual(std::ceil(a.v)); }
Dual trunc(const Dual &a) { return Dual(std::trunc(a.v)); }
Dual round(const Dual &a) { return Dual(std::round(a.v)); }
Dual nearbyint(const Dual &a) { return Dual(std::nearbyint(a.v)); }
Dual rint(const Dual &a) { return Dual(std::rint(a.v)); }
long lround(const Dual &a) { return std::lround(a.v); }
long lrint(const Dual &a) { return std::lrint(a.v); }
Dual ldexp(const Dual &a, int e) { return detail::unary(a, std::ldexp(a.v, e), std::ldexp(1.0, e)); }

Dual pow(const Dual &a, const Dual &b) {
    const double p = std::pow(a.v, b.v);
    // The log term only when the exponent actually varies: a constant
    // exponent on a non-positive base is common (a junction's (1 - v/Vj)^(1-M))
    // and log() of it would poison the derivative with NaN for nothing.
    const double fa = (a.n && b.v != 0.0) ? b.v * std::pow(a.v, b.v - 1.0) : 0.0;
    const double fb = b.n ? p * std::log(a.v) : 0.0;
    return detail::binary(a, b, p, fa, fb);
}
Dual atan2(const Dual &y, const Dual &x) {
    const double r2 = x.v * x.v + y.v * y.v;
    return detail::binary(y, x, std::atan2(y.v, x.v), x.v / r2, -y.v / r2);
}
Dual hypot(const Dual &a, const Dual &b) {
    const double h = std::hypot(a.v, b.v);
    return detail::binary(a, b, h, a.v / h, b.v / h);
}
Dual fmod(const Dual &a, const Dual &b) {
    return detail::binary(a, b, std::fmod(a.v, b.v), 1.0, -std::trunc(a.v / b.v));
}
Dual remainder(const Dual &a, const Dual &b) {
    return detail::binary(a, b, std::remainder(a.v, b.v), 1.0, -std::nearbyint(a.v / b.v));
}
Dual copysign(const Dual &a, const Dual &b) {
    const double s = (std::signbit(a.v) == std::signbit(b.v)) ? 1.0 : -1.0;
    return detail::unary(a, std::copysign(a.v, b.v), s);
}
Dual fdim(const Dual &a, const Dual &b) { return a.v > b.v ? a - b : Dual(std::fdim(a.v, b.v)); }
Dual fmax(const Dual &a, const Dual &b) { return (b.v > a.v) ? b : a; }
Dual fmin(const Dual &a, const Dual &b) { return (b.v < a.v) ? b : a; }
Dual fma(const Dual &a, const Dual &b, const Dual &c) {
    Dual r = a * b + c;
    r.v = std::fma(a.v, b.v, c.v);
    return r;
}


}  // namespace hvr
