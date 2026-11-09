#include "state_slots.hpp"

void slots_resize(StateSlots &st, int n) {
    for (auto *v : {&st.s_now, &st.d_now, &st.h0, &st.h1, &st.h2, &st.d_prev, &st.blend})
        v->assign(n, 0.0);
    st.is_idt.assign(n, 0);
}

void slots_reset(StateSlots &st) {
    slots_resize(st, st.size());
    st.alpha = st.inv_alpha = st.beta = 0.0;
}

void slots_begin(StateSlots &st, double alpha, const double w[3], double beta) {
    st.alpha     = alpha;
    st.inv_alpha = (alpha != 0.0) ? 1.0 / alpha : 0.0;
    st.beta      = beta;
    for (int k = 0; k < st.size(); k++)
        st.blend[k] = w[0] * st.h0[k] + w[1] * st.h1[k] + w[2] * st.h2[k];
}

void slots_begin_be(StateSlots &st, double alpha) {
    static const double w[3] = {1.0, 0.0, 0.0};
    slots_begin(st, alpha, w, 0.0);
}

void slots_begin_zero(StateSlots &st, double alpha) {
    static const double w[3] = {0.0, 0.0, 0.0};
    slots_begin(st, alpha, w, 0.0);
}

double slots_ddt(StateSlots &st, int k, double q) {
    const double d = st.alpha * (q - st.blend[k]) - st.beta * st.d_prev[k];
    st.s_now[k] = q;
    st.d_now[k] = d;
    return d;
}

double slots_idt(StateSlots &st, int k, double x) {
    const double s = st.blend[k] + (x + st.beta * st.d_prev[k]) * st.inv_alpha;
    st.s_now[k] = s;
    st.d_now[k] = x;
    st.is_idt[k] = 1;
    return s;
}

void slots_commit(StateSlots &st) {
    st.h2.swap(st.h1);        // h2 <- h1
    st.h1.swap(st.h0);        // h1 <- h0
    st.h0 = st.s_now;         // h0 <- s_now
    st.d_prev = st.d_now;
}

void slots_anchor(StateSlots &st) {
    st.h0 = st.h1 = st.h2 = st.s_now;
    st.d_prev = st.d_now;
}
