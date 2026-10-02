# S-212: skquad-embedder — llama.cpp llama-server serving Qwen3-Embedding-0.6B
# (OpenAI-compatible /v1/embeddings) for skquad agent memory RAG.
#
# The GGUF model is baked into the image so pods start with no external
# downloads. Base is the VULKAN build (S-212 scope change: vendor-neutral
# GPU — works with NVIDIA/AMD/Intel via Mesa userspace drivers; no CUDA
# dependency). The same Dockerfile builds a CPU-only variant via
# --build-arg BASE_IMAGE=...:server-b11347 for smoke tests.
#
# Vulkan device access in-cluster: mount host /dev/dri (and /dev/kfd for
# AMD) via embedder.gpu.devicePaths; the operator binds the pod to nodes
# exposing a supported GPU resource.
#
# Qwen3 embeddings require last-token pooling (--pooling last).
# --embd-normalize 2 (llama.cpp default) L2-normalizes vectors, which is
# exactly what pgvector cosine distance expects.

ARG BASE_IMAGE=ghcr.io/ggml-org/llama.cpp:server-vulkan-b11347
FROM ${BASE_IMAGE}

ARG MODEL_URL=https://huggingface.co/Qwen/Qwen3-Embedding-0.6B-GGUF/resolve/main/Qwen3-Embedding-0.6B-Q8_0.gguf
ARG MODEL_FILE=/models/qwen3-embed-0.6b-q8_0.gguf

USER root
RUN mkdir -p /models \
 && curl -fsSL --retry 3 --retry-delay 5 -o "${MODEL_FILE}" "${MODEL_URL}" \
 && ls -l "${MODEL_FILE}"
# llama.cpp images run as uid 1000 ("app"); keep that for the server process.
RUN chown -R 1000:1000 /models
USER 1000

EXPOSE 8080

ENTRYPOINT ["/app/llama-server"]
CMD ["--model", "/models/qwen3-embed-0.6b-q8_0.gguf", \
     "--embeddings", \
     "--pooling", "last", \
     "--host", "0.0.0.0", \
     "--port", "8080", \
     "--alias", "qwen3-embed-0.6b"]
