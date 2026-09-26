FROM node:20-alpine AS build
WORKDIR /app

# S-141: release identity (e.g. 0.1.100 + commit SHA) passed in by the Images
# workflow. next.config.ts re-exports these as NEXT_PUBLIC_* so they are inlined
# into the bundle at build time.
ARG SKQUAD_VERSION=""
ARG SKQUAD_COMMIT=""

ENV NEXT_TELEMETRY_DISABLED=1 \
    SKQUAD_VERSION=${SKQUAD_VERSION} \
    SKQUAD_COMMIT=${SKQUAD_COMMIT}

COPY package.json package-lock.json ./
RUN npm ci

COPY . .
RUN npm run build

FROM node:20-alpine
WORKDIR /app

ENV NODE_ENV=production \
    NEXT_TELEMETRY_DISABLED=1 \
    PORT=3000

COPY package.json package-lock.json ./
RUN npm ci --omit=dev && npm cache clean --force

COPY --from=build /app/.next ./.next
COPY --from=build /app/src ./src
# S-123: public/ holds the brand logo + favicon assets served by next start;
# without this COPY they 404 in the runtime image.
COPY --from=build /app/public ./public
RUN addgroup -S skquad -g 65532 \
    && adduser -S skquad -u 65532 -G skquad

USER 65532:65532
EXPOSE 3000
CMD ["npm", "run", "start"]
