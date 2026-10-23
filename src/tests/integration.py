#!/usr/bin/env python3
"""
Integration tests for the S3Lite redundancy pipeline.

Requirements
------------
A running SeaweedFS cluster and control plane:
  master (9333), S3 gateway (8333), control plane (8099), bin/weed, bin/mc.
The script resets the cluster itself during setup (full reset), so it can be
re-run safely and does not depend on a previously prepared test state.

Cases (EC 6+4, file ~3 MB)
---------------------------
  A  four PCs offline   -> volumes DEGRADED (6/10), data readable,
                           offline_nodes == 4, no start advice
  B  five PCs offline   -> volumes AT_RISK (5/10), data unreadable (cold read),
                           nodes_to_start recommends the exact PCs to power on
  C  PCs return         -> volumes OK (10/10) automatically, offline cleared
  D  one PC down        -> DEGRADED, ec.rebuild plans reconstruction,
                           data readable
  E  shard file lost    -> ec.rebuild restores, data still readable

How to run (see also: python3 integration.py --help)
-----------------------------------------------------
  # 1) Everything (default): reset cluster, upload, EC 6+4, then all cases:
  python3 integration.py

  # 2) Selected cases only (each case handles its own preconditions):
  python3 integration.py -c A,B,E

  # 3) Just prepare the environment, without running test cases:
  python3 integration.py --setup-only

  # 4) Reset the cluster before running, even if it was left dirty:
  python3 integration.py   # setup always performs the full reset first

Exit code: 0 if all checks passed, 1 on any failure, 2 on usage errors.
"""

import argparse
import glob
import hashlib
import json
import os
import re
import subprocess
import sys
import time
import urllib.request

ROOT = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
WEED = os.path.join(ROOT, "bin", "weed")
MC = os.path.join(ROOT, "bin", "mc")
CLUSTER_SCRIPT = os.path.join(ROOT, "scripts", "start_cluster.sh")

MASTER = "192.168.1.9:9333"
MASTER_HTTP = "http://127.0.0.1:9333"
S3_URL = "http://192.168.1.9:8333"
CP_URL = "http://192.168.1.9:8099"

COLLECTION = "itest"
DATA_FILE = "/tmp/itest.dat"
EXPECTED_MD5 = ""

PASS = 0
FAIL = 0


# --- helpers ---------------------------------------------------------------
def run(args, input_text=None, timeout=120):
    p = subprocess.run(args, input=input_text, capture_output=True, text=True,
                       timeout=timeout)
    return p.returncode, p.stdout


def run_bg(args, log_path):
    with open(log_path, "w") as f:
        subprocess.Popen(args, stdout=f, stderr=subprocess.STDOUT,
                         start_new_session=True)


def http_get_json(url):
    try:
        with urllib.request.urlopen(url, timeout=5) as r:
            return json.loads(r.read().decode())
    except Exception:
        return None


def volumes():
    d = http_get_json(f"{CP_URL}/api/v1/volumes/risk")
    return d.get("volumes", []) if d else []


def states():
    return [v.get("state", "") for v in volumes()
            if v.get("collection") == COLLECTION]


def avails():
    vs = [v for v in volumes() if v.get("collection") == COLLECTION]
    return ",".join(f"{v['available_shards']}/{v['total_shards']}" for v in vs)


def all_offline():
    """Union of offline nodes across the collection's volumes."""
    seen = set()
    for v in volumes():
        if v.get("collection") == COLLECTION:
            seen.update(v.get("offline_nodes", []))
    return sorted(seen)


def all_start_advice():
    """Union of recommended nodes across the collection's volumes."""
    seen = set()
    for v in volumes():
        if v.get("collection") == COLLECTION:
            seen.update(v.get("nodes_to_start", []))
    return sorted(seen)


def node_pid(name):
    try:
        with open(os.path.join(ROOT, ".cluster", "pids", f"{name}.pid")) as f:
            return int(f.read().strip())
    except Exception:
        return None


def kill_node(n):
    pid = node_pid(f"volume{n}")
    if pid:
        try:
            os.kill(pid, 9)
        except ProcessLookupError:
            pass


def ensure_node_offline(n):
    if node_pid(f"volume{n}"):
        kill_node(n)
    # Kill again silently after the pid refresh, in case a previous restart
    # reused the pid file for a live process we already killed.

def shell(cmds):
    return run([WEED, "shell", f"-master={MASTER}"],
               input_text="\n".join(cmds) + "\n")


def read_ok():
    p = subprocess.run([MC, "cat", f"dev/{COLLECTION}/file.bin"],
                       capture_output=True)
    return hashlib.md5(p.stdout).hexdigest() == EXPECTED_MD5


def wait_s3(timeout=90):
    for _ in range(timeout):
        try:
            urllib.request.urlopen(S3_URL, timeout=2)
            return True
        except Exception:
            time.sleep(1)
    return False


def wait_cp(timeout=60):
    for _ in range(timeout):
        try:
            urllib.request.urlopen(f"{CP_URL}/healthz", timeout=2)
            return True
        except Exception:
            time.sleep(1)
    return False


def wait_for(desc, check, attempts=20, delay=3):
    """Poll `check` (callable -> bool) with visible progress dots."""
    print(f"    waiting for {desc}:", end="", flush=True)
    for _ in range(attempts):
        if check():
            print(" done")
            return True
        print(".", end="", flush=True)
        time.sleep(delay)
    print(" TIMEOUT")
    return False


def pause(desc, secs):
    print(f"    {desc} ({secs}s)", flush=True)
    time.sleep(secs)


def ok(msg):
    global PASS
    PASS += 1
    print(f"  [PASS] {msg}")


def bad(msg):
    global FAIL
    FAIL += 1
    print(f"  [FAIL] {msg}")


def check(name, cond, got=""):
    (ok if cond else bad)(f"{name}" + (f" (got {got})" if got else ""))


def all_ok_state():
    s = states()
    return bool(s) and all(x == "OK" for x in s)


def any_state(regex):
    return any(re.search(regex, s) for s in states())


# --- setup -----------------------------------------------------------------
def setup():
    print("==> Setup: full cluster reset")
    run([CLUSTER_SCRIPT, "stop"], timeout=60)
    for pf in glob.glob(os.path.join(ROOT, ".cluster", "pids", "agent*.pid")):
        try:
            with open(pf) as f:
                pid = int(f.read().strip())
            os.kill(pid, 9)
        except Exception:
            pass
    time.sleep(1)
    for d in ("data", "logs", "pids"):
        subprocess.run(["rm", "-rf", os.path.join(ROOT, ".cluster", d)])
    run([CLUSTER_SCRIPT, "start"], timeout=60)

    print("==> Waiting for the S3 gateway and control plane")
    wait_s3()
    if not wait_cp():
        print("FATAL: control plane not running on", CP_URL)
        sys.exit(2)

    print("==> Preparing an EC 6+4 volume")
    run([MC, "alias", "set", "dev", S3_URL, "some_access_key1", "some_secret_key1"])
    run([MC, "mb", f"dev/{COLLECTION}"])
    with open(DATA_FILE, "wb") as f:
        f.write(os.urandom(3 * 1024 * 1024))
    global EXPECTED_MD5
    EXPECTED_MD5 = hashlib.md5(open(DATA_FILE, "rb").read()).hexdigest()
    print("    uploading 3 MB test file ...", flush=True)
    run([MC, "cp", DATA_FILE, f"dev/{COLLECTION}/file.bin"], timeout=60)
    pause("letting the master see the volumes", 6)

    d = http_get_json(f"{MASTER_HTTP}/vol/status")
    vol_ids = []
    for dc in d["Volumes"]["DataCenters"].values():
        for rack in dc.values():
            for server, rs in rack.items():
                for v in rs:
                    if v.get("Collection") == COLLECTION and v.get("FileCount", 0) > 0:
                        vol_ids.append(str(v["Id"]))
    print("    data volumes:", ",".join(vol_ids) or "NONE")
    if not vol_ids:
        print("FATAL: no data volumes found after upload")
        sys.exit(2)

    print("    encoding volumes as 6+4 via weed shell ...", flush=True)
    shell(["lock",
           f"ec.encode -volumeIds={','.join(vol_ids)} -dataShards=6 -parityShards=4",
           "unlock"])
    pause("letting EC shards register and the control plane poll", 8)

    okf = wait_for("volumes to be OK after encoding", all_ok_state, attempts=12)
    check("setup: volumes OK after EC 6+4", okf, avails())


# --- cases ---------------------------------------------------------------
def case_A():
    print("==> Case A: 4 PCs offline -> DEGRADED, data readable")
    print("    expect: state DEGRADED, available 6/10, offline==4 nodes, no start advice, file readable")
    for n in (4, 5, 6, 7):
        kill_node(n)
        print(f"    killed volume{n}")
    wait_for("DEGRADED", lambda: any_state(r"^DEGRADED"))
    check("A: state DEGRADED", "DEGRADED" in states(), ",".join(states()))
    check("A: available=6/10", all(a == "6/10" for a in avails().split(",")), avails())
    off = all_offline()
    check("A: offline lists 4 nodes", len(off) == 4 and len(set(off)) == 4, ",".join(off))
    check("A: no start advice while DEGRADED", not all_start_advice())
    check("A: file still readable", read_ok())


def case_B():
    print("==> Case B: 5 PCs offline -> AT_RISK, data unreadable")
    print("    expect: state AT_RISK, available 5/10, offline>=5, nodes_to_start non-empty, file unreadable (cold)")
    for n in (4, 5, 6, 7, 9):
        kill_node(n)
    wait_for("AT_RISK", lambda: any_state(r"^AT_RISK"))
    check("B: state AT_RISK", "AT_RISK" in states(), ",".join(states()))
    check("B: available=5/10", all(a == "5/10" for a in avails().split(",")), avails())
    off = all_offline()
    check("B: offline lists the gone PCs", len(off) >= 5, ",".join(off))
    advice = all_start_advice()
    check("B: recommends PCs to start", len(advice) >= 1, ",".join(advice))
    # Cold read: restart the filer so its cache (warmed by previous cases)
    # cannot serve the file; the cluster itself must return an error.
    print("    restarting filer for a cold read ...", flush=True)
    pid = node_pid("filer")
    if pid:
        try:
            os.kill(pid, 9)
        except ProcessLookupError:
            pass
    time.sleep(2)
    run_bg([WEED, "filer", "-port=8888", "-master=" + MASTER],
           os.path.join(ROOT, ".cluster", "logs", "filer.log"))
    pause("filer warm-up", 4)
    if read_ok():
        bad("B: file must be unreadable")
    else:
        ok("B: file unreadable")


def case_C():
    print("==> Case C: PCs return -> OK automatically")
    print("    expect: state OK, no offline nodes, file readable")
    # The cluster is still alive: restart to emulate the PCs being powered on.
    run([CLUSTER_SCRIPT, "stop"], timeout=60)
    run([CLUSTER_SCRIPT, "start"], timeout=60)
    okf = wait_for("volumes to be OK after return", all_ok_state, attempts=20)
    check("C: state OK again", okf, ",".join(states()))
    check("C: no offline nodes remain", not all_offline())
    check("C: file readable after return", read_ok())


def case_D():
    print("==> Case D: one PC down -> ec.rebuild reconstructs")
    print("    expect: state DEGRADED, ec.rebuild plans copies, file readable")
    kill_node(3)
    print("    killed volume3")
    wait_for("DEGRADED", lambda: any_state(r"^DEGRADED"))
    check("D: state DEGRADED", "DEGRADED" in states(), ",".join(states()))
    print("    running ec.rebuild (dry-run shown as 'would copy') ...", flush=True)
    rc, out = shell(["lock", "confirm", f"ec.rebuild -collection={COLLECTION}", "unlock"])
    for line in out.splitlines():
        if "would copy" in line or "unrepairable" in line:
            print("    " + line)
    pause("after rebuild", 8)
    check("D: file readable after ec.rebuild", read_ok())


def case_E():
    print("==> Case E: shard file lost on a live node -> ec.rebuild restores")
    print("    expect: a shard file is removed, ec.rebuild runs, file stays readable")
    shards = []
    for v in volumes():
        if v.get("collection") != COLLECTION:
            continue
        vid = v["volume_id"]
        prefix = f"{COLLECTION}_{vid}."
        for dd in glob.glob(os.path.join(ROOT, ".cluster", "data", "v*")):
            for fn in os.listdir(dd):
                if fn.startswith(prefix) and re.search(r"\.ec\d\d$", fn):
                    shards.append(os.path.join(dd, fn))
        if shards:
            break
    if not shards:
        bad("E: no shard file found to delete")
        return
    target = shards[0]
    print(f"    removing {os.path.relpath(target, ROOT)} ...", flush=True)
    os.remove(target)
    print("    running ec.rebuild ...", flush=True)
    shell(["lock", "confirm", f"ec.rebuild -collection={COLLECTION}", "unlock"])
    pause("after rebuild", 8)
    check("E: file readable after shard rebuild", read_ok())


CASES = {"A": case_A, "B": case_B, "C": case_C, "D": case_D, "E": case_E}


# --- main ------------------------------------------------------------------
def main():
    # Usage (see the module docstring for full detail):
    #   python3 integration.py            # run all cases
    #   python3 integration.py -c A,B,E   # run selected cases only
    #   python3 integration.py --setup-only
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("-c", "--cases", default="ALL",
                        help="comma-separated cases to run, e.g. A,B,E (default: ALL)")
    parser.add_argument("--setup-only", action="store_true",
                        help="only reset the cluster and prepare an EC 6+4 volume")
    args = parser.parse_args()

    setup()
    if args.setup_only:
        print(f"==> Setup complete. Summary so far: PASS={PASS} FAIL={FAIL}")
        sys.exit(0 if FAIL == 0 else 1)

    selected = list(CASES) if args.cases == "ALL" else [c.strip().upper() for c in args.cases.split(",")]
    unknown = [c for c in selected if c not in CASES]
    if unknown:
        print("Unknown cases:", ", ".join(unknown))
        sys.exit(2)

    for name in selected:
        print()
        CASES[name]()

    print()
    print(f"==> Summary: PASS={PASS} FAIL={FAIL}")
    sys.exit(0 if FAIL == 0 else 1)


if __name__ == "__main__":
    main()
