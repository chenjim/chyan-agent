import html as html_mod
import json
import logging
import re
import subprocess
import urllib.parse
import urllib.request
from pathlib import Path

logger = logging.getLogger("chyan-agent")

SENSITIVE_FILES = {"config.toml", "config.example.toml", ".env", ".env.local"}
EXCLUDE_DIRS = {".venv", "venv", ".git", "__pycache__", "node_modules", ".idea", ".vscode"}
MAX_SEARCH_RESULTS = 30
MAX_GREP_RESULTS = 10

TODO_STATUSES = {"pending", "in_progress", "completed"}

# bash 危险命令拦截（保守黑名单，命中即拒绝）
DANGEROUS_PATTERNS = [
    r"\brm\s+(-[a-z]*\s+)*-[a-z]*r[a-z]*f?\s+/(?:\s|$|\*)",
    r"\bmkfs(\.\w+)?\b",
    r"\bdd\s+.*\bof=/dev/",
    r":\(\)\s*\{.*\}\s*;?\s*:",
    r"\b(shutdown|reboot|halt|poweroff)\b",
    r"\bsudo\b",
    r">\s*/dev/(sd|nvme|hd)",
    r"\bchmod\s+-R\s+777\s+/(?:\s|$)",
    r"(curl|wget)[^|;]*\|\s*(?:ba|z|d)?sh\b",
]

DEFAULT_TOOL_CONFIG = {
    "read_max_lines": 500,
    "read_truncate_mode": "auto",
    # 单个工具结果统一上限（字节），超出则截断为 preview
    "tool_output_max_bytes": 20000,
    # 文件操作/命令的工作目录根；空字符串表示不限制
    "work_dir": "",
    "bash_enabled": False,
    "bash_timeout": 30,
    "bash_workdir": "",
    "bash_max_output_bytes": 20000,
    "web_enabled": False,
    "web_proxy": "",
    "web_max_bytes": 20000,
    "web_search_url": "https://html.duckduckgo.com/html/?q={query}",
}


class ToolExecutor:
    def __init__(self, memory_manager, tool_config=None):
        self.memory_manager = memory_manager
        self.tool_config = dict(DEFAULT_TOOL_CONFIG)
        if tool_config:
            self.update_config(tool_config)
        self.current_turn = 0
        self.todos = []

    def update_config(self, config: dict):
        """增量合并配置，未提供的键保留默认/现值。"""
        for key, value in config.items():
            if key in DEFAULT_TOOL_CONFIG:
                self.tool_config[key] = value

    # ------------------------------------------------------------------ schema

    def get_schemas(self):
        tools = [
            {
                "name": "read",
                "description": "读取文件内容。默认整文件读取并智能截断；可用 offset/limit 读取指定行范围（从 1 开始计数）。",
                "parameters": {
                    "type": "object",
                    "properties": {
                        "path": {"type": "string", "description": "文件路径"},
                        "offset": {"type": "integer", "description": "起始行号（从 1 开始），默认 1"},
                        "limit": {"type": "integer", "description": "读取的最大行数，默认到文件末尾"},
                    },
                    "required": ["path"],
                },
            },
            {
                "name": "write",
                "description": "写入或追加内容到文件（整体覆盖）。修改已有文件请优先用 edit。禁止覆盖 config.toml、.env 等敏感文件。",
                "parameters": {
                    "type": "object",
                    "properties": {
                        "path": {"type": "string", "description": "文件路径"},
                        "content": {"type": "string", "description": "要写入的内容"},
                        "mode": {
                            "type": "string",
                            "enum": ["write", "append"],
                            "description": "写入模式：write 覆盖，append 追加",
                        },
                    },
                    "required": ["path", "content"],
                },
            },
            {
                "name": "edit",
                "description": "对文件做精确字符串替换。old_string 必须在文件中唯一（否则用 replace_all 或提供更多上下文）。修改已有文件首选此工具。",
                "parameters": {
                    "type": "object",
                    "properties": {
                        "path": {"type": "string", "description": "文件路径"},
                        "old_string": {"type": "string", "description": "要被替换的原文（含足够上下文以保证唯一）"},
                        "new_string": {"type": "string", "description": "替换后的新文本"},
                        "replace_all": {"type": "boolean", "description": "是否替换所有匹配，默认 false"},
                    },
                    "required": ["path", "old_string", "new_string"],
                },
            },
            {
                "name": "list",
                "description": "列出目录内容（名称/类型/大小），自动跳过隐藏文件与 .git 等目录。",
                "parameters": {
                    "type": "object",
                    "properties": {
                        "path": {"type": "string", "description": "目录路径，默认当前目录"},
                        "depth": {"type": "integer", "description": "递归深度，默认 1（仅当前层）"},
                    },
                    "required": [],
                },
            },
            {
                "name": "search",
                "description": "在目录中按文件名 glob 模式查找文件。支持 * 和 ** 通配符。",
                "parameters": {
                    "type": "object",
                    "properties": {
                        "path": {"type": "string", "description": "搜索起始目录"},
                        "pattern": {"type": "string", "description": "glob 匹配模式，如 '*.py'、'**/*.json'"},
                    },
                    "required": ["path", "pattern"],
                },
            },
            {
                "name": "grep",
                "description": "在文件或目录中按正则表达式搜索内容。",
                "parameters": {
                    "type": "object",
                    "properties": {
                        "path": {"type": "string", "description": "文件或目录路径"},
                        "pattern": {"type": "string", "description": "正则表达式"},
                        "include": {
                            "type": "string",
                            "description": "文件 glob 过滤（如 '*.py'、'*.{ts,js}'）",
                        },
                    },
                    "required": ["path", "pattern"],
                },
            },
            {
                "name": "delete",
                "description": "删除文件或空目录。无法删除非空目录。",
                "parameters": {
                    "type": "object",
                    "properties": {
                        "path": {"type": "string", "description": "要删除的文件或空目录路径"},
                    },
                    "required": ["path"],
                },
            },
            {
                "name": "remember",
                "description": "把一条事实写入长期记忆。当用户提到需要记住的信息时使用。",
                "parameters": {
                    "type": "object",
                    "properties": {
                        "content": {"type": "string", "description": "要记住的事实内容"},
                    },
                    "required": ["content"],
                },
            },
            {
                "name": "recall",
                "description": "从长期记忆中召回相关内容。使用自然语言描述你想找的信息，系统会自动用 BM25 算法匹配最相关的记忆。",
                "parameters": {
                    "type": "object",
                    "properties": {
                        "query": {"type": "string", "description": "要查找的内容关键词或短语"},
                        "top_k": {
                            "type": "integer",
                            "description": "返回最相关的 N 条记忆",
                        },
                    },
                    "required": ["query"],
                },
            },
            {
                "name": "todo_write",
                "description": "写入/更新当前任务清单（整表覆盖）。用于多步任务的进度跟踪。",
                "parameters": {
                    "type": "object",
                    "properties": {
                        "todos": {
                            "type": "array",
                            "description": "完整任务列表",
                            "items": {
                                "type": "object",
                                "properties": {
                                    "content": {"type": "string", "description": "任务描述"},
                                    "status": {
                                        "type": "string",
                                        "enum": ["pending", "in_progress", "completed"],
                                    },
                                },
                                "required": ["content", "status"],
                            },
                        },
                    },
                    "required": ["todos"],
                },
            },
            {
                "name": "todo_read",
                "description": "读取当前任务清单。",
                "parameters": {"type": "object", "properties": {}, "required": []},
            },
        ]

        if self.tool_config.get("bash_enabled"):
            tools.append({
                "name": "bash",
                "description": "在受限 shell 中执行命令（工作目录受限、有超时与输出上限）。用于运行测试、git、构建等。",
                "parameters": {
                    "type": "object",
                    "properties": {
                        "command": {"type": "string", "description": "要执行的 shell 命令"},
                        "cwd": {"type": "string", "description": "工作目录，默认 bash_workdir/work_dir"},
                        "timeout": {"type": "integer", "description": "超时秒数，默认配置值"},
                    },
                    "required": ["command"],
                },
            })

        if self.tool_config.get("web_enabled"):
            tools.extend([
                {
                    "name": "web_fetch",
                    "description": "抓取一个 URL 并返回纯文本内容（自动去 HTML 标签，带大小上限）。",
                    "parameters": {
                        "type": "object",
                        "properties": {
                            "url": {"type": "string", "description": "要抓取的完整 URL"},
                        },
                        "required": ["url"],
                    },
                },
                {
                    "name": "web_search",
                    "description": "联网搜索，返回标题/链接/摘要列表。",
                    "parameters": {
                        "type": "object",
                        "properties": {
                            "query": {"type": "string", "description": "搜索关键词"},
                            "top": {"type": "integer", "description": "返回条数，默认 5"},
                        },
                        "required": ["query"],
                    },
                },
            ])

        return [
            {
                "type": "function",
                "function": {
                    "name": t["name"],
                    "description": t["description"],
                    "parameters": t["parameters"],
                },
            }
            for t in tools
        ]

    # ------------------------------------------------------------------ helpers

    def _resolve(self, path: str) -> Path:
        p = Path(path).resolve()
        root = self.tool_config.get("work_dir") or ""
        if root:
            rootp = Path(root).resolve()
            if p != rootp and rootp not in p.parents:
                raise PermissionError(f"路径超出工作目录 {rootp}: {path}")
        return p

    @staticmethod
    def _check_sensitive(p: Path):
        if p.name in SENSITIVE_FILES:
            raise PermissionError(f"禁止操作敏感文件: {p.name}")

    def _cap_output(self, result: dict) -> dict:
        limit = int(self.tool_config.get("tool_output_max_bytes", 20000))
        try:
            text = json.dumps(result, ensure_ascii=False)
        except (TypeError, ValueError):
            text = str(result)
        if len(text.encode("utf-8")) <= limit:
            return result
        return {
            "success": bool(result.get("success", True)),
            "truncated_output": True,
            "note": f"工具输出超过 {limit} 字节，已截断",
            "preview": text[:limit],
        }

    @staticmethod
    def _truncate_text(text: str, max_bytes: int) -> tuple[str, bool]:
        data = text.encode("utf-8")
        if len(data) <= max_bytes:
            return text, False
        return data[:max_bytes].decode("utf-8", errors="ignore"), True

    # ------------------------------------------------------------------ file ops

    def _read(self, path: str, offset=None, limit=None) -> dict:
        p = self._resolve(path)
        if not p.exists():
            return {
                "success": False,
                "error": f"文件不存在: {path}",
                "suggestion": "使用 search 或 list 工具确认路径，或用 write 工具创建新文件",
            }
        if not p.is_file():
            return {"success": False, "error": f"路径不是文件: {path}"}

        try:
            lines = p.read_text(encoding="utf-8").splitlines(keepends=True)
        except Exception as e:
            return {"success": False, "error": f"读取文件失败: {e}"}

        total_lines = len(lines)

        if offset is not None or limit is not None:
            start = max(1, int(offset or 1))
            end = total_lines if limit is None else min(total_lines, start - 1 + int(limit))
            window = lines[start - 1:end]
            return {
                "success": True,
                "path": str(p),
                "total_lines": total_lines,
                "start_line": start,
                "end_line": start - 1 + len(window),
                "content": "".join(window),
                "truncated": end < total_lines or start > 1,
            }

        max_lines = self.tool_config.get("read_max_lines", 500)
        mode = self.tool_config.get("read_truncate_mode", "auto")
        truncated = total_lines > max_lines

        if not truncated:
            content = "".join(lines)
        elif mode == "head":
            content = "".join(lines[:max_lines]) + f"\n\n[截断：剩余 {total_lines - max_lines} 行]"
        elif mode == "tail":
            content = f"[截断：省略前 {total_lines - max_lines} 行]\n\n" + "".join(lines[-max_lines:])
        else:
            head_n = int(max_lines * 0.7)
            tail_n = max_lines - head_n
            content = (
                "".join(lines[:head_n])
                + f"\n\n[截断：省略中间 {total_lines - max_lines} 行]\n\n"
                + "".join(lines[-tail_n:])
            )

        return {
            "success": True,
            "path": str(p),
            "total_lines": total_lines,
            "returned_lines": min(total_lines, max_lines),
            "truncated": truncated,
            "content": content,
        }

    def _write(self, path: str, content: str, mode: str = "write") -> dict:
        p = self._resolve(path)
        self._check_sensitive(p)

        try:
            if mode == "append" and p.exists():
                with open(p, "a", encoding="utf-8") as f:
                    f.write(content)
            else:
                p.parent.mkdir(parents=True, exist_ok=True)
                with open(p, "w", encoding="utf-8") as f:
                    f.write(content)
        except Exception as e:
            return {"success": False, "error": f"写入文件失败: {e}"}

        return {"success": True, "path": str(p), "mode": mode, "size": len(content)}

    def _edit(self, path: str, old_string: str, new_string: str, replace_all: bool = False) -> dict:
        p = self._resolve(path)
        self._check_sensitive(p)
        if not p.is_file():
            return {"success": False, "error": f"文件不存在: {path}"}
        if old_string == "":
            return {
                "success": False,
                "error": "old_string 不能为空",
                "suggestion": "提供要替换的原文；新建文件请用 write",
            }

        try:
            text = p.read_text(encoding="utf-8")
        except Exception as e:
            return {"success": False, "error": f"读取文件失败: {e}"}

        count = text.count(old_string)
        if count == 0:
            return {
                "success": False,
                "error": "未找到 old_string",
                "suggestion": "先用 read 确认原文（注意缩进与换行），或提供更完整的上下文",
            }
        if count > 1 and not replace_all:
            return {
                "success": False,
                "error": f"old_string 匹配到 {count} 处，无法确定替换目标",
                "suggestion": "扩大 old_string 使其唯一，或设置 replace_all=true 全部替换",
            }

        replacements = count if replace_all else 1
        new_text = text.replace(old_string, new_string) if replace_all else text.replace(old_string, new_string, 1)
        try:
            p.write_text(new_text, encoding="utf-8")
        except Exception as e:
            return {"success": False, "error": f"写入文件失败: {e}"}

        return {"success": True, "path": str(p), "replacements": replacements}

    def _list(self, path: str = ".", depth: int = 1) -> dict:
        p = self._resolve(path)
        if not p.exists():
            return {"success": False, "error": f"路径不存在: {path}"}
        if not p.is_dir():
            return {"success": False, "error": f"不是目录: {path}"}

        depth = max(1, int(depth))
        entries = []

        def walk(d: Path, level: int):
            if len(entries) >= MAX_SEARCH_RESULTS:
                return
            try:
                children = sorted(d.iterdir(), key=lambda x: (not x.is_dir(), x.name.lower()))
            except (PermissionError, OSError):
                return
            for child in children:
                if child.name.startswith(".") or child.name in EXCLUDE_DIRS:
                    continue
                kind = "dir" if child.is_dir() else "file"
                item = {"path": str(child.relative_to(p)), "type": kind}
                if kind == "file":
                    try:
                        item["size"] = child.stat().st_size
                    except OSError:
                        pass
                entries.append(item)
                if len(entries) >= MAX_SEARCH_RESULTS:
                    return
                if kind == "dir" and level < depth:
                    walk(child, level + 1)

        walk(p, 1)
        return {
            "success": True,
            "path": str(p),
            "entries": entries,
            "count": len(entries),
            "truncated": len(entries) >= MAX_SEARCH_RESULTS,
        }

    def _search(self, path: str, pattern: str) -> dict:
        p = self._resolve(path)
        if not p.exists():
            return {"success": False, "error": f"目录不存在: {path}"}

        matching = []
        try:
            for f in p.rglob(pattern):
                try:
                    rel = f.relative_to(p)
                    if any(part.startswith(".") or part in EXCLUDE_DIRS for part in rel.parts):
                        continue
                    matching.append(str(rel))
                    if len(matching) >= MAX_SEARCH_RESULTS:
                        break
                except (ValueError, OSError):
                    continue
        except (PermissionError, OSError):
            pass

        return {
            "success": True,
            "path": str(p),
            "pattern": pattern,
            "results": matching,
            "count": len(matching),
            "truncated": len(matching) >= MAX_SEARCH_RESULTS,
        }

    def _grep(self, path: str, pattern: str, include: str = None) -> dict:
        p = self._resolve(path)
        if not p.exists():
            return {"success": False, "error": f"路径不存在: {path}"}

        matching_files = []
        if p.is_file():
            files_to_check = [p]
        else:
            files_to_check = []
            try:
                for fp in p.rglob(include or "*"):
                    try:
                        rel = fp.relative_to(p)
                        if any(part.startswith(".") or part in EXCLUDE_DIRS for part in rel.parts):
                            continue
                        files_to_check.append(fp)
                        if len(files_to_check) > MAX_GREP_RESULTS * 10:
                            break
                    except (ValueError, OSError):
                        continue
            except (PermissionError, OSError):
                pass

        for fp in files_to_check:
            if fp.is_dir() or fp.name.startswith("."):
                continue
            try:
                text = fp.read_text(encoding="utf-8", errors="ignore")
                matches = list(re.finditer(pattern, text, re.MULTILINE))
                if matches:
                    rel_path = str(fp.relative_to(p.parent) if p.is_dir() else fp)
                    lines_info = []
                    for m in matches[:20]:
                        line_num = text[:m.start()].count("\n") + 1
                        snippet = text[max(0, m.start() - 20):m.end() + 80].replace("\n", " ").strip()
                        lines_info.append({"line": line_num, "content": snippet})
                    matching_files.append({
                        "file": rel_path,
                        "matches": len(matches),
                        "lines": lines_info,
                    })
                    if len(matching_files) >= MAX_GREP_RESULTS:
                        break
            except Exception:
                continue

        return {
            "success": True,
            "path": str(p),
            "pattern": pattern,
            "results": matching_files,
            "count": len(matching_files),
            "truncated": len(matching_files) >= MAX_GREP_RESULTS,
        }

    def _delete(self, path: str) -> dict:
        p = self._resolve(path)
        if not p.exists():
            return {"success": False, "error": f"文件不存在: {path}"}
        self._check_sensitive(p)
        try:
            if p.is_dir():
                p.rmdir()
            else:
                p.unlink()
            return {"success": True, "path": str(p), "action": "deleted"}
        except OSError as e:
            return {"success": False, "error": f"删除失败: {e}"}

    # ------------------------------------------------------------------ memory

    def _remember(self, content: str) -> dict:
        mid = self.memory_manager.remember(content, self.current_turn)
        return {"success": True, "memory_id": mid, "content": content}

    def _recall(self, query: str, top_k: int = 5) -> dict:
        results = self.memory_manager.recall(query, top_k)
        return {
            "success": True,
            "query": query,
            "results": results,
            "count": len(results),
        }

    # ------------------------------------------------------------------ todo

    def _todo_write(self, todos) -> dict:
        if not isinstance(todos, list):
            return {"success": False, "error": "todos 必须是数组"}
        normalized = []
        for i, item in enumerate(todos):
            if not isinstance(item, dict) or not item.get("content"):
                return {"success": False, "error": f"第 {i + 1} 项缺少 content"}
            status = item.get("status", "pending")
            if status not in TODO_STATUSES:
                return {"success": False, "error": f"第 {i + 1} 项 status 非法: {status}"}
            normalized.append({"content": str(item["content"]), "status": status})
        self.todos = normalized
        return {"success": True, "todos": self.todos, "count": len(self.todos)}

    def _todo_read(self) -> dict:
        counts = {s: 0 for s in TODO_STATUSES}
        for t in self.todos:
            counts[t["status"]] = counts.get(t["status"], 0) + 1
        return {"success": True, "todos": self.todos, "count": len(self.todos), "summary": counts}

    # ------------------------------------------------------------------ bash

    def _bash(self, command: str, cwd: str = None, timeout: int = None) -> dict:
        if not self.tool_config.get("bash_enabled"):
            return {"success": False, "error": "bash 工具未启用（在 [tools] 设置 bash_enabled = true）"}
        if not command or not command.strip():
            return {"success": False, "error": "command 不能为空"}

        for pattern in DANGEROUS_PATTERNS:
            if re.search(pattern, command):
                return {
                    "success": False,
                    "error": "命令命中危险模式，已拒绝执行",
                    "suggestion": "拆分为更具体的命令，或去掉危险操作",
                }

        workdir = cwd or self.tool_config.get("bash_workdir") or self.tool_config.get("work_dir") or None
        if workdir:
            root = self.tool_config.get("work_dir") or workdir
            wd = Path(workdir).resolve()
            rootp = Path(root).resolve()
            if wd != rootp and rootp not in wd.parents:
                return {"success": False, "error": f"工作目录超出允许范围: {workdir}"}
            workdir = str(wd)

        timeout = int(timeout or self.tool_config.get("bash_timeout", 30))
        try:
            proc = subprocess.run(
                command,
                shell=True,
                cwd=workdir,
                timeout=timeout,
                capture_output=True,
                text=True,
            )
        except subprocess.TimeoutExpired:
            return {"success": False, "error": f"命令超时（>{timeout}s）", "command": command}
        except Exception as e:
            return {"success": False, "error": f"命令执行失败: {e}"}

        max_bytes = int(self.tool_config.get("bash_max_output_bytes", 20000))
        stdout, out_trunc = self._truncate_text(proc.stdout or "", max_bytes)
        stderr, err_trunc = self._truncate_text(proc.stderr or "", max_bytes)
        return {
            "success": proc.returncode == 0,
            "command": command,
            "exit_code": proc.returncode,
            "stdout": stdout,
            "stderr": stderr,
            "truncated": out_trunc or err_trunc,
        }

    # ------------------------------------------------------------------ web

    def _http_get(self, url: str) -> str:
        cfg = self.tool_config
        handlers = []
        proxy = cfg.get("web_proxy") or ""
        if proxy:
            handlers.append(urllib.request.ProxyHandler({"http": proxy, "https": proxy}))
        opener = urllib.request.build_opener(*handlers)
        req = urllib.request.Request(url, headers={"User-Agent": "Mozilla/5.0 chyan-agent"})
        with opener.open(req, timeout=20) as resp:
            raw = resp.read()
        max_bytes = int(cfg.get("web_max_bytes", 20000)) * 5
        text, _ = self._truncate_text(raw.decode("utf-8", errors="ignore"), max_bytes)
        return text

    def _web_fetch(self, url: str) -> dict:
        if not self.tool_config.get("web_enabled"):
            return {"success": False, "error": "web 工具未启用（在 [tools] 设置 web_enabled = true）"}
        try:
            html_text = self._http_get(url)
        except Exception as e:
            return {"success": False, "error": f"抓取失败: {e}"}
        text = _html_to_text(html_text)
        max_bytes = int(self.tool_config.get("web_max_bytes", 20000))
        content, truncated = self._truncate_text(text, max_bytes)
        return {"success": True, "url": url, "content": content, "truncated": truncated}

    def _web_search(self, query: str, top: int = 5) -> dict:
        if not self.tool_config.get("web_enabled"):
            return {"success": False, "error": "web 工具未启用（在 [tools] 设置 web_enabled = true）"}
        template = self.tool_config.get("web_search_url") or DEFAULT_TOOL_CONFIG["web_search_url"]
        url = template.format(query=urllib.parse.quote_plus(query))
        try:
            body = self._http_get(url)
        except Exception as e:
            return {"success": False, "error": f"搜索失败: {e}"}

        results = _parse_search_json(body)
        if results is None:
            results = _parse_search_results(body)
            if not results and _looks_like_challenge(body):
                return {
                    "success": False,
                    "error": "搜索被反爬拦截（返回人机校验页）",
                    "suggestion": "在 [tools] 将 web_search_url 配置为 SearXNG JSON 接口"
                                  "（如 https://searx.example/search?q={query}&format=json）",
                }
        results = results[: max(1, int(top))]
        return {"success": True, "query": query, "results": results, "count": len(results)}

    # ------------------------------------------------------------------ dispatch

    def execute(self, name: str, **kwargs) -> dict:
        logger.info(f"工具调用: {name}({kwargs})")
        try:
            result = self._dispatch(name, **kwargs)
        except PermissionError as e:
            result = {"success": False, "error": str(e)}
        except Exception as e:
            logger.error(f"工具执行失败: {name}: {e}")
            result = {"success": False, "error": str(e), "suggestion": "请检查输入参数后重试"}
        return self._cap_output(result)

    def _dispatch(self, name: str, **kwargs) -> dict:
        if name == "read":
            return self._read(kwargs["path"], kwargs.get("offset"), kwargs.get("limit"))
        if name == "write":
            return self._write(kwargs["path"], kwargs["content"], kwargs.get("mode", "write"))
        if name == "edit":
            return self._edit(
                kwargs["path"], kwargs["old_string"], kwargs["new_string"],
                kwargs.get("replace_all", False),
            )
        if name == "list":
            return self._list(kwargs.get("path", "."), kwargs.get("depth", 1))
        if name == "search":
            return self._search(kwargs["path"], kwargs["pattern"])
        if name == "grep":
            return self._grep(kwargs["path"], kwargs["pattern"], kwargs.get("include"))
        if name == "delete":
            return self._delete(kwargs["path"])
        if name == "remember":
            return self._remember(kwargs["content"])
        if name == "recall":
            return self._recall(kwargs["query"], kwargs.get("top_k", 5))
        if name == "todo_write":
            return self._todo_write(kwargs.get("todos"))
        if name == "todo_read":
            return self._todo_read()
        if name == "bash":
            return self._bash(kwargs["command"], kwargs.get("cwd"), kwargs.get("timeout"))
        if name == "web_fetch":
            return self._web_fetch(kwargs["url"])
        if name == "web_search":
            return self._web_search(kwargs["query"], kwargs.get("top", 5))
        return {"success": False, "error": f"未知工具: {name}"}


def _html_to_text(html_text: str) -> str:
    text = re.sub(r"(?is)<(script|style)[^>]*>.*?</\1>", " ", html_text)
    text = re.sub(r"(?s)<[^>]+>", " ", text)
    text = html_mod.unescape(text)
    return re.sub(r"[ \t\r\f\v]+", " ", text).strip()


def _ddg_unwrap(href: str) -> str:
    if "uddg=" in href:
        qs = urllib.parse.urlparse(href).query
        target = urllib.parse.parse_qs(qs).get("uddg")
        if target:
            return urllib.parse.unquote(target[0])
    if href.startswith("//"):
        return "https:" + href
    return href


def _parse_search_json(body: str):
    """解析 SearXNG/通用 JSON 搜索响应；非 JSON 返回 None。"""
    stripped = body.lstrip()
    if not stripped.startswith(("{", "[")):
        return None
    try:
        data = json.loads(body)
    except json.JSONDecodeError:
        return None
    if not isinstance(data, dict):
        return None
    raw = data.get("results") or data.get("items") or []
    results = []
    for item in raw:
        if not isinstance(item, dict):
            continue
        url = item.get("url") or item.get("link")
        title = item.get("title") or ""
        if not url:
            continue
        results.append({
            "title": str(title).strip(),
            "url": str(url),
            "snippet": str(item.get("content") or item.get("snippet") or "").strip(),
        })
    return results


def _looks_like_challenge(body: str) -> bool:
    lowered = body.lower()
    return any(k in lowered for k in (
        "complete the following challenge",
        "bots use duckduckgo",
        "captcha",
        "are you a robot",
    ))


def _parse_search_results(html_text: str) -> list:
    results = []
    snippets = re.findall(r'class="[^"]*result__snippet[^"]*"[^>]*>(.*?)</a>', html_text, re.S | re.I)
    for i, m in enumerate(re.finditer(
        r'<a[^>]+class="[^"]*result__a[^"]*"[^>]*href="([^"]+)"[^>]*>(.*?)</a>',
        html_text, re.S | re.I,
    )):
        title = html_mod.unescape(re.sub(r"<[^>]+>", "", m.group(2))).strip()
        item = {"title": title, "url": _ddg_unwrap(html_mod.unescape(m.group(1)))}
        if i < len(snippets):
            item["snippet"] = html_mod.unescape(re.sub(r"<[^>]+>", "", snippets[i])).strip()
        results.append(item)
    return results
