# Lunar Lander

A playable lunar lander whose flight dynamics are solved by Hover.

```
python3 hovercraft_drivers/python/examples/lunar_lander/lunar_lander.py
```

Needs only the Python standard library (tkinter). `lander.hvr` is compiled on
first run and rebuilt whenever it changes.

## Controls

| Key | |
|---|---|
| `Up` / `W` | throttle up (hold; releases bleed off) |
| `Left` / `Right` | gimbal the engine — self-centres when released |
| `Space` | emergency cutoff |
| `L` | dump the flight recorder to `flight_log.csv` |
| `R` | reset and fly again |
| `Q` / `Esc` | quit |

You start at 1000 m, descending at 18 m/s, drifting downrange at 12 m/s, with
240 kg of propellant. The pad is 300 m downrange. To land rather than crash:
be on the pad, under 2.5 m/s vertical, under 1.5 m/s lateral, within 7° of
vertical.

## What is actually doing the work

`lunar_lander.py` contains no physics. Every frame it writes two numbers,
advances the simulation, and reads the answers back:

```python
hc.inputs.throttle = self.throttle   # () logic args, settable any time
hc.inputs.gimbal   = self.gimbal
hc.run(DT)                           # advance DT simulation seconds
alt = hc.outputs.main_alt            # .save()d columns, read live
```

Everything else lives in `lander.hvr`, and leans on two Hover ideas.

**Integration is a circuit.** `idt(x)` is rewritten at elaboration time into
a 1 F capacitor driven by a current source carrying `x`, so the node voltage
*is* the integral. Acceleration → velocity → position is two of those in
series, and the timestep is chosen by the solver rather than by the game
loop:

```
double vy  = VY_0  + idt(ay);
double alt = ALT_0 + idt(vy);
```

`idt()` integrates from zero, which is why the initial conditions appear as
plain constants of integration rather than as capacitor pre-charge.

**The fuel tank is a capacitor.** Charge a 1 F cap with the mass flow rate
and its voltage is, literally, kilograms burned. Because the tank is a real
branch in the MNA matrix, the flow can be read back out with `I()` through
the usual 0 V ammeter:

```
current_source<>(flow_cmd) [gnd, tank];
voltage_source vflow<0>()  [tank, burned];   // ammeter
C<1>()                     [burned, gnd];    // the tank

double flow = I(vflow);                      // kg/s
```

Burning propellant lightens the vehicle, so thrust-to-weight climbs through
the descent — the mass in `ay = thrust/mass - g` is the simulated mass, not
a constant.

## Why a library rather than a binary

A one-shot `hover lander.hvr` run would be over before you touched a key: the
whole point of `--hovercraft` is that the simulation can be advanced a frame
at a time with fresh inputs each frame. The game also uses:

- `hc.reset()` — restart without recompiling or reloading
- `hc.clear_log_before(t)` — bound the flight recorder during a long hover
- `hc.save_csv(path)` — dump the recorded trajectory for plotting
- `hc.describe()` — printed at startup; the ABI, from the library's own
  manifest

The CSV that `L` writes has one row per solver step, so it is a real
trajectory record and not a per-frame sample.
