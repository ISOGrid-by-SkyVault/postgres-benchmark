# Production image: the app is built to static files and served by nginx,
# running as a non-root user on port 8080.

# Stage 1: build
FROM node:26-alpine AS build

WORKDIR /app

# Install dependencies first so this layer is cached until the lockfile changes
COPY package.json package-lock.json ./
RUN npm ci

COPY . .
RUN npm run build

# Stage 2: serve
FROM nginxinc/nginx-unprivileged:1.29-alpine

# Where /api is proxied to. Override it when the backend is not reachable as
# "backend" on the internal network.
ENV BACKEND_UPSTREAM=http://backend:8080

# The image entrypoint runs envsubst on this template at startup and writes
# the result to /etc/nginx/conf.d/default.conf
COPY nginx.conf.template /etc/nginx/templates/default.conf.template

COPY --from=build /app/dist /usr/share/nginx/html

EXPOSE 8080

HEALTHCHECK --interval=10s --timeout=5s --retries=3 CMD ["wget", "-qO-", "http://127.0.0.1:8080/healthz"]
