# syntax=docker/dockerfile:1
# TG-6 browser-service (docs/tg6-browser-protocol.md).
#
# Base: official Playwright image (Chromium + deps preinstalled). The tag MUST
# match the playwright-core version in package-lock.json (currently 1.63.0) —
# a mismatch means the bundled browser revision doesn't match what
# playwright-core expects and launch fails.
#
# TODO(TG-6 follow-up): pin the base by digest (@sha256:...) for reproducible
# builds; tracked in WORKLOG.md. Tag pin only for now.
ARG BASE_IMAGE=mcr.microsoft.com/playwright:v1.63.0-noble

# ---------- build: compile TypeScript (needs dev deps) ----------
FROM ${BASE_IMAGE} AS build
WORKDIR /app
COPY package.json package-lock.json ./
RUN npm ci
COPY tsconfig.json ./
COPY src ./src
COPY test ./test
RUN npm run build

# ---------- runtime: production deps only ----------
FROM ${BASE_IMAGE}
ENV NODE_ENV=production \
    PLAYWRIGHT_BROWSERS_PATH=/ms-playwright \
    PORT=8090 \
    # playwright-core treats executablePath as a literal path (NOT a PATH
    # lookup), so the app default 'chromium' only works when an absolute
    # path is supplied — point it at the bundled Chromium symlink below.
    BROWSER_EXECUTABLE=/usr/local/bin/chromium
WORKDIR /app

COPY package.json package-lock.json ./
RUN npm ci --omit=dev && npm cache clean --force

COPY --from=build /app/dist ./dist

# The code default is BROWSER_EXECUTABLE=chromium (PATH lookup); expose the
# bundled Chromium that way.
RUN ln -sf "$(ls -d /ms-playwright/chromium-*/chrome-linux64 | head -n 1)/chrome" /usr/local/bin/chromium \
    && test -x /usr/local/bin/chromium

# Non-root runtime user (base image ships pwuser; force uid/gid 1000 so the
# chart's runAsUser: 1000 is deterministic).
RUN usermod -u 1000 pwuser && groupmod -g 1000 pwuser || true \
    && chown -R 1000:1000 /app
USER pwuser

EXPOSE 8090
ENTRYPOINT ["node", "dist/src/server.js"]
