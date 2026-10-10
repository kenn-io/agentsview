#!/usr/bin/env python3
"""Metadata-only source inventory and disposable scanner-cache experiments."""
import argparse
import json
import os
import platform
import sqlite3
import stat
import statistics
import struct
import tempfile
import time
from pathlib import Path


DEFAULT_ROOTS = (
    ("claude", ".claude/projects"),
    ("codex", ".codex/sessions"),
    ("pi", ".pi/agent/sessions"),
    ("cursor", ".cursor/projects"),
    ("opencode", ".local/share/opencode"),
)
PAGE = 256


def load_average():
    getloadavg = getattr(os, "getloadavg", None)
    if getloadavg is None:
        return None
    try:
        return list(getloadavg())
    except OSError:
        return None


def is_directory_link(metadata):
    # Conservatively skip all Windows reparse points, including junctions.
    return (stat.S_ISLNK(metadata.st_mode) or
            bool(getattr(metadata, "st_file_attributes", 0) &
                 getattr(stat, "FILE_ATTRIBUTE_REPARSE_POINT", 0x400)))


def inventory(root, max_entries):
    """Inspect metadata, skipping symlinks. Return aggregates, never names."""
    out = dict(entries=0, directories=0, candidates=0, errors=0, symlinks=0,
               relative_path_bytes=0, basename_bytes=0, directory_name_bytes=0,
               recent_mtime_files={str(h): 0 for h in (1, 24, 168)}, truncated=False)
    now = time.time_ns()
    # Reject static symlinks in root components as well as descendants.
    for part in (root, *root.parents):
        try:
            if is_directory_link(part.lstat()):
                out["symlinks"] += 1
                out["skipped_root"] = True
                return out
        except OSError:
            out["errors"] += 1
            return out
    stack = [root]
    while stack:
        directory = stack.pop()
        out["directories"] += 1
        out["directory_name_bytes"] += len(os.fsencode(directory.name))
        try:
            with os.scandir(directory) as entries:
                for entry in entries:
                    if out["entries"] >= max_entries:
                        out["truncated"] = True
                        return out
                    out["entries"] += 1
                    try:
                        metadata = entry.stat(follow_symlinks=False)
                        if is_directory_link(metadata):
                            out["symlinks"] += 1
                        elif stat.S_ISDIR(metadata.st_mode):
                            stack.append(Path(entry.path))
                        elif stat.S_ISREG(metadata.st_mode) and entry.name.endswith(
                                (".jsonl", ".json", ".txt", ".db", ".db-wal")):
                            out["candidates"] += 1
                            out["relative_path_bytes"] += len(os.fsencode(
                                os.path.relpath(entry.path, root)))
                            out["basename_bytes"] += len(os.fsencode(entry.name))
                            age = now - metadata.st_mtime_ns
                            for hours in (1, 24, 168):
                                if 0 <= age <= hours * 3600 * 10**9:
                                    out["recent_mtime_files"][str(hours)] += 1
                    except OSError:
                        out["errors"] += 1
        except OSError:
            out["errors"] += 1
    return out


def measure_inventory(root, passes, max_entries):
    wall, cpu, results = [], [], []
    for _ in range(passes):
        start, cpu_start = time.perf_counter(), time.process_time()
        results.append(inventory(root, max_entries))
        wall.append(time.perf_counter() - start)
        cpu.append(time.process_time() - cpu_start)
    return dict(first_pass=results[0], last_pass=results[-1],
                first_pass_s=wall[0], subsequent_median_s=statistics.median(wall[1:]),
                subsequent_cpu_median_s=statistics.median(cpu[1:]))


def filename(i):
    return f"rollout-2026-10-01T00-00-00-{i:08d}-0000-0000-0000-000000000000.jsonl"


def signature(i):
    return (1024, 1700000000000000000, 1700000000000000000, i)


def packed_page(rows):
    """Front-code sorted basenames; retain four 64-bit signature values."""
    previous = b""
    data = bytearray()
    for name, values in rows:
        name = name.encode("ascii")
        prefix = 0
        while prefix < min(len(previous), len(name)) and previous[prefix] == name[prefix]:
            prefix += 1
        suffix = name[prefix:]
        data.extend(struct.pack("<HH4q", prefix, len(suffix), *values))
        data.extend(suffix)
        previous = name
    return bytes(data)


def unpack_page(data):
    previous, offset = b"", 0
    while offset < len(data):
        prefix, length, *values = struct.unpack_from("<HH4q", data, offset)
        offset += 36
        name = previous[:prefix] + data[offset:offset + length]
        offset += length
        yield name.decode("ascii"), tuple(values)
        previous = name


def cache_experiment(base, layout, count):
    path = base / f"{layout}-{count}.sqlite"
    connection = sqlite3.connect(path)
    connection.executescript("""
        PRAGMA page_size=4096;
        PRAGMA auto_vacuum=INCREMENTAL;
        PRAGMA journal_mode=TRUNCATE;
        PRAGMA cache_size=-8192;
    """)
    if layout == "full_paths":
        connection.executescript("""
            CREATE TABLE files(root_key TEXT, rel_path TEXT, parent TEXT,
              size INTEGER, mtime_ns INTEGER, ctime_ns INTEGER, inode INTEGER,
              PRIMARY KEY(root_key, rel_path));
            CREATE INDEX files_parent ON files(root_key, parent);
        """)
    else:
        connection.execute("""CREATE TABLE dirs(id INTEGER PRIMARY KEY,
            parent_id INTEGER, name TEXT NOT NULL, mtime_ns INTEGER,
            ctime_ns INTEGER, inode INTEGER)""")
        connection.execute("INSERT INTO dirs VALUES(0,NULL,'',0,0,0)")
        connection.executemany("INSERT INTO dirs VALUES(?,0,?,?,?,?)", (
            (d + 1, f"project-{d:04d}", 1700000000000000000,
             1700000000000000000, d) for d in range((count + PAGE - 1) // PAGE)))
        if layout == "interned_dirs":
            connection.execute("""CREATE TABLE files(dir_id INTEGER, name TEXT,
                size INTEGER, mtime_ns INTEGER, ctime_ns INTEGER, inode INTEGER,
                PRIMARY KEY(dir_id,name)) WITHOUT ROWID""")
        else:
            connection.execute("""CREATE TABLE pages(dir_id INTEGER, page INTEGER,
                payload BLOB, PRIMARY KEY(dir_id,page)) WITHOUT ROWID""")
    connection.commit()
    journal = Path(str(path) + "-journal")
    journal_sample = 0

    def install():
        nonlocal journal_sample
        for start in range(0, count, PAGE):
            end = min(start + PAGE, count)
            directory = f"project-{start // PAGE:04d}"
            if layout == "full_paths":
                connection.executemany("INSERT INTO files VALUES(?,?,?,?,?,?,?)", (
                    ("/source/provider/root", directory + "/" + filename(i),
                     directory, *signature(i)) for i in range(start, end)))
            elif layout == "interned_dirs":
                connection.executemany("INSERT INTO files VALUES(?,?,?,?,?,?)", (
                    (start // PAGE + 1, filename(i), *signature(i))
                    for i in range(start, end)))
            else:
                rows = [(filename(i), signature(i)) for i in range(start, end)]
                payload = packed_page(rows)
                connection.execute("INSERT INTO pages VALUES(?,0,?)",
                                   (start // PAGE + 1, payload))
            if journal.exists():
                journal_sample = max(journal_sample, journal.stat().st_size)
            connection.commit()

    start = time.perf_counter()
    install()
    build_s = time.perf_counter() - start
    main_bytes = path.stat().st_size
    table = "pages" if layout == "prefix_pages" else "files"
    # Same whole-listing deletion/replacement workload for every representation.
    start = time.perf_counter()
    connection.execute(f"DELETE FROM {table}")
    if journal.exists():
        journal_sample = max(journal_sample, journal.stat().st_size)
    connection.commit()
    after_delete = path.stat().st_size
    install()
    churn_s = time.perf_counter() - start
    after_churn = path.stat().st_size
    connection.close()
    # Reopen, consume every signature, and prove the codec preserves all values.
    start = time.perf_counter()
    connection = sqlite3.connect(path)
    observed = 0
    if layout == "prefix_pages":
        for directory, payload in connection.execute(
                "SELECT dir_id,payload FROM pages ORDER BY dir_id,page"):
            for name, values in unpack_page(payload):
                assert name == filename(observed)
                assert values == signature(observed)
                assert directory == observed // PAGE + 1
                observed += 1
    else:
        query = ("SELECT rel_path,size,mtime_ns,ctime_ns,inode FROM files ORDER BY rel_path"
                 if layout == "full_paths" else
                 "SELECT name,size,mtime_ns,ctime_ns,inode FROM files ORDER BY dir_id,name")
        for name, *values in connection.execute(query):
            assert name.rsplit("/", 1)[-1] == filename(observed)
            assert tuple(values) == signature(observed)
            observed += 1
    assert observed == count
    reopen_read_s = time.perf_counter() - start
    connection.close()
    return dict(layout=layout, files=count, main_bytes=main_bytes,
                bytes_per_file=main_bytes / count, build_s=build_s,
                after_delete_bytes=after_delete, after_churn_bytes=after_churn,
                delete_and_refill_s=churn_s, reopen_full_read_s=reopen_read_s,
                journal_sample_bytes=journal_sample, roundtrip_files=observed)


def synthetic_files(base, count, passes):
    root = base / "files"
    root.mkdir()
    paths, directories = [], []
    start = time.perf_counter()
    for i in range(count):
        if i % PAGE == 0:
            directory = root / f"project-{i // PAGE:04d}"
            directory.mkdir()
            directories.append(directory)
        path = directory / filename(i)
        path.touch()
        paths.append(path)
    create_s = time.perf_counter() - start
    timings = dict(stats=[], listings=[], enumerate_and_stat=[])
    for _ in range(passes):
        start = time.perf_counter()
        for path in paths:
            path.lstat()
        timings["stats"].append(time.perf_counter() - start)
        start = time.perf_counter()
        for directory in directories:
            with os.scandir(directory) as entries:
                for _ in entries:
                    pass
        timings["listings"].append(time.perf_counter() - start)
        start = time.perf_counter()
        for directory in directories:
            with os.scandir(directory) as entries:
                for entry in entries:
                    entry.stat(follow_symlinks=False)
        timings["enumerate_and_stat"].append(time.perf_counter() - start)
    return dict(files=count, directories=len(directories), create_s=create_s,
                warm_median_s={k: statistics.median(v) for k, v in timings.items()})


def positive(value):
    result = int(value)
    if result < 1:
        raise argparse.ArgumentTypeError("must be positive")
    return result


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", type=Path, action="append",
                        help="replace default roots; reported only as root-N")
    parser.add_argument("--skip-live", action="store_true")
    parser.add_argument("--scratch-parent", type=Path,
                        help="existing directory on the filesystem to measure")
    parser.add_argument("--synthetic-files", type=positive, default=50000)
    parser.add_argument("--rows", type=positive, nargs="+", default=[50000, 250000])
    parser.add_argument("--passes", type=positive, default=5)
    parser.add_argument("--max-entries", type=positive, default=1000000)
    args = parser.parse_args()
    if args.passes < 2:
        parser.error("--passes must be at least 2")
    os.umask(0o077)
    report = dict(report_version=1, logical_cpus=os.cpu_count(),
                  load_average_start=load_average(), system=platform.system(),
                  os_release=platform.release(), architecture=platform.machine(),
                  python=platform.python_version(), sqlite=sqlite3.sqlite_version,
                  page_records=PAGE, live=[], cache_comparison=[])
    if not args.skip_live:
        roots = ([(f"root-{i}", p.expanduser().absolute())
                  for i, p in enumerate(args.root or [], 1)] if args.root else
                 [(label, Path.home() / relative) for label, relative in DEFAULT_ROOTS])
        for label, root in roots:
            report["live"].append(dict(source=label, **measure_inventory(
                root, args.passes, args.max_entries)))
    with tempfile.TemporaryDirectory(prefix="source-watch-measure-",
                                     dir=args.scratch_parent) as scratch:
        base = Path(scratch)
        report["synthetic"] = synthetic_files(base, args.synthetic_files, args.passes)
        for rows in args.rows:
            for layout in ("full_paths", "interned_dirs", "prefix_pages"):
                report["cache_comparison"].append(cache_experiment(base, layout, rows))
    report["load_average_end"] = load_average()
    print(json.dumps(report, indent=2))


if __name__ == "__main__":
    main()
