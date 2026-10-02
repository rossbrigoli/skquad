-- 0034: S-212 memory RAG — canonical embedding dimension 1024.
--
-- The platform embedder is Qwen3-Embedding-0.6B (native dim 1024); the
-- original vector(1536) column was sized for an OpenAI text-embedding
-- shape that was never wired. All existing rows carry NULL embeddings
-- (verified pre-migration: 9 rows, 0 embeddings), so the dimension
-- change discards no data. The USING NULL cast makes that explicit: any
-- hypothetical non-NULL vector is dropped rather than silently resized.
--
-- The ivfflat cosine index is dropped and recreated against the new
-- column type (pgvector indexes are dimension-bound).

DROP INDEX IF EXISTS idx_memory_embedding;

ALTER TABLE agent_memory
    ALTER COLUMN embedding TYPE vector(1024) USING NULL;

CREATE INDEX IF NOT EXISTS idx_memory_embedding
    ON agent_memory
    USING ivfflat (embedding vector_cosine_ops);
