# The dashboard.
#
# Next.js standalone output: the final image carries the server bundle and the
# node runtime, not the toolchain or node_modules.

FROM node:22-alpine AS deps
WORKDIR /app
RUN corepack enable
COPY apps/dashboard/package.json apps/dashboard/pnpm-lock.yaml* ./
RUN pnpm install --prod=false --no-frozen-lockfile

FROM node:22-alpine AS build
WORKDIR /app
RUN corepack enable
COPY --from=deps /app/node_modules ./node_modules
COPY apps/dashboard ./
COPY packages/shared ../../packages/shared
# Same-origin: the API is proxied by whatever terminates TLS, so the client
# needs no absolute base URL. See the comment in next.config.ts.
ENV NEXT_PUBLIC_API_BASE_URL=""
# Empty for the live dashboard; "/v2" (or similar) for a build served beside it.
ARG EP_BASE_PATH=""
ENV EP_BASE_PATH=${EP_BASE_PATH}
RUN pnpm build

FROM node:22-alpine AS runtime
WORKDIR /app
ENV NODE_ENV=production
# Non-root. Nothing here writes anything.
RUN addgroup -g 65532 app && adduser -u 65532 -G app -S app
COPY --from=build --chown=65532:65532 /app/.next/standalone ./
COPY --from=build --chown=65532:65532 /app/.next/static ./.next/static
COPY --from=build --chown=65532:65532 /app/public ./public
USER 65532:65532
EXPOSE 3000
CMD ["node", "server.js"]
