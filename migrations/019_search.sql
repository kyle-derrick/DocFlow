-- 019: 文件全文搜索（v2 规划 Meilisearch，本批次先落地 PostgreSQL 原生方案）。
-- file_search_docs：文件名 + 文本内容的检索文档表，每文件一行（file_id 主键）。
-- 构建时机：文件版本创建（上传完成：新建 / 覆盖新版本）后经 task:search-index
-- 队列异步重建（Indexer 见 internal/search）；文件彻底删除（files 行硬删）时
-- 经外键 ON DELETE CASCADE 级联移除，janitor 另有孤儿兜底清理。
-- tsv 生成列：to_tsvector('simple', name || ' ' || content)——选 'simple' 而非
-- 语言配置（如 english）：中文无空格分词，语言配置的停用词/词干化对中文无效且
-- 会错误切分 CJK 文本；'simple' 按空白切分，中文整句成为单 token（内容检索对
-- 中文基本无效，中文场景以名称 ILIKE 兜底为主，内容 tsvector 对英文有效）。
-- 内容索引上限 2MB（应用层限流读取），超出/二进制（图片/pdf/office）仅索引名称
--（content 为 NULL，tsv 退化为名称向量）。
CREATE TABLE IF NOT EXISTS file_search_docs (
    file_id UUID PRIMARY KEY REFERENCES files(id) ON DELETE CASCADE,
    version_id UUID,
    owner_id UUID NOT NULL,
    team_id UUID,
    name TEXT NOT NULL,
    content TEXT,
    tsv tsvector GENERATED ALWAYS AS (to_tsvector('simple', coalesce(name, '') || ' ' || coalesce(content, ''))) STORED,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- 全文检索（tsv @@ plainto_tsquery('simple', q)）走 GIN。
CREATE INDEX IF NOT EXISTS idx_file_search_docs_tsv ON file_search_docs USING GIN (tsv);
-- 访问控制过滤（个人 owner 命中）。
CREATE INDEX IF NOT EXISTS idx_file_search_docs_owner ON file_search_docs (owner_id);
-- 访问控制过滤（团队成员 EXISTS 判定）。
CREATE INDEX IF NOT EXISTS idx_file_search_docs_team ON file_search_docs (team_id);
