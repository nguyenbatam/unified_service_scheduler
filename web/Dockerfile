# syntax=docker/dockerfile:1
# Node version: keep in step with web/.nvmrc (CI reads it via setup-node node-version-file).
FROM node:26.12.0-alpine AS build
WORKDIR /app
# The lockfile is required: npm ci installs exactly it and fails if it is out of date.
COPY package.json package-lock.json ./
RUN --mount=type=cache,target=/root/.npm \
    npm ci --no-audit --no-fund
COPY . .
# "true" ships the one-click demo sign-in (seeded accounts and passwords) in the bundle; the compose
# stack passes it (VITE_DEMO in .env, default true there), a plain image build leaves it out.
ARG VITE_DEMO=false
ENV VITE_DEMO=$VITE_DEMO
RUN npm run build
# Precompress once at the highest level (nginx gzip_static serves these; no per-request CPU).
RUN find dist -type f \( -name '*.js' -o -name '*.css' -o -name '*.html' -o -name '*.svg' \) -size +1k \
    -exec gzip -9 -k {} +

FROM nginx:1.30.6-alpine
COPY nginx.conf /etc/nginx/conf.d/default.conf
COPY --from=build /app/dist /usr/share/nginx/html
EXPOSE 80
