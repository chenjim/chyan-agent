import json
import logging
import re
from pathlib import Path

logger = logging.getLogger("chyan-agent")

SENSITIVE_FILES = {"config.toml", "config.example.toml", ".env", ".env.local"}


class ToolExecutor:
    def __init__(self, memory_manager, tool_config=None):
        self.memory_manager = memory_manager
        self.tool_config = tool_config or {}
        self.current_turn = 0

    def update_config(self, config):
        self.tool_config = {
            "read_max_lines": config.get("read_max_lines", 500),
            "read_truncate_mode": config.get("read_truncate_mode", "auto"),
        }

    def get_schemas(self):
        _tools = [
            {
                "name": "read",
                "description": "读取指定文件的内容。支持智能截断，防止大文件占满上下文。",
                "parameters": {
                    "type": "object",
                    "properties": {
                        "path": {"type": "string", "description": "文件路径"},
                    },
                    "required": ["path"],
                },
            },
            {
                "name": "write",
                "description": "写入或追加内容到文件。禁止覆盖 config.toml、.env 等敏感文件。",
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
        ]
        return [
            {"type": "function", "function": {"name": t["name"], "description": t["description"], "parameters": t["parameters"]}}
            for t in _tools
        ]

    def _read(self, path: str) -> dict:
        p = Path(path).resolve()
        if not p.exists():
            return {
                "success": False,
                "error": f"文件不存在: {path}",
                "suggestion": "使用 search 工具查找文件路径，或用 write 工具创建新文件",
            }
        if not p.is_file():
            return {"success": False, "error": f"路径不是文件: {path}"}

        try:
            lines = p.read_text(encoding="utf-8").splitlines(keepends=True)
        except Exception as e:
            return {"success": False, "error": f"读取文件失败: {e}"}

        total_lines = len(lines)
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
        p = Path(path).resolve()

        if p.name in SENSITIVE_FILES:
            return {
                "success": False,
                "error": f"禁止操作敏感文件: {p.name}",
                "suggestion": "请使用其他文件名",
            }

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

    def _search(self, path: str, pattern: str) -> dict:
        p = Path(path).resolve()
        if not p.exists():
            return {"success": False, "error": f"目录不存在: {path}"}

        matching = list(p.rglob(pattern))
        return {
            "success": True,
            "path": str(p),
            "pattern": pattern,
            "results": [str(f.relative_to(p)) for f in matching],
            "count": len(matching),
        }

    def _grep(self, path: str, pattern: str, include: str = None) -> dict:
        p = Path(path).resolve()
        if not p.exists():
            return {"success": False, "error": f"路径不存在: {path}"}

        matching_files = []
        if p.is_file():
            files_to_check = [p]
        else:
            files_to_check = list(p.rglob(include or "*"))

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
            except Exception:
                continue

        return {
            "success": True,
            "path": str(p),
            "pattern": pattern,
            "results": matching_files,
            "count": len(matching_files),
        }

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

    def execute(self, name: str, **kwargs) -> dict:
        logger.info(f"工具调用: {name}({kwargs})")
        try:
            if name == "read":
                return self._read(kwargs["path"])
            elif name == "write":
                return self._write(kwargs["path"], kwargs["content"], kwargs.get("mode", "write"))
            elif name == "search":
                return self._search(kwargs["path"], kwargs["pattern"])
            elif name == "grep":
                return self._grep(kwargs["path"], kwargs["pattern"], kwargs.get("include"))
            elif name == "remember":
                return self._remember(kwargs["content"])
            elif name == "recall":
                return self._recall(kwargs["query"], kwargs.get("top_k", 5))
            else:
                return {"success": False, "error": f"未知工具: {name}"}
        except Exception as e:
            logger.error(f"工具执行失败: {name}: {e}")
            return {"success": False, "error": str(e), "suggestion": "请检查输入参数后重试"}
