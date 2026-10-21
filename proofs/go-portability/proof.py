"""Disposable native proof and strict five-target evidence gate. No providers."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import platform
import re
import shutil
import subprocess
import tempfile
import time

ROOT = Path(__file__).resolve().parent
TARGETS = ("darwin/arm64", "linux/arm64", "linux/amd64", "windows/arm64", "windows/amd64")
GO_VERSION = "go1.27.3"
CHECKS = {"legacy-control": 0, "launch": 0, "path": 0, "exchange-success": 0,
          "exchange-failure": 1, "legacy-unsupported": 2, "new-core-failure": 1}
SOURCES = ("go.mod", "main.go", "native_unix.go", "native_windows.go", "proof.py", "test_proof.py")
LIMIT = 65536
BUILD_TIMEOUT = 600


class InvalidEvidence(ValueError):
    pass


def require(condition, message):
    if not condition:
        raise InvalidEvidence(message)


def jsonl(raw, count):
    require(isinstance(raw, bytes), "capture must retain bytes")
    require(len(raw) <= LIMIT, "output exceeds bound")
    require(b"\r" not in raw and not raw.startswith(b"\xef\xbb\xbf"), "JSONL requires LF and no BOM")
    require(raw.endswith(b"\n") if count else raw == b"", "invalid terminal newline")
    lines = raw[:-1].split(b"\n") if raw else []
    require(len(lines) == count and all(lines), "wrong record count")
    try:
        values = [json.loads(line.decode("utf-8")) for line in lines]
    except (UnicodeError, ValueError) as exc:
        raise InvalidEvidence("invalid UTF-8 JSON") from exc
    require(all(isinstance(v, dict) for v in values), "records must be objects")
    return values


def identity(commit):
    require(bool(re.fullmatch(r"[0-9a-f]{40}", commit)), "source commit required")
    return {"commit": commit, "files": {name: hashlib.sha256((ROOT / name).read_bytes()).hexdigest()
                                        for name in SOURCES}}


def capture(argv, env, cwd, timeout=120):
    start = time.monotonic()
    # Temporary file capture bounds memory; reports never contain unfiltered output.
    with tempfile.TemporaryFile() as out, tempfile.TemporaryFile() as err:
        try:
            p = subprocess.run(argv, cwd=cwd, env=env, stdout=out, stderr=err, timeout=timeout)
        except subprocess.TimeoutExpired as exc:
            raise InvalidEvidence("command timed out") from exc
        out.seek(0)
        err.seek(0)
        stdout, stderr = out.read(LIMIT + 1), err.read(LIMIT + 1)
    require(len(stdout) <= LIMIT and len(stderr) <= LIMIT, "command output exceeds bound")
    return p.returncode, stdout, stderr, round(time.monotonic() - start, 3)


def expected(name, target):
    osname, arch = target.split("/")
    outputs = {
        "legacy-control": [],
        "launch": [{"fixture": True, "os": osname, "arch": arch, "native_arch": arch, "emulated": False}],
        "path": [{"path": "<fixture>", "content": "payload café 雪\n"}],
        "exchange-success": [{"child_exit": 0, "response": {"value": "space café 雪", "state": "ok"}}],
        "exchange-failure": [{"child_exit": 7, "response": {"value": "space café 雪", "state": "failed"}}],
        "legacy-unsupported": [], "new-core-failure": [],
    }
    diagnostics = {"exchange-failure": "PROOF_CHILD_FAILURE", "legacy-unsupported": "PROOF_UNSUPPORTED_LEGACY_ROUTE",
                   "new-core-failure": "PROOF_NEW_CORE_FAILURE"}
    return outputs[name], ([{"diagnostic": diagnostics[name]}] if name in diagnostics else [])


def validate_check(check, target):
    name = check.get("name")
    require(name in CHECKS, "unknown check")
    require(check.get("passed") is True, "native check failed")
    require(type(check.get("exit")) is int and check["exit"] == CHECKS[name], "wrong check exit")
    out, err = expected(name, target)
    require(check.get("stdout") == out and check.get("stderr") == err, "unexpected check records")
    require(check.get("byte_jsonl_valid") is True, "byte validation missing")
    require(check.get("marker_valid") is True, "legacy marker validation missing")


def validate_report(report, source):
    target = report.get("target")
    require(target in TARGETS, "unknown target")
    require(report.get("schema") == 1 and report.get("mode") == "native", "unknown evidence schema/mode")
    require(report.get("source") == source, "source identity mismatch")
    require(report.get("go_version") == GO_VERSION, "Go version mismatch")
    require(report.get("cgo_enabled") == "0", "unexpected cgo setting")
    require(report.get("errors") == [], "run contains failures")
    build = report.get("build", {})
    require(build.get("passed") is True and type(build.get("exit")) is int and build["exit"] == 0, "build not passed")
    require(bool(re.fullmatch(r"[0-9a-f]{64}", build.get("sha256", ""))), "artifact identity missing")
    checks = report.get("checks", [])
    require(len(checks) == len(CHECKS) and {c.get("name") for c in checks} == set(CHECKS), "missing or duplicate checks")
    for check in checks:
        validate_check(check, target)
    require(report.get("native_passed") is True, "native target not passed")


def aggregate(reports, source):
    require(len(reports) == len(TARGETS), "five reports required")
    require({r.get("target") for r in reports} == set(TARGETS), "missing or duplicate targets")
    for report in reports:
        validate_report(report, source)
    by_target = {r["target"]: r for r in reports}
    matrix = {target: {"build_passed": True, "native_passed": True, "artifact_sha256": by_target[target]["build"]["sha256"],
                       "go_version": by_target[target]["go_version"], "build_seconds": by_target[target]["build"].get("seconds"),
                       "host": by_target[target].get("host"), "checks": len(by_target[target]["checks"])}
              for target in TARGETS}
    return {"passed": True, "source": source, "targets": list(TARGETS), "matrix": matrix}


def smoke(binary, target, env, directory):
    sandbox = directory / "fixture café 雪"
    sandbox.mkdir()
    executable = sandbox / ("proof runner.exe" if target.startswith("windows/") else "proof runner")
    shutil.copy2(binary, executable)
    marker = sandbox / "legacy marker"
    path = sandbox / "input café 雪.txt"
    path.write_bytes("payload café 雪\n".encode("utf-8"))
    env = dict(env, PROOF_LEGACY_MARKER=str(marker))
    checks = []
    for name in CHECKS:
        args = {"legacy-control": ["_legacy-fixture"], "path": ["path", str(path)],
                "exchange-success": ["exchange", "success"], "exchange-failure": ["exchange", "failure"]}.get(name, [name])
        check = {"name": name, "command": ["<artifact>"] + (["path", "<fixture>"] if name == "path" else args), "passed": False}
        try:
            code, out, err, duration = capture([str(executable)] + args, env, ROOT)
            check.update(exit=code, seconds=duration)
            wanted_out, wanted_err = expected(name, target)
            actual_out, actual_err = jsonl(out, len(wanted_out)), jsonl(err, len(wanted_err))
            if name == "path":
                require(actual_out == [{"path": str(path), "content": "payload café 雪\n"}], "path roundtrip failed")
                actual_out[0]["path"] = "<fixture>"
            check.update(stdout=actual_out, stderr=actual_err, byte_jsonl_valid=True)
            if name == "legacy-control":
                require(marker.read_bytes() == b"fixture invoked\n", "legacy positive control failed")
                marker.unlink()
            else:
                require(not marker.exists(), "legacy route invoked unexpectedly")
            check.update(marker_valid=True, passed=True)
            validate_check(check, target)
        except (InvalidEvidence, OSError) as exc:
            # Only controlled messages are published; OS errors may contain host paths.
            check.update(passed=False, failure=str(exc) if isinstance(exc, InvalidEvidence) else "fixture OS operation failed")
            check.pop("stdout", None)
            check.pop("stderr", None)
        checks.append(check)
    return checks


def native_run(target, commit):
    report = {"schema": 1, "mode": "native", "target": target, "source": identity(commit),
              "go_version": "unknown", "cgo_enabled": "0", "build": {"passed": False},
              "checks": [], "native_passed": False, "errors": [],
              "host": {"os": platform.system(), "release": platform.release(), "machine": platform.machine()}}
    try:
        osname, arch = target.split("/")
        require(platform.system().lower() == osname, "host OS does not match target")
        with tempfile.TemporaryDirectory(prefix="go-proof-") as temp:
            directory = Path(temp)
            env = dict(os.environ, CGO_ENABLED="0", GOTOOLCHAIN="local", GOPROXY="off", GOSUMDB="off",
                       GOENV="off", GOWORK="off", GOFLAGS="", GOTELEMETRY="off", GOOS=osname, GOARCH=arch,
                       GOCACHE=str(directory / "cache"), GOPATH=str(directory / "gopath"), GOMODCACHE=str(directory / "modules"))
            go = shutil.which("go")
            require(go is not None, "Go toolchain unavailable")
            code, out, err, _ = capture([go, "version"], env, ROOT)
            require(code == 0 and not err, "Go version command failed")
            fields = out.decode("utf-8").split()
            require(len(fields) == 4 and fields[2] == GO_VERSION, "unexpected Go version")
            report["go_version"] = fields[2]
            binary = directory / ("proof.exe" if osname == "windows" else "proof")
            # A cold standard-library compile on a slow hosted runner is not a fixture failure.
            code, out, err, seconds = capture([go, "build", "-trimpath", "-o", str(binary), "."], env, ROOT, timeout=BUILD_TIMEOUT)
            report["build"] = {"passed": code == 0, "exit": code, "seconds": seconds,
                               "command": ["go", "build", "-trimpath", "-o", "<artifact>", "."],
                               "environment": {"GOOS": osname, "GOARCH": arch, "CGO_ENABLED": "0"}}
            require(code == 0, "build failed (inspect job console/toolchain)")
            report["build"]["sha256"] = hashlib.sha256(binary.read_bytes()).hexdigest()
            report["checks"] = smoke(binary, target, env, directory)
            report["native_passed"] = len(report["checks"]) == len(CHECKS) and all(c["passed"] for c in report["checks"])
            validate_report(report, report["source"])
    except (InvalidEvidence, OSError, UnicodeError) as exc:
        report["errors"].append(str(exc) if isinstance(exc, InvalidEvidence) else "toolchain or fixture OS operation failed")
        report["native_passed"] = False
    return report


def write_report(path, report):
    data = (json.dumps(report, ensure_ascii=False, indent=2) + "\n").encode("utf-8")
    require(len(data) <= LIMIT, "report exceeds bound")
    path.parent.mkdir(parents=True, exist_ok=True)
    with path.open("xb") as f:  # Never overwrite earlier evidence.
        f.write(data)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("mode", choices=("native", "aggregate"))
    parser.add_argument("--target", choices=TARGETS)
    parser.add_argument("--source-commit", required=True)
    parser.add_argument("--output", required=True, type=Path)
    parser.add_argument("--reports", type=Path)
    args = parser.parse_args()
    if args.mode == "native":
        require(args.target is not None, "target required")
        report = native_run(args.target, args.source_commit)
        passed = report["native_passed"]
    else:
        try:
            require(args.reports is not None, "reports directory required")
            paths = sorted(args.reports.rglob("*.json"))
            require(len(paths) == 5, "five report files required")
            require(all(p.stat().st_size <= LIMIT for p in paths), "report exceeds bound")
            reports = [json.loads(p.read_text(encoding="utf-8")) for p in paths]
            report = aggregate(reports, identity(args.source_commit))
            passed = True
        except (InvalidEvidence, ValueError, OSError) as exc:
            report = {"passed": False, "error": str(exc) if isinstance(exc, InvalidEvidence) else "invalid report files"}
            passed = False
    write_report(args.output, report)
    print(json.dumps({"passed": passed, "mode": args.mode, "target": args.target}))
    return 0 if passed else 1


if __name__ == "__main__":
    raise SystemExit(main())
