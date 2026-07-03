import json
import logging
from datetime import datetime
from pathlib import Path
from typing import Optional

logger = logging.getLogger("chyan-agent")

try:
    from rank_bm25 import BM25Okapi
    HAS_BM25 = True
except ImportError:
    HAS_BM25 = False
    logger.warning("rank_bm25 未安装，recall 将使用简单关键词匹配")

try:
    import jieba
    HAS_JIEBA = True
except ImportError:
    HAS_JIEBA = False
    logger.warning("jieba 未安装，中文分词降级为逐字拆分")


class MemoryManager:
    def __init__(self, data_dir: str, recall_mode: str = "bm25", recall_top_k: int = 5):
        self.data_dir = Path(data_dir)
        self.recall_mode = recall_mode
        self.recall_top_k = recall_top_k
        self.memories = []
        self.bm25_index = None

        self.data_dir.mkdir(parents=True, exist_ok=True)
        self.conversation_file = self.data_dir / "conversation.jsonl"
        self.memories_file = self.data_dir / "memories.json"
        self.meta_file = self.data_dir / "meta.json"

        self._load_memories()
        self._rebuild_bm25_index()

    def _load_memories(self):
        if self.memories_file.exists():
            try:
                with open(self.memories_file, encoding="utf-8") as f:
                    self.memories = json.load(f)
                logger.info(f"加载 {len(self.memories)} 条记忆")
            except (json.JSONDecodeError, IOError) as e:
                logger.error(f"记忆文件损坏，重置: {e}")
                self.memories = []
        else:
            self.memories = []

    def _save_memories(self):
        with open(self.memories_file, "w", encoding="utf-8") as f:
            json.dump(self.memories, f, ensure_ascii=False, indent=2)

    def _tokenize(self, text: str) -> list[str]:
        if HAS_JIEBA:
            return list(jieba.cut(text))
        return list(text)

    def _rebuild_bm25_index(self):
        if not self.memories or not HAS_BM25:
            self.bm25_index = None
            return

        docs = [m["content"] for m in self.memories]
        tokenized = [self._tokenize(doc) for doc in docs]
        self.bm25_index = BM25Okapi(tokenized)

    def _simple_recall(self, query: str, top_k: int) -> list[dict]:
        query_words = set(self._tokenize(query.lower()))
        scored = []
        for m in self.memories:
            content_words = set(self._tokenize(m["content"].lower()))
            score = len(query_words & content_words)
            if score > 0:
                scored.append({**m, "relevance_score": score})
        scored.sort(key=lambda x: x["relevance_score"], reverse=True)
        return scored[:top_k]

    def save_turn(self, turn: int, role: str, content: str, tool_calls: Optional[list] = None):
        entry = {
            "turn": turn,
            "role": role,
            "content": content,
            "timestamp": datetime.now().isoformat(),
        }
        if tool_calls:
            entry["tool_calls"] = tool_calls
        with open(self.conversation_file, "a", encoding="utf-8") as f:
            f.write(json.dumps(entry, ensure_ascii=False) + "\n")

    def load_conversation(self) -> list[dict]:
        if not self.conversation_file.exists():
            return []
        conversations = []
        with open(self.conversation_file, encoding="utf-8") as f:
            for line in f:
                if line.strip():
                    conversations.append(json.loads(line))
        return conversations

    def save_meta(self, meta: dict):
        with open(self.meta_file, "w", encoding="utf-8") as f:
            json.dump(meta, f, ensure_ascii=False, indent=2)

    def load_meta(self) -> Optional[dict]:
        if not self.meta_file.exists():
            return None
        try:
            with open(self.meta_file, encoding="utf-8") as f:
                return json.load(f)
        except (json.JSONDecodeError, IOError):
            return None

    def rebuild_context(self) -> tuple[str, list, int]:
        conversations = self.load_conversation()
        recent = []
        total_turns = 0

        for entry in conversations:
            role = entry["role"]
            content = entry["content"]
            if role in ("user", "assistant"):
                recent.append({"role": role, "content": content})
                if role == "assistant":
                    total_turns += 1

        return "", recent, total_turns

    def recall(self, query: str, top_k: Optional[int] = None) -> list[dict]:
        k = top_k or self.recall_top_k

        if self.recall_mode == "bm25" and HAS_BM25:
            return self._bm25_recall(query, k)
        return self._simple_recall(query, k)

    def _bm25_recall(self, query: str, top_k: int) -> list[dict]:
        if not self.bm25_index:
            return []
        scores = self.bm25_index.get_scores(self._tokenize(query))
        top_indices = sorted(
            range(len(scores)),
            key=lambda i: scores[i],
            reverse=True,
        )[:top_k]

        results = []
        for idx in top_indices:
            if scores[idx] > 0:
                results.append({
                    **self.memories[idx],
                    "relevance_score": round(scores[idx], 3),
                })
        return results

    def remember(self, content: str, source_turn: int) -> str:
        memory_id = f"m{len(self.memories) + 1}"
        new_memory = {
            "id": memory_id,
            "content": content,
            "source_turn": source_turn,
            "created_at": datetime.now().isoformat(),
        }
        self.memories.append(new_memory)
        self._save_memories()
        self._rebuild_bm25_index()
        return memory_id

    def get_all_memories(self) -> list[dict]:
        return self.memories.copy()

    def reset(self):
        self.memories = []
        self.bm25_index = None
        self._save_memories()
        if self.conversation_file.exists():
            self.conversation_file.unlink()
        if self.meta_file.exists():
            self.meta_file.unlink()
        logger.info("已清空所有对话历史和记忆")
