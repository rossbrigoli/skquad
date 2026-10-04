FROM python:3.11-slim

ENV PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1 \
    PIP_NO_CACHE_DIR=1 \
    PRISMA_HOME_DIR=/home/skquad/.cache/prisma-python

WORKDIR /app
COPY pyproject.toml ./
COPY config.yaml /etc/skquad/config.yaml
COPY skquad_litellm_callbacks.py /app/skquad_litellm_callbacks.py
COPY vision_gate.py /app/vision_gate.py
# S7031: single RUN layer — apt deps and the pip/prisma build are merged so
# the image has one transient layer instead of two.
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates libatomic1 \
    && rm -rf /var/lib/apt/lists/* \
    && python -m pip install --upgrade pip \
    && python -m pip install . \
    && useradd --create-home --uid 65532 --shell /usr/sbin/nologin skquad \
    && prisma generate --schema /usr/local/lib/python3.11/site-packages/litellm/proxy/schema.prisma \
    && HOME=/home/skquad python -m prisma py fetch \
    && chown -R 65532:65532 /home/skquad

USER 65532:65532
EXPOSE 4000
ENTRYPOINT ["litellm"]
CMD ["--config", "/etc/skquad/config.yaml", "--host", "0.0.0.0", "--port", "4000"]
