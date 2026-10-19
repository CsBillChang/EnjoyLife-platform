#!/usr/bin/env python3
"""Verify migration fault assertions against three isolated negative controls.

Requires Docker, Go, and Python 3. All database writes use a newly created MySQL
container with tmpfs storage. Source mutations exist only in temporary overlays.
"""

import argparse
import contextlib
import hashlib
import json
import os
from pathlib import Path
import re
import secrets
import signal
import subprocess
import sys
import tempfile
import time
import uuid


REPO = Path(__file__).resolve().parents[1]
SOURCE = REPO / "internal/platform/migrate/migrate.go"
TEST_SOURCE = REPO / "internal/platform/migrate/failure_integration_test.go"
TEST = "TestMigrationFailureReleasesLocksAndRecoversPoolIntegration"
CASES = {
    "acquire-response-error", "acquire-response-null", "release-response-error",
    "release-response-zero", "release-response-null", "ddl-error",
    "bootstrap-transaction-cancel",
}
ACQUIRE = {"acquire-response-error", "acquire-response-null"}
RELEASE = {"release-response-error", "release-response-zero", "release-response-null"}


class VerificationError(Exception):
    """The baseline or a negative control did not match its expected result."""


def checked(command, timeout=30):
    """Run a setup command without exposing its arguments in error messages."""
    try:
        result = subprocess.run(command, text=True, capture_output=True, timeout=timeout)
    except subprocess.TimeoutExpired as exc:
        raise VerificationError(f"{command[0]} {command[1]} timed out") from exc
    if result.returncode != 0:
        raise VerificationError(f"{command[0]} {command[1]} failed: {result.stderr.strip()}")
    return result.stdout.strip()


@contextlib.contextmanager
def isolated_mysql():
    """Start and remove only the container created by this invocation."""
    password = secrets.token_hex(16)
    name = "huixiang-migration-controls-" + uuid.uuid4().hex[:12]
    container = checked([
        "docker", "create", "--name", name, "--tmpfs", "/var/lib/mysql",
        "--publish", "127.0.0.1::3306", "--env", "MYSQL_ROOT_PASSWORD=" + password,
        "mysql:8.4",
    ], timeout=120)
    try:
        checked(["docker", "start", container])
        mapping = checked(["docker", "port", container, "3306/tcp"])
        match = re.fullmatch(r"127\.0\.0\.1:(\d+)", mapping)
        if match is None:
            raise VerificationError("unexpected temporary MySQL port mapping")
        deadline = time.monotonic() + 60
        while True:
            try:
                version = checked([
                    "docker", "exec", container, "sh", "-c",
                    'MYSQL_PWD="$MYSQL_ROOT_PASSWORD" mysql --protocol=TCP '
                    '-h127.0.0.1 -uroot -Nse "SELECT VERSION()"',
                ], timeout=3)
                break
            except VerificationError:
                if time.monotonic() >= deadline:
                    raise VerificationError("temporary MySQL did not become ready")
                time.sleep(1)
        yield {
            "dsn": f"root:{password}@tcp(127.0.0.1:{match.group(1)})/?parseTime=true",
            "version": version,
            "image_id": checked(["docker", "inspect", "--format", "{{.Image}}", container]),
        }
    finally:
        checked(["docker", "rm", "-f", container])


def replace_once(body, old, new):
    """Refuse source drift rather than silently testing a missing mutation."""
    if body.count(old) != 1:
        raise VerificationError("source anchor changed; review the control before rerunning")
    return body.replace(old, new)


def variants(source, tests):
    """Build overlays and the exact assertions that must reject each control."""
    without_discard, count = re.subn(
        r"(?m)^[ \t]*_ = conn\.Raw\(func\(any\) error \{ return driver\.ErrBadConn \}\)\n",
        "", source,
    )
    if count != 3:
        raise VerificationError("expected exactly three physical-discard sites")
    without_discard = replace_once(without_discard, '\t"database/sql/driver"\n', "")
    without_injection = replace_once(
        tests,
        "cfg.InterpolateParams = fault == faultAcquireError || fault == faultAcquireNull",
        "cfg.InterpolateParams = false",
    )
    no_retry = replace_once(source, '\t"strings"\n', '\t"strings"\n\t"sync"\n\t"sync/atomic"\n')
    header = "func ApplyDirectory(ctx context.Context, db *sql.DB, directory string) (returnErr error) {"
    no_retry = replace_once(
        no_retry, header,
        "var faultControlAttempts sync.Map\n\n" + header +
        "\n\tcontrolCount, _ := faultControlAttempts.LoadOrStore(db, new(atomic.Int64))" +
        "\n\tif controlCount.(*atomic.Int64).Add(1) > 1 { return nil }",
    )
    return [
        ("baseline", {}, {}),
        ("omit-physical-discard", {SOURCE: without_discard}, {
            case: "ambiguous locked session returned to pool" for case in ACQUIRE | RELEASE
        }),
        ("bypass-acquire-injection", {TEST_SOURCE: without_injection}, {
            case: "fault was not exercised" for case in ACQUIRE
        }),
        ("skip-retry-work", {SOURCE: no_retry}, {
            case: "pool not reusable after successful retry" if case in RELEASE else "history after retry"
            for case in CASES
        }),
    ]


def validate_events(stdout, stderr, returncode, expected):
    """Require all seven outcomes and expected assertion diagnostics, not just failure."""
    outcomes, output = {}, {}
    parent, package = [], []
    if any(marker in stdout + stderr for marker in (
        "WARNING: DATA RACE", "panic:", "fatal error:", "drop owned test schema:",
    )):
        raise VerificationError("race, panic, or cleanup error is not an expected assertion failure")
    for line in stdout.splitlines():
        try:
            event = json.loads(line)
        except json.JSONDecodeError as exc:
            raise VerificationError("go test produced non-JSON output") from exc
        action, test = event.get("Action"), event.get("Test", "")
        if action == "skip" or action == "build-fail":
            raise VerificationError("skipped tests or build failures cannot satisfy a control")
        if action == "output" and test.startswith(TEST + "/"):
            case = test[len(TEST) + 1:]
            output[case] = output.get(case, "") + event.get("Output", "")
        if action not in {"pass", "fail"}:
            continue
        if test.startswith(TEST + "/"):
            case = test[len(TEST) + 1:]
            if case in outcomes:
                raise VerificationError("duplicate subtest outcome")
            outcomes[case] = action
        elif test == TEST:
            parent.append(action)
        elif not test:
            package.append(action)
        else:
            raise VerificationError("unexpected test ran outside the selected fault matrix")
    wanted = {case: "fail" if case in expected else "pass" for case in CASES}
    final = "fail" if expected else "pass"
    if outcomes != wanted or parent != [final] or package != [final]:
        raise VerificationError(f"unexpected test outcomes: {outcomes}")
    if returncode != (1 if expected else 0):
        raise VerificationError(f"unexpected go test exit code: {returncode}")
    for case, diagnostic in expected.items():
        if diagnostic not in output.get(case, ""):
            raise VerificationError(f"{case} failed without its expected assertion")
    return {"passed": sorted(CASES - expected.keys()), "expected_failures": sorted(expected)}


def run_variant(name, replacements, expected, scratch, env, output_dir):
    """Run one baseline/control and retain complete event logs when requested."""
    overlay = {}
    for index, (original, body) in enumerate(replacements.items()):
        path = scratch / f"{name}-{index}.go"
        path.write_text(body, encoding="utf-8")
        overlay[str(original)] = str(path)
    config = scratch / f"{name}.json"
    config.write_text(json.dumps({"Replace": overlay}), encoding="utf-8")
    command = [
        "go", "test", "-json", "-race", "-count=1", "-timeout=30s",
        "-overlay", str(config), "./internal/platform/migrate", "-run", "^" + TEST + "$",
    ]
    try:
        result = subprocess.run(command, cwd=REPO, env=env, text=True, capture_output=True, timeout=120)
    except subprocess.TimeoutExpired as exc:
        raise VerificationError(f"{name}: go test timed out") from exc
    if output_dir:
        (output_dir / f"{name}.jsonl").write_text(result.stdout, encoding="utf-8")
        (output_dir / f"{name}.stderr.log").write_text(result.stderr, encoding="utf-8")
    try:
        outcome = validate_events(result.stdout, result.stderr, result.returncode, expected)
    except VerificationError as exc:
        if not output_dir:
            print(result.stdout[-12000:], file=sys.stderr)
            print(result.stderr[-2000:], file=sys.stderr)
        raise VerificationError(f"{name}: {exc}") from exc
    print(f"{name}: passed={len(outcome['passed'])}, expected_failures={len(outcome['expected_failures'])}", flush=True)
    return {"name": name, "exit_code": result.returncode, **outcome}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output-dir", type=Path, help="new directory for event logs and summary; must not exist")
    args = parser.parse_args()
    output_dir = args.output_dir.resolve() if args.output_dir else None
    if output_dir:
        try:
            output_dir.mkdir(parents=True, exist_ok=False)
        except FileExistsError:
            parser.error("--output-dir already exists; choose a new directory")
    summary = {"status": "running", "variants": []}
    try:
        sources = {SOURCE: SOURCE.read_bytes(), TEST_SOURCE: TEST_SOURCE.read_bytes()}
        controls = variants(*(body.decode("utf-8") for body in sources.values()))
        summary["source_sha256"] = {
            str(path.relative_to(REPO)): hashlib.sha256(body).hexdigest() for path, body in sources.items()
        }
        summary["go_version"] = checked(["go", "version"])
        with tempfile.TemporaryDirectory(prefix="huixiang-fault-controls-") as temporary:
            with isolated_mysql() as mysql:
                summary["mysql_version"], summary["mysql_image_id"] = mysql["version"], mysql["image_id"]
                env = os.environ.copy()
                env.update({
                    "LOCAL_LIFE_INTEGRATION": "1", "LOCAL_LIFE_MIGRATION_TEST_DSN": mysql["dsn"],
                    "GOFLAGS": "", "GOWORK": "off",
                })
                # Child-only build settings prevent inherited overlays/skip flags
                # from changing the verified matrix. User configuration is not edited.
                for name, replacements, expected in controls:
                    summary["variants"].append(run_variant(
                        name, replacements, expected, Path(temporary), env, output_dir,
                    ))
        if any(path.read_bytes() != body for path, body in sources.items()):
            raise VerificationError("on-disk source changed while controls were running")
        summary["status"] = "passed"
        print("All controls matched; temporary MySQL and overlays removed; repository source unchanged.")
    except (VerificationError, OSError, KeyboardInterrupt) as exc:
        summary["status"], summary["error"] = "failed", str(exc) or "interrupted"
        print("Verification failed: " + summary["error"], file=sys.stderr)
    finally:
        if output_dir:
            (output_dir / "summary.json").write_text(json.dumps(summary, indent=2) + "\n", encoding="utf-8")
            print("Evidence: " + str(output_dir), flush=True)
    return 0 if summary["status"] == "passed" else 1


if __name__ == "__main__":
    def interrupted(_signum, _frame):
        raise KeyboardInterrupt

    signal.signal(signal.SIGTERM, interrupted)
    sys.exit(main())
