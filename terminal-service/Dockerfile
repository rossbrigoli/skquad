# TG-10: terminal-service (Terminal-as-a-Service). Build from THIS directory's
# context (matches images.yml `context: terminal-service`); the module is
# self-contained (no replace of ../shared):
#   docker build -t skquad-terminal-service terminal-service
ARG GO_VERSION=1.26.9
# TG-11: ansible pins for the artifact apply engine. ansible-core 2.21.5
# (latest stable, released 2026-10-05; requires Python >= 3.12) plus ONLY
# the collection the lint gate's package modules need beyond builtins:
# zypper -> community.general (13.5.0, latest stable). apt/yum/dnf/package
# are ansible-core builtins — no extra collection.
ARG ANSIBLE_CORE_VERSION=2.21.5
ARG COMMUNITY_GENERAL_VERSION=13.5.0

FROM golang:${GO_VERSION}-alpine AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/skquad-terminal-service ./cmd/terminal

# Ansible layer: pinned venv + pinned collections, built in a throwaway
# stage so the runtime only ever sees the final artifacts.
FROM python:3.12-slim-bookworm AS ansible
ARG ANSIBLE_CORE_VERSION
ARG COMMUNITY_GENERAL_VERSION
RUN python -m venv --copies /opt/ansible-venv \
 && /opt/ansible-venv/bin/pip install --no-cache-dir "ansible-core==${ANSIBLE_CORE_VERSION}" \
 && /opt/ansible-venv/bin/ansible-galaxy collection install \
      "community.general==${COMMUNITY_GENERAL_VERSION}" -p /opt/ansible/collections \
 && /opt/ansible-venv/bin/ansible-playbook --version | head -2

# Runtime. NOTE (TG-11, 2026-10-09): the apply engine (internal/apply)
# and drift-check (internal/drift) shell out to `ansible-playbook`, so
# the TG-10 distroless/static base cannot host this image anymore —
# ansible-core 2.21 needs Python >= 3.12. python:3.12-slim is the
# minimal base that satisfies it. Hardening is preserved at the pod
# securityContext (chart): read-only rootfs + nonroot. The service
# binary itself remains a static Go build.
FROM python:3.12-slim-bookworm
COPY --from=build /out/skquad-terminal-service /usr/local/bin/skquad-terminal-service
COPY --from=ansible /opt/ansible-venv /opt/ansible-venv
COPY --from=ansible /opt/ansible/collections /opt/ansible/collections
# ansible-playbook must be on PATH (apply engine default AnsibleBin).
# ANSIBLE_HOME/ANSIBLE_LOCAL_TEMP point at /tmp so the nonroot user can
# write on a read-only rootfs (chart mounts an emptyDir at /tmp).
ENV PATH="/opt/ansible-venv/bin:${PATH}" \
    ANSIBLE_COLLECTIONS_PATH=/opt/ansible/collections \
    ANSIBLE_HOME=/tmp/.ansible \
    ANSIBLE_LOCAL_TEMP=/tmp/.ansible/tmp
EXPOSE 8091
USER 65532:65532
ENTRYPOINT ["skquad-terminal-service"]
