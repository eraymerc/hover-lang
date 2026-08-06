"""Log — the Python-side view of an HVRLogResult."""

from .errors import UnknownSignalError


class Log:
    """A block of logged simulation data.

    Holds plain Python lists, fully detached from the library: the
    HVRLogResult it came from is freed before this object is returned, so
    a Log stays valid after the Hovercraft that produced it is closed.

    Indexable by column name (`log["main.vout"]`), by column position, and
    iterable row-wise as dicts.
    """

    __slots__ = ("time", "names", "columns")

    def __init__(self, time, names, columns):
        self.time = time
        self.names = names
        self.columns = columns

    @classmethod
    def from_result(cls, lib, result):
        """Copy an HVRLogResult into Python objects, then free it.

        The free is unconditional (finally): every exit path from here has
        to release the malloc'd arrays, or a polling loop calling
        last_step() each step leaks steadily.
        """
        try:
            n_rows = int(result.n_rows)
            n_cols = int(result.n_cols)

            time = list(result.time[:n_rows]) if n_rows else []
            names, columns = [], {}
            for c in range(n_cols):
                raw = result.names[c]
                name = raw.decode("utf-8", "replace") if raw else "col{}".format(c)
                names.append(name)
                columns[name] = list(result.columns[c][:n_rows]) if n_rows else []
            return cls(time, names, columns)
        finally:
            lib.HVR_free_log_result(result)

    # ── mapping / sequence access ────────────────────────────────────────

    def __getitem__(self, key):
        if isinstance(key, int):
            return self.columns[self.names[key]]
        try:
            return self.columns[key]
        except KeyError:
            raise UnknownSignalError(
                "no logged column {!r}. Available: {}".format(
                    key, ", ".join(self.names) or "(none)")
            ) from None

    def __contains__(self, key):
        return key in self.columns

    def __len__(self):
        """Number of rows -- so `if log:` means "did anything get logged"."""
        return len(self.time)

    def __iter__(self):
        """Iterate rows as dicts, time included."""
        for i in range(len(self.time)):
            row = {"time": self.time[i]}
            for name in self.names:
                row[name] = self.columns[name][i]
            yield row

    def get(self, key, default=None):
        return self.columns.get(key, default)

    # ── conversions ──────────────────────────────────────────────────────

    def to_dict(self):
        """{"time": [...], "<column>": [...], ...} — a copy."""
        out = {"time": list(self.time)}
        for name in self.names:
            out[name] = list(self.columns[name])
        return out

    def to_numpy(self):
        """(time, data) as numpy arrays; data is (n_rows, n_cols).

        numpy is imported here rather than at module scope so the package
        has no hard dependency on it.
        """
        import numpy as np

        data = np.empty((len(self.time), len(self.names)), dtype=float)
        for c, name in enumerate(self.names):
            data[:, c] = self.columns[name]
        return np.asarray(self.time, dtype=float), data

    def to_pandas(self):
        """A DataFrame indexed by time. Requires pandas."""
        import pandas as pd

        return pd.DataFrame(
            {name: self.columns[name] for name in self.names},
            index=pd.Index(self.time, name="time"),
        )

    def to_csv(self, path):
        """Write the same column layout HVR_save_log would."""
        import csv

        with open(path, "w", newline="") as fh:
            w = csv.writer(fh)
            w.writerow(["time"] + self.names)
            for i in range(len(self.time)):
                w.writerow([self.time[i]] + [self.columns[n][i] for n in self.names])

    def __repr__(self):
        return "<Log {} rows x {} columns: {}>".format(
            len(self.time), len(self.names),
            ", ".join(self.names) if self.names else "(none)")
