#!/usr/bin/env bash
# Round-13 Task 16: developer prerequisite checker.
#
# Walks the list of tools/runtimes a contributor needs to run
# make build / make test / make eval / make test-integration
# and prints a green/red checklist. Exits 0 when every required
# item passes; exits 1 otherwise so CI can gate.
set -uo pipefail

PASS="\033[32m[OK]\033[0m"
FAIL="\033[31m[!!]\033[0m"
WARN="\033[33m[~~]\033[0m"

fail_count=0
warn_count=0

check_pass() { printf "  ${PASS} %s\n" "$1"; }
check_warn() { printf "  ${WARN} %s\n" "$1"; warn_count=$((warn_count+1)); }
check_fail() { printf "  ${FAIL} %s\n" "$1"; fail_count=$((fail_count+1)); }

echo "==> hunting-fishball doctor"
echo

# Go >= 1.25
if command -v go >/dev/null 2>&1; then
    raw=$(go version | awk '{print $3}' | sed 's/^go//')
    major=$(echo "$raw" | cut -d. -f1)
    minor=$(echo "$raw" | cut -d. -f2)
    if [ "$major" -gt 1 ] || { [ "$major" -eq 1 ] && [ "$minor" -ge 25 ]; }; then
        check_pass "Go $raw (>= 1.25)"
    else
        check_fail "Go $raw — need >= 1.25"
    fi
else
    check_fail "Go not installed"
fi

# Docker daemon
if command -v docker >/dev/null 2>&1; then
    if docker info >/dev/null 2>&1; then
        check_pass "Docker running"
    else
        check_warn "Docker installed but daemon not reachable (some e2e tests will skip)"
    fi
else
    check_warn "Docker not installed (e2e + migrate-dry-run-pg unavailable)"
fi

# docker-compose / docker compose
if command -v docker-compose >/dev/null 2>&1; then
    check_pass "docker-compose binary"
elif docker compose version >/dev/null 2>&1; then
    check_pass "docker compose plugin"
else
    check_warn "docker compose not available"
fi

# Python 3.11+
if command -v python3 >/dev/null 2>&1; then
    pyraw=$(python3 -c 'import sys; print("%d.%d" % sys.version_info[:2])')
    pymajor=$(echo "$pyraw" | cut -d. -f1)
    pyminor=$(echo "$pyraw" | cut -d. -f2)
    if [ "$pymajor" -ge 3 ] && [ "$pyminor" -ge 11 ]; then
        check_pass "Python $pyraw (>= 3.11)"
    else
        check_fail "Python $pyraw — need >= 3.11"
    fi
else
    check_fail "python3 not installed"
fi

# protoc
if command -v protoc >/dev/null 2>&1; then
    pcv=$(protoc --version 2>/dev/null)
    check_pass "$pcv"
else
    check_warn "protoc not installed (make proto-gen will fail)"
fi

# golangci-lint
if command -v golangci-lint >/dev/null 2>&1; then
    glv=$(golangci-lint --version 2>/dev/null | head -1)
    check_pass "$glv"
else
    check_warn "golangci-lint not installed (make lint will skip)"
fi

# Required env vars for e2e (warn only).
for v in CONTEXT_ENGINE_DATABASE_URL CONTEXT_ENGINE_REDIS_URL; do
    if [ -n "${!v:-}" ]; then
        check_pass "$v set"
    else
        check_warn "$v not set (only required for e2e)"
    fi
done

echo
echo "Summary: $fail_count fail / $warn_count warn"
if [ "$fail_count" -gt 0 ]; then
    exit 1
fi
exit 0
