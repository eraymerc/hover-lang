"""Drive examples/rc_filter.hvr from Python.

Run from anywhere:

    python3 hovercraft_drivers/python/examples/drive_rc.py

Compiles the .hvr on first run (and whenever it changes), then steps the
simulation in a closed loop -- reading the output each step and feeding it
back into the input, which is the thing a one-shot binary cannot do.
"""

import sys
from pathlib import Path

# Allow running straight from a checkout without installing.
sys.path.insert(0, str(Path(__file__).resolve().parents[2]))

from hovercraft import Hovercraft  # noqa: E402

HERE = Path(__file__).resolve().parent


def main():
    with Hovercraft(HERE / "rc_filter.hvr") as hc:
        print("--DESCRIBE--")
        print(hc.describe(), "\n")
        print("--DESCRIBE--")
        # <> params are only settable at t == 0.
        hc.params.gain = 2.0

        # () inputs are settable at any time.
        hc.inputs.vin = 1.0
        hc.inputs.taps = [0.5, 0.25, 0.25]   # drive = 2*1.0 + 1.0 = 3.0

        print("closed loop: bang-bang control toward a 1.5 V setpoint")
        print("  {:>8}  {:>9}  {:>9}  {:>9}".format("t (ms)", "vin", "drive", "vout"))
        setpoint = 1.5
        for i in range(8):
            hc.run(200e-6)
            vout = hc.outputs.main_vout
            # React to the measurement: drop the drive once we pass the
            # setpoint, raise it when we fall short.
            hc.inputs.vin = 0.2 if vout > setpoint else 1.0
            print("  {:8.3f}  {:9.4f}  {:9.4f}  {:9.4f}".format(
                hc.time * 1e3, hc.inputs.vin, hc.outputs.main_drive, vout))

        log = hc.log()
        print("\nlogged {} rows x {} columns: {}".format(
            len(log), len(log.names), ", ".join(log.names)))
        print("vout range: {:.4f} .. {:.4f} V".format(
            min(log["main.vout"]), max(log["main.vout"])))

        # Independent second instance from the same file, to contrast.
        with Hovercraft(HERE / "rc_filter.hvr", rebuild="never") as other:
            other.params.gain = 10.0
            other.inputs.vin = 1.0
            other.run(1e-3)
            print("\nsecond instance is a separate simulation:")
            print("  first  drive = {:.4f} (gain=2)".format(hc.outputs.main_drive))
            print("  second drive = {:.4f} (gain=10)".format(other.outputs.main_drive))


if __name__ == "__main__":
    main()
