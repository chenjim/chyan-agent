"""模型上下文窗口解析。

分层策略（先命中先用）：
1. 配置显式指定（``context_limit`` 为整数或数字字符串）
2. 联网模型目录 models.dev（可选，带本地缓存；离线时自动降级）
3. 内置模型窗口表（JSON 文件，具体模型名精确映射，见 ``model_windows.json``）
4. 兜底默认窗口 ``default_limit``

内置表默认从 ``chyan_agent/model_windows.json`` 加载（可用 ``context.window_table`` 指向自定义文件），
按**具体模型名**整理、对日期/版本后缀做前缀容错；需要最新值时开启 ``online_catalog``，目录优先于内置表。

联网目录结构（models.dev/api.json）::

    {"<provider>": {"models": {"<provider>/<model>": {"limit": {"context": N}, ...}}}}
"""
import json
import logging
import time
import urllib.request
from functools import lru_cache
from pathlib import Path
from typing import Optional

logger = logging.getLogger("chyan-agent")

DEFAULT_FALLBACK = 131072
DEFAULT_CATALOG_URL = "https://models.dev/api.json"
DEFAULT_CATALOG_CACHE = "./data/model_catalog.json"
DEFAULT_CATALOG_TTL_HOURS = 168

# 内置窗口表文件（JSON）。结构：{"windows": {"<model>": <tokens>}}，也兼容平铺字典。
DEFAULT_WINDOW_TABLE = str(Path(__file__).with_name("model_windows.json"))


def _read_window_table(path: str) -> dict[str, int]:
    try:
        raw = json.loads(Path(path).read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as e:
        logger.warning(f"模型窗口表不可用 ({path}): {e}")
        return {}
    windows = raw.get("windows", raw) if isinstance(raw, dict) else {}
    if not isinstance(windows, dict):
        logger.warning(f"模型窗口表格式异常 ({path})")
        return {}
    return {
        str(k).strip().lower(): int(v)
        for k, v in windows.items()
        if isinstance(v, int) and not isinstance(v, bool) and v > 0
    }


@lru_cache(maxsize=8)
def load_window_table(path: str = DEFAULT_WINDOW_TABLE) -> dict[str, int]:
    """加载（并缓存）模型窗口表；缺失/损坏时返回空表，由兜底默认接管。"""
    return _read_window_table(path)


BUILTIN_WINDOWS = load_window_table()


def _segment_match(pattern: str, model: str) -> bool:
    """pattern 是否出现在 model 的某个段起始处。"""
    idx = model.find(pattern)
    while idx != -1:
        if idx == 0 or not model[idx - 1].isalnum():
            return True
        idx = model.find(pattern, idx + 1)
    return False


def match_builtin(model: str, windows: Optional[dict] = None) -> Optional[int]:
    """按具体模型名匹配窗口表（默认内置表）。"""
    table = BUILTIN_WINDOWS if windows is None else windows
    name = (model or "").strip().lower()
    if not name or not table:
        return None
    if name in table:
        return table[name]

    short = name.split("/")[-1]
    if short in table:
        return table[short]

    # 版本/日期后缀容错：取最长的、作为前缀命中的具体模型名
    best_key = None
    for key in table:
        if short.startswith(key) and (best_key is None or len(key) > len(best_key)):
            best_key = key
    return table[best_key] if best_key else None


def _http_get_json(url: str, proxy: str = "", timeout: int = 20) -> dict:
    handlers = []
    if proxy:
        handlers.append(urllib.request.ProxyHandler({"http": proxy, "https": proxy}))
    opener = urllib.request.build_opener(*handlers)
    req = urllib.request.Request(url, headers={"User-Agent": "chyan-agent/1.0"})
    with opener.open(req, timeout=timeout) as resp:
        return json.loads(resp.read().decode("utf-8"))


def _distill(data: dict) -> dict:
    """把 models.dev 原始数据压缩为 {models: {id: ctx}, families: {family: ctx}}。

    同名模型跨 provider 时取较小窗口，宁可早压缩也不越界。
    """
    models: dict[str, int] = {}
    families: dict[str, int] = {}

    def _keep(bucket: dict[str, int], key: str, ctx: int):
        if key:
            bucket[key] = min(bucket.get(key, ctx), ctx)

    for provider in (data or {}).values():
        if not isinstance(provider, dict):
            continue
        for model_id, info in (provider.get("models") or {}).items():
            if not isinstance(info, dict):
                continue
            ctx = (info.get("limit") or {}).get("context")
            if not isinstance(ctx, int) or ctx <= 0:
                continue
            _keep(models, str(model_id).split("/")[-1].lower(), ctx)
            _keep(families, str(info.get("family") or "").lower(), ctx)

    return {"models": models, "families": families, "fetched_at": time.time()}


def _load_cache(path: str) -> Optional[dict]:
    p = Path(path)
    if not p.exists():
        return None
    try:
        return json.loads(p.read_text(encoding="utf-8"))
    except (json.JSONDecodeError, OSError) as e:
        logger.warning(f"模型目录缓存不可用: {e}")
        return None


def _save_cache(path: str, catalog: dict):
    p = Path(path)
    try:
        p.parent.mkdir(parents=True, exist_ok=True)
        p.write_text(json.dumps(catalog, ensure_ascii=False), encoding="utf-8")
    except OSError as e:
        logger.warning(f"写入模型目录缓存失败: {e}")


def _is_expired(catalog: dict, ttl_hours: float) -> bool:
    ts = catalog.get("fetched_at")
    if not isinstance(ts, (int, float)):
        return True
    return (time.time() - ts) > ttl_hours * 3600


def fetch_catalog(
    url: str = DEFAULT_CATALOG_URL,
    proxy: str = "",
    cache_path: str = DEFAULT_CATALOG_CACHE,
    ttl_hours: float = DEFAULT_CATALOG_TTL_HOURS,
) -> dict:
    """获取（带缓存的）模型目录；失败时返回过期缓存或空字典，不抛异常。"""
    cache = _load_cache(cache_path)
    if cache and not _is_expired(cache, ttl_hours):
        return cache
    try:
        catalog = _distill(_http_get_json(url, proxy))
        _save_cache(cache_path, catalog)
        logger.info(f"模型目录已更新: {len(catalog['models'])} 个模型 ({url})")
        return catalog
    except Exception as e:
        logger.warning(f"获取模型目录失败，降级使用缓存/兜底: {e}")
        return cache or {}


def match_catalog(model: str, catalog: dict) -> Optional[int]:
    name = (model or "").strip().lower()
    if not name or not catalog:
        return None

    models = catalog.get("models") or {}
    key = name.split("/")[-1]
    if name in models:
        return models[name]
    if key in models:
        return models[key]

    candidates = [(k, v) for k, v in models.items() if k and (k in name or name in k)]
    if candidates:
        candidates.sort(key=lambda kv: len(kv[0]), reverse=True)
        return candidates[0][1]

    for family, ctx in (catalog.get("families") or {}).items():
        if family and _segment_match(family, name):
            return ctx
    return None


def resolve_context_limit(model: str, context_config) -> tuple[int, str]:
    """返回 (窗口 tokens, 来源)。来源取值: config/online/builtin/fallback。"""
    raw = getattr(context_config, "context_limit", "auto")
    if isinstance(raw, int) and not isinstance(raw, bool) and raw > 0:
        return raw, "config"
    if isinstance(raw, str) and raw.strip().isdigit():
        return int(raw.strip()), "config"

    # 联网目录优先于内置表：内置表是保守近似，目录才是精确值
    if getattr(context_config, "online_catalog", False):
        catalog = fetch_catalog(
            getattr(context_config, "catalog_url", DEFAULT_CATALOG_URL),
            getattr(context_config, "catalog_proxy", ""),
            getattr(context_config, "catalog_cache", DEFAULT_CATALOG_CACHE),
            getattr(context_config, "catalog_ttl_hours", DEFAULT_CATALOG_TTL_HOURS),
        )
        window = match_catalog(model, catalog)
        if window:
            return window, "online"

    table = None
    custom_table = getattr(context_config, "window_table", "") or ""
    if custom_table:
        table = load_window_table(str(custom_table))
    window = match_builtin(model, table)
    if window:
        return window, "builtin"

    fallback = getattr(context_config, "default_limit", DEFAULT_FALLBACK)
    return fallback, "fallback"
