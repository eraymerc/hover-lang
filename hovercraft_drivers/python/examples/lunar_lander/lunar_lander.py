"""LUNAR LANDER — a game whose physics engine is a circuit simulator.

    python3 hovercraft_drivers/python/examples/lunar_lander/lunar_lander.py

Everything that moves is computed by lander.hvr, compiled to a hovercraft
library and stepped in real time. This file owns no physics at all: it reads
the keyboard, writes two () inputs, advances the simulation by one frame's
worth of simulation seconds, and draws whatever came back.

That division is the point. The trajectory is the solution of an MNA system
with a real integrator and a real timestep, not a += in a game loop.

Controls
    Up / W          throttle up (hold)
    Left / Right    gimbal the engine (self-centres when released)
    Space           emergency cutoff
    L               dump the flight recorder to flight_log.csv
    R               reset and try again
    Q / Esc         quit

Needs only the standard library (tkinter).
"""

import math
import sys
import time
import tkinter as tk
from pathlib import Path

# Allow running straight from a checkout without installing.
sys.path.insert(0, str(Path(__file__).resolve().parents[2]))

from hovercraft import Hovercraft  # noqa: E402

HERE = Path(__file__).resolve().parent

# ── Mission parameters ───────────────────────────────────────────────────────
# These must agree with lander.hvr; the simulation is the authority on the
# ones it also knows (FUEL_0), and this file only uses them to draw.
FUEL_0 = 240.0
PAD_X = 300.0          # metres downrange
PAD_HALF = 25.0        # half-width of the landing pad

# What counts as a landing rather than an expensive hole in the regolith.
MAX_TOUCHDOWN_VY = 2.5     # m/s descent
MAX_TOUCHDOWN_VX = 1.5     # m/s lateral
MAX_TOUCHDOWN_TILT = 0.12  # rad from vertical

# ── Controls ─────────────────────────────────────────────────────────────────
THROTTLE_RATE_UP = 2.0     # per second, holding thrust
THROTTLE_RATE_DOWN = 3.0   # per second, released
GIMBAL_MAX = 0.55          # rad
GIMBAL_RATE = 1.6          # rad/s
GIMBAL_CENTRE_RATE = 2.2   # rad/s, self-centring

# ── Presentation ─────────────────────────────────────────────────────────────
W, H = 1000, 720
HUD_W = 250
VIEW_W = W - HUD_W
FPS = 50
FRAME_MS = int(1000 / FPS)
DT = 1.0 / FPS             # simulation seconds advanced per frame

SKY = "#05060d"
REGOLITH = "#5a5347"
REGOLITH_LIT = "#7b7261"
PAD_COLOUR = "#2f9e8f"
INK = "#c8d6e5"
DIM = "#5c6b7a"
WARN = "#e8703a"
GOOD = "#6bd490"

STARS = [
    # (x, y, radius) — fixed so the sky doesn't shimmer between frames.
    (int(VIEW_W * fx), int(H * fy), r)
    for fx, fy, r in [
        (0.05, 0.08, 1), (0.13, 0.31, 1), (0.19, 0.12, 2), (0.27, 0.44, 1),
        (0.33, 0.07, 1), (0.41, 0.26, 1), (0.47, 0.55, 2), (0.52, 0.15, 1),
        (0.58, 0.38, 1), (0.63, 0.09, 1), (0.71, 0.29, 2), (0.78, 0.48, 1),
        (0.83, 0.13, 1), (0.89, 0.35, 1), (0.94, 0.21, 1), (0.97, 0.52, 2),
        (0.09, 0.58, 1), (0.24, 0.63, 1), (0.36, 0.71, 1), (0.68, 0.66, 1),
    ]
]

# Cosmetic surface detail, in world metres. The simulated surface is flat --
# altitude in lander.hvr is measured from a single datum -- so these are
# craters and boulders drawn *on* the plane, never hills that would imply a
# terrain height the physics doesn't model.
CRATERS = [(-160.0, 26.0), (-40.0, 14.0), (95.0, 34.0), (180.0, 11.0),
           (430.0, 22.0), (520.0, 40.0), (660.0, 17.0), (780.0, 29.0)]
BOULDERS = [(-95.0, 3.0), (55.0, 2.0), (240.0, 2.5), (400.0, 4.0),
            (600.0, 2.0), (720.0, 3.5)]


class Lander:
    def __init__(self, root):
        self.root = root
        root.title("Lunar Lander — powered by Hover")
        root.resizable(False, False)

        self.canvas = tk.Canvas(root, width=W, height=H, bg=SKY,
                                highlightthickness=0)
        self.canvas.pack()

        print("compiling lander.hvr ...")
        self.hc = Hovercraft(HERE / "lander.hvr")
        print(self.hc.describe())

        # Held keys. X11 auto-repeat synthesises a release immediately before
        # each repeat press, so a naive held-key set flickers off every repeat
        # interval. Releases are therefore deferred by one repeat period and
        # cancelled if the matching press arrives first.
        self.held = set()
        self._release_jobs = {}
        root.bind("<KeyPress>", self.on_press)
        root.bind("<KeyRelease>", self.on_release)

        self.reset()
        self.last_wall = time.perf_counter()
        self.root.after(FRAME_MS, self.tick)

    # ── state ────────────────────────────────────────────────────────────

    def reset(self):
        self.hc.reset()
        self.hc.reset_log()
        self.throttle = 0.0
        self.gimbal = 0.0
        self.hc.inputs.throttle = 0.0
        self.hc.inputs.gimbal = 0.0
        self.outcome = None        # None | "landed" | "crashed"
        self.outcome_lines = []
        self.touchdown = None      # frozen telemetry at contact
        self.message = ""
        self.message_until = 0.0

    def notify(self, text):
        self.message = text
        self.message_until = time.perf_counter() + 2.5

    # ── input ────────────────────────────────────────────────────────────

    def on_press(self, event):
        key = event.keysym.lower()
        job = self._release_jobs.pop(key, None)
        if job is not None:
            self.root.after_cancel(job)
        self.held.add(key)

        if key in ("q", "escape"):
            self.root.destroy()
        elif key == "r":
            self.reset()
        elif key == "space":
            self.throttle = 0.0
        elif key == "l":
            self.dump_log()

    def on_release(self, event):
        key = event.keysym.lower()
        job = self._release_jobs.pop(key, None)
        if job is not None:
            self.root.after_cancel(job)
        self._release_jobs[key] = self.root.after(
            40, lambda k=key: (self.held.discard(k),
                               self._release_jobs.pop(k, None)))

    def dump_log(self):
        path = HERE / "flight_log.csv"
        self.hc.save_csv(path)
        rows = len(self.hc.log())
        self.notify("wrote {} rows -> {}".format(rows, path.name))
        print("flight recorder: {} rows -> {}".format(rows, path))

    # ── loop ─────────────────────────────────────────────────────────────

    def tick(self):
        now = time.perf_counter()
        self.last_wall = now

        if self.outcome is None:
            self.apply_controls()
            self.hc.run(DT)
            self.check_contact()

        self.draw()
        self.root.after(FRAME_MS, self.tick)

    def apply_controls(self):
        thrusting = bool(self.held & {"up", "w"})
        if thrusting:
            self.throttle = min(1.0, self.throttle + THROTTLE_RATE_UP * DT)
        else:
            self.throttle = max(0.0, self.throttle - THROTTLE_RATE_DOWN * DT)

        # Sign follows the simulation: lander.hvr computes
        # ax = (thrust/mass) * sin(tilt), so a POSITIVE tilt accelerates in
        # +x, which is downrange-right. Right key -> positive tilt.
        left = bool(self.held & {"left", "a"})
        right = bool(self.held & {"right", "d"})
        if right and not left:
            self.gimbal = min(GIMBAL_MAX, self.gimbal + GIMBAL_RATE * DT)
        elif left and not right:
            self.gimbal = max(-GIMBAL_MAX, self.gimbal - GIMBAL_RATE * DT)
        else:
            # Self-centring, and never past centre in one step.
            if self.gimbal > 0:
                self.gimbal = max(0.0, self.gimbal - GIMBAL_CENTRE_RATE * DT)
            else:
                self.gimbal = min(0.0, self.gimbal + GIMBAL_CENTRE_RATE * DT)

        # The only two things this program tells the simulation.
        self.hc.inputs.throttle = self.throttle
        self.hc.inputs.gimbal = self.gimbal

    def telemetry(self):
        if self.touchdown is not None:
            return self.touchdown
        o = self.hc.outputs
        return {
            "alt": o.main_alt, "x": o.main_downrange,
            "vy": o.main_vy, "vx": o.main_vx,
            "fuel": o.main_fuel, "mass": o.main_mass,
            "thrust": o.main_thrust, "flow": o.main_flow,
            "tilt": o.main_tilt, "t": self.hc.time,
        }

    def check_contact(self):
        tm = self.telemetry()
        if tm["alt"] > 0.0:
            return

        self.touchdown = tm
        on_pad = abs(tm["x"] - PAD_X) <= PAD_HALF
        faults = []
        if not on_pad:
            faults.append("missed the pad by {:.0f} m".format(
                abs(tm["x"] - PAD_X) - PAD_HALF))
        if abs(tm["vy"]) > MAX_TOUCHDOWN_VY:
            faults.append("descent {:.1f} m/s (limit {:.1f})".format(
                abs(tm["vy"]), MAX_TOUCHDOWN_VY))
        if abs(tm["vx"]) > MAX_TOUCHDOWN_VX:
            faults.append("drift {:.1f} m/s (limit {:.1f})".format(
                abs(tm["vx"]), MAX_TOUCHDOWN_VX))
        if abs(tm["tilt"]) > MAX_TOUCHDOWN_TILT:
            faults.append("tilted {:.0f} deg (limit {:.0f})".format(
                abs(math.degrees(tm["tilt"])),
                math.degrees(MAX_TOUCHDOWN_TILT)))

        if faults:
            self.outcome = "crashed"
            self.outcome_lines = faults
        else:
            self.outcome = "landed"
            self.outcome_lines = [
                "touchdown at {:.2f} m/s, {:.0f} m from pad centre".format(
                    abs(tm["vy"]), abs(tm["x"] - PAD_X)),
                "{:.1f} kg of propellant remaining".format(max(0.0, tm["fuel"])),
            ]

    # ── view transform ───────────────────────────────────────────────────

    def camera(self, tm):
        """Return (metres_per_pixel, ground_y, camera_x).

        The view zooms in as the lander descends, so the last hundred metres
        -- where the game is actually decided -- aren't a two-pixel crawl.
        """
        span = min(1250.0, max(70.0, tm["alt"] * 1.7 + 45.0))
        ppm = (H * 0.72) / span
        ground_y = H - 90
        return ppm, ground_y, tm["x"]

    def sx(self, world_x, cam_x, ppm):
        return VIEW_W * 0.5 + (world_x - cam_x) * ppm

    # ── drawing ──────────────────────────────────────────────────────────

    def draw(self):
        c = self.canvas
        c.delete("all")
        tm = self.telemetry()
        ppm, ground_y, cam_x = self.camera(tm)

        # Explicit sky rather than relying on the canvas background: the
        # background isn't a canvas item, so it is absent from a PostScript
        # export and the whole scene comes out on white.
        c.create_rectangle(0, 0, VIEW_W, H, fill=SKY, outline="")
        for x, y, r in STARS:
            c.create_oval(x - r, y - r, x + r, y + r, fill="#8fa3bb",
                          outline="")

        self.draw_surface(tm, ppm, ground_y, cam_x)
        self.draw_lander(tm, ppm, ground_y, cam_x)
        self.draw_hud(tm)
        self.draw_banner()

        # Bound the flight recorder: a long hover would otherwise grow the
        # log without limit. Thirty seconds of history is plenty to dump.
        if tm["t"] > 30.0:
            self.hc.clear_log_before(tm["t"] - 30.0)

    def draw_surface(self, tm, ppm, ground_y, cam_x):
        c = self.canvas
        c.create_rectangle(0, ground_y, VIEW_W, H, fill=REGOLITH, outline="")
        c.create_line(0, ground_y, VIEW_W, ground_y, fill=REGOLITH_LIT,
                      width=2)

        for wx, r in CRATERS:
            px = self.sx(wx, cam_x, ppm)
            rp = r * ppm
            if px + rp < 0 or px - rp > VIEW_W or rp < 2:
                continue
            c.create_arc(px - rp, ground_y - rp * 0.35, px + rp,
                         ground_y + rp * 0.35, start=180, extent=180,
                         style=tk.ARC, outline=REGOLITH_LIT, width=1)

        for wx, r in BOULDERS:
            px = self.sx(wx, cam_x, ppm)
            rp = max(1.5, r * ppm)
            if px + rp < 0 or px - rp > VIEW_W:
                continue
            c.create_oval(px - rp, ground_y - rp, px + rp, ground_y,
                          fill=REGOLITH_LIT, outline="")

        # The pad.
        left = self.sx(PAD_X - PAD_HALF, cam_x, ppm)
        right = self.sx(PAD_X + PAD_HALF, cam_x, ppm)
        c.create_rectangle(left, ground_y - 3, right, ground_y + 3,
                           fill=PAD_COLOUR, outline="")
        for px in (left, right):
            c.create_line(px, ground_y - 3, px, ground_y - 18,
                          fill=PAD_COLOUR, width=2)
            c.create_oval(px - 3, ground_y - 22, px + 3, ground_y - 16,
                          fill=GOOD, outline="")

        # An off-screen pad still has to be findable, or the first flight is
        # a guessing game.
        if right < 0 or left > VIEW_W:
            arrow_x = 24 if right < 0 else VIEW_W - 24
            direction = -1 if right < 0 else 1
            c.create_text(arrow_x, ground_y - 46, fill=PAD_COLOUR,
                          font=("TkFixedFont", 11, "bold"),
                          text="PAD\n{:.0f} m".format(abs(tm["x"] - PAD_X)))
            c.create_polygon(arrow_x + 14 * direction, ground_y - 26,
                             arrow_x - 6 * direction, ground_y - 34,
                             arrow_x - 6 * direction, ground_y - 18,
                             fill=PAD_COLOUR, outline="")

    def draw_lander(self, tm, ppm, ground_y, cam_x):
        c = self.canvas
        px = self.sx(tm["x"], cam_x, ppm)
        py = ground_y - max(0.0, tm["alt"]) * ppm

        # Drawn at a fixed screen size: a to-scale 4 m vehicle would be
        # invisible at 1000 m and absurd at 5 m.
        s = 13.0
        tilt = tm["tilt"]
        cos_t, sin_t = math.cos(tilt), math.sin(tilt)

        def pt(bx, by):
            # Body frame: +by is up along the thrust axis, +bx is right.
            # Positive tilt leans the nose RIGHT, so the thrust vector gains a
            # +x component -- the same convention lander.hvr integrates as
            # ax = (thrust/mass) * sin(tilt). Getting this backwards makes the
            # rocket lean away from the direction it accelerates.
            return (px + (bx * cos_t + by * sin_t) * s,
                    py - (by * cos_t - bx * sin_t) * s)

        if self.outcome == "crashed":
            c.create_text(px, py - 26, text="✹", fill=WARN,
                          font=("TkDefaultFont", 30, "bold"))
            c.create_line(px - 22, ground_y, px + 22, ground_y, fill=WARN,
                          width=3)
            return

        # Exhaust plume, length modulated by the *simulated* thrust rather
        # than by the key being held -- an empty tank shows no flame.
        duty = tm["thrust"] / 3600.0
        if duty > 0.01:
            flare = 1.0 + 2.6 * duty
            c.create_polygon(*pt(-0.45, -0.9), *pt(0.45, -0.9),
                             *pt(0.0, -0.9 - flare),
                             fill="#ffb347", outline="")
            c.create_polygon(*pt(-0.22, -0.9), *pt(0.22, -0.9),
                             *pt(0.0, -0.9 - flare * 0.6),
                             fill="#fff2b2", outline="")

        # Legs, then body, then the crew module.
        for side in (-1, 1):
            c.create_line(*pt(0.55 * side, -0.45), *pt(1.15 * side, -1.15),
                          fill=DIM, width=2)
            c.create_line(*pt(0.85 * side, -1.15), *pt(1.45 * side, -1.15),
                          fill=DIM, width=2)
        c.create_polygon(*pt(-0.85, -0.9), *pt(0.85, -0.9), *pt(0.7, 0.35),
                         *pt(-0.7, 0.35), fill="#8b93a5", outline=INK)
        c.create_polygon(*pt(-0.7, 0.35), *pt(0.7, 0.35), *pt(0.0, 1.35),
                         fill="#b9c2d0", outline=INK)
        c.create_oval(*pt(-0.28, 0.62), *pt(0.28, 0.9), fill="#243447",
                      outline=INK)

    def draw_hud(self, tm):
        c = self.canvas
        x0 = VIEW_W
        c.create_rectangle(x0, 0, W, H, fill="#0b0f18", outline="")
        c.create_line(x0, 0, x0, H, fill="#1d2735", width=2)

        pad_x = x0 + 20
        y = 24

        def row(label, value, colour=INK, gap=22):
            nonlocal y
            c.create_text(pad_x, y, anchor="w", text=label, fill=DIM,
                          font=("TkFixedFont", 9))
            c.create_text(W - 20, y, anchor="e", text=value, fill=colour,
                          font=("TkFixedFont", 11, "bold"))
            y += gap

        def heading(text):
            nonlocal y
            y += 6
            c.create_text(pad_x, y, anchor="w", text=text, fill=PAD_COLOUR,
                          font=("TkFixedFont", 9, "bold"))
            y += 18

        c.create_text(pad_x, y, anchor="w", text="LUNAR LANDER", fill=INK,
                      font=("TkFixedFont", 13, "bold"))
        y += 16
        c.create_text(pad_x, y, anchor="w", text="physics by hover",
                      fill=DIM, font=("TkFixedFont", 8))
        y += 22

        heading("FLIGHT")
        alt = max(0.0, tm["alt"])
        row("ALTITUDE", "{:7.1f} m".format(alt),
            WARN if alt < 60 else INK)
        vy_ok = abs(tm["vy"]) <= MAX_TOUCHDOWN_VY
        row("VERT SPEED", "{:+7.2f} m/s".format(tm["vy"]),
            GOOD if vy_ok else (WARN if alt < 150 else INK))
        vx_ok = abs(tm["vx"]) <= MAX_TOUCHDOWN_VX
        row("HORZ SPEED", "{:+7.2f} m/s".format(tm["vx"]),
            GOOD if vx_ok else (WARN if alt < 150 else INK))
        offset = tm["x"] - PAD_X
        row("PAD OFFSET", "{:+7.1f} m".format(offset),
            GOOD if abs(offset) <= PAD_HALF else INK)
        row("TILT", "{:+7.1f} deg".format(math.degrees(tm["tilt"])))

        heading("PROPULSION")
        row("THROTTLE", "{:6.0f} %".format(self.throttle * 100))
        row("THRUST", "{:7.0f} N".format(tm["thrust"]))
        row("FLOW", "{:7.2f} kg/s".format(tm["flow"]))
        row("MASS", "{:7.1f} kg".format(tm["mass"]))

        fuel = max(0.0, tm["fuel"])
        frac = fuel / FUEL_0
        row("PROPELLANT", "{:7.1f} kg".format(fuel),
            WARN if frac < 0.2 else INK, gap=12)
        bar_l, bar_r = pad_x, W - 20
        c.create_rectangle(bar_l, y, bar_r, y + 12, outline="#1d2735")
        if frac > 0:
            c.create_rectangle(bar_l + 1, y + 1,
                               bar_l + 1 + (bar_r - bar_l - 2) * frac,
                               y + 11,
                               fill=WARN if frac < 0.2 else PAD_COLOUR,
                               outline="")
        y += 30

        heading("MISSION")
        row("ELAPSED", "{:7.2f} s".format(tm["t"]))
        row("SIM STEP", "{:7.0f} ms".format((self.hc.time_step or 0) * 1e3))

        # Touchdown limits, so the player isn't guessing at the rules.
        # anchor="nw", not "w": a multi-line block anchored west is centred
        # on y and grows upward into the row above it.
        y += 12
        c.create_text(pad_x, y, anchor="nw", fill=DIM, justify="left",
                      font=("TkFixedFont", 8),
                      text="touchdown limits\n"
                           "  vert  < {:.1f} m/s\n"
                           "  horz  < {:.1f} m/s\n"
                           "  tilt  < {:.0f} deg\n"
                           "  on the pad".format(
                               MAX_TOUCHDOWN_VY, MAX_TOUCHDOWN_VX,
                               math.degrees(MAX_TOUCHDOWN_TILT)))

        c.create_text(pad_x, H - 92, anchor="nw", fill=DIM, justify="left",
                      font=("TkFixedFont", 8),
                      text="UP/W    thrust\n"
                           "LEFT/RIGHT  gimbal\n"
                           "SPACE   cut engine\n"
                           "L  log CSV    R  reset    Q  quit")

        if self.message and time.perf_counter() < self.message_until:
            c.create_text(VIEW_W // 2, H - 24, text=self.message, fill=GOOD,
                          font=("TkFixedFont", 10))

    def draw_banner(self):
        if self.outcome is None:
            return
        c = self.canvas
        landed = self.outcome == "landed"
        title = "THE EAGLE HAS LANDED" if landed else "MISSION LOST"
        colour = GOOD if landed else WARN

        cx, cy = VIEW_W // 2, H // 2 - 40
        c.create_rectangle(cx - 260, cy - 60, cx + 260,
                           cy + 42 + 16 * len(self.outcome_lines),
                           fill="#0b0f18", outline=colour, width=2)
        c.create_text(cx, cy - 30, text=title, fill=colour,
                      font=("TkFixedFont", 18, "bold"))
        yy = cy + 2
        for line in self.outcome_lines:
            c.create_text(cx, yy, text=line, fill=INK,
                          font=("TkFixedFont", 10))
            yy += 16
        c.create_text(cx, yy + 14, text="R to fly again    L to save the log",
                      fill=DIM, font=("TkFixedFont", 9))


def main():
    root = tk.Tk()
    Lander(root)
    root.mainloop()


if __name__ == "__main__":
    main()
