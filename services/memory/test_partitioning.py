"""Phase 8 Task 17 — verify Mem0 tenant prefix partitioning.

The MemoryBackend keys every Mem0 operation by ``tenant_prefix(tenant_id)``
so two tenants with overlapping user_ids cannot read each other's
memories. These tests use a fake Mem0 client to assert the keys
actually carry the prefix.
"""

from __future__ import annotations

import os
import sys

import pytest

_THIS_DIR = os.path.dirname(os.path.abspath(__file__))
_SERVICES_DIR = os.path.dirname(_THIS_DIR)
for p in (_SERVICES_DIR, _THIS_DIR):
    if p not in sys.path:
        sys.path.insert(0, p)

import memory_server as srv  # noqa: E402


class _FakeMem0:
    """Records add/search calls and namespaces by user_id, so tests
    can verify the prefix actually scopes lookups."""

    def __init__(self):
        # user_id -> [{"id", "memory", "metadata", "tenant_id"}]
        self._by_user: dict[str, list[dict]] = {}
        self._next_id = 1

    def add(self, content, user_id, metadata):
        rid = f"m-{self._next_id}"
        self._next_id += 1
        rec = {"id": rid, "memory": content, "metadata": dict(metadata)}
        self._by_user.setdefault(user_id, []).append(rec)
        return {"id": rid}

    def search(self, query, user_id, limit):  # noqa: ARG002
        # Fake search just returns everything stored under that user_id.
        return list(self._by_user.get(user_id, []))[:limit]


def _backend_with_fake() -> tuple[srv.MemoryBackend, _FakeMem0]:
    backend = srv.MemoryBackend()
    fake = _FakeMem0()
    backend._mem = fake  # type: ignore[attr-defined]
    return backend, fake


def test_tenant_prefix_is_resolved_from_template():
    assert srv.tenant_prefix("tenantA") == "tenantA"


def test_tenant_prefix_template_can_be_overridden(monkeypatch: pytest.MonkeyPatch):
    monkeypatch.setattr(srv, "_TENANT_PREFIX_TEMPLATE", "prod-{tenant_id}")
    assert srv.tenant_prefix("acme") == "prod-acme"


def test_tenant_prefix_required():
    with pytest.raises(ValueError):
        srv.tenant_prefix("")


def test_two_tenants_with_same_user_id_are_isolated():
    backend, fake = _backend_with_fake()

    backend.write("tenantA", user_id="alice", session_id="s1", content="A-secret", metadata={})
    backend.write("tenantB", user_id="alice", session_id="s1", content="B-secret", metadata={})

    a_results = backend.search("tenantA", user_id="alice", session_id="s1", query="secret", top_k=10)
    b_results = backend.search("tenantB", user_id="alice", session_id="s1", query="secret", top_k=10)

    a_texts = [r["memory"] for r in a_results]
    b_texts = [r["memory"] for r in b_results]
    assert a_texts == ["A-secret"]
    assert b_texts == ["B-secret"]
    # Mem0 keys are explicitly tenant-prefixed.
    assert "tenantA:alice" in fake._by_user
    assert "tenantB:alice" in fake._by_user


def test_metadata_records_tenant_id_and_prefix():
    backend, fake = _backend_with_fake()
    backend.write("tenantA", user_id="alice", session_id="s1", content="hi", metadata={"k": "v"})
    rec = fake._by_user["tenantA:alice"][0]
    assert rec["metadata"]["tenant_id"] == "tenantA"
    assert rec["metadata"]["tenant_prefix"] == "tenantA"
    assert rec["metadata"]["k"] == "v"


def test_search_drops_stray_hits_whose_metadata_tenant_mismatches():
    """Defence-in-depth: even if Mem0 ignored user_id and returned
    cross-tenant rows, the server filters them out by metadata."""
    backend, fake = _backend_with_fake()

    # Hand-inject a row under tenantA's namespace whose metadata says
    # tenantB — the search must drop it.
    fake._by_user["tenantA:alice"] = [
        {"id": "leak", "memory": "should-not-leak", "metadata": {"tenant_id": "tenantB"}},
        {"id": "ok", "memory": "fine", "metadata": {"tenant_id": "tenantA"}},
    ]
    out = backend.search("tenantA", user_id="alice", session_id="s1", query="x", top_k=10)
    assert [r["id"] for r in out] == ["ok"]
