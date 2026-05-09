"""Pytest config: make `services/_proto` and the per-service dir
importable so tests work from any cwd."""

from __future__ import annotations

import os
import sys

_THIS_DIR = os.path.dirname(os.path.abspath(__file__))
_SERVICES_DIR = os.path.dirname(_THIS_DIR)
for p in (_SERVICES_DIR, _THIS_DIR):
    if p not in sys.path:
        sys.path.insert(0, p)
