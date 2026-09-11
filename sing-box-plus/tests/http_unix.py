"""tests.http_unix 是 scripts/http_unix.py 的再导出 shim。

README §6 的交付物表把 HTTP/UDS 传输层列在 tests/ 下，并要求 scripts/ 侧的客户端与它一致。
本项目不复制第二份实现——复制出来的两份只能靠门禁去追，而单实现 + shim 让漂移无从发生。

按文件路径显式装载而不是 `from http_unix import *`：两个文件同名，
后者在 tests/ 优先于 scripts/ 的搜索顺序下会变成自我导入。
"""

from __future__ import annotations

import importlib.util
import sys
from pathlib import Path

_IMPLEMENTATION = Path(__file__).resolve().parent.parent / "scripts" / "http_unix.py"
_MODULE_NAME = "sing_box_plus_http_unix"

if _MODULE_NAME in sys.modules:
    _module = sys.modules[_MODULE_NAME]
else:
    _spec = importlib.util.spec_from_file_location(_MODULE_NAME, _IMPLEMENTATION)
    if _spec is None or _spec.loader is None:
        raise ImportError(f"无法装载 HTTP/UDS 实现：{_IMPLEMENTATION}")
    _module = importlib.util.module_from_spec(_spec)
    sys.modules[_MODULE_NAME] = _module
    _spec.loader.exec_module(_module)

DEFAULT_TIMEOUT = _module.DEFAULT_TIMEOUT
MAX_BODY_BYTES = _module.MAX_BODY_BYTES
HTTPUnixError = _module.HTTPUnixError
Response = _module.Response
request = _module.request
IMPLEMENTATION_PATH = _IMPLEMENTATION

__all__ = [
    "DEFAULT_TIMEOUT",
    "MAX_BODY_BYTES",
    "HTTPUnixError",
    "Response",
    "request",
    "IMPLEMENTATION_PATH",
]
